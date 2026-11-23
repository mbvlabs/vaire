package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestInstallVersionPolicy(t *testing.T) {
	for _, tc := range []struct {
		version, channel string
		ok               bool
	}{
		{"1.0.0-rc.1", "rc", false}, {"1.0.0-rc.2", "rc", true},
		{"1.1.0-rc.1", "rc", true}, {"1.0.0", "stable", true},
		{"0.9.9", "stable", false}, {"1.0.0-beta.2", "edge", false},
		{"edge-123456789abc", "edge", false}, {"dev", "dev", false},
		{"1.0.0", "rc", false}, {"1.0.0-rc.2", "stable", false},
	} {
		if got := allowedInstallVersion(tc.version, tc.channel); got != tc.ok {
			t.Errorf("%s/%s = %v", tc.version, tc.channel, got)
		}
	}
}

func TestInstallNoopAndDowngrade(t *testing.T) {
	oldV, oldC, oldCh := buildinfo.Version, buildinfo.Commit, buildinfo.Channel
	oldCurrent := installCurrentBinary

	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit, buildinfo.Channel = oldV, oldC, oldCh })
	t.Cleanup(func() { installCurrentBinary = oldCurrent })

	buildinfo.Version, buildinfo.Commit, buildinfo.Channel = "1.0.0-rc.3", "abc", "rc"
	installed := BuildID{"1.0.0-rc.3", "abc", "rc"}
	installCurrentBinary = func(context.Context, string) (BuildID, error) { return installed, nil }
	env := &Env{TrustedHostAdmin: true, Req: rpc.Request{Params: json.RawMessage(`{"version":"1.0.0-rc.3","channel":"rc"}`)}}
	res, err := handleUpgradeInstall(context.Background(), env)

	data, ok := res.Data.(InstallResult)
	if err != nil || !ok || data.Changed {
		t.Fatalf("noop: %#v %v", res, err)
	}

	env.Req.Params = json.RawMessage(`{"version":"1.0.0-rc.2","channel":"rc"}`)
	if _, err := handleUpgradeInstall(context.Background(), env); err == nil {
		t.Fatal("downgrade accepted")
	}

	env.Req.Params = json.RawMessage(`{"version":"1.0.0","channel":"stable","path":"/tmp/evil"}`)
	if _, err := handleUpgradeInstall(context.Background(), env); err == nil {
		t.Fatal("path accepted")
	}

	buildinfo.Version, buildinfo.Channel = "1.0.0", "stable"
	installed = BuildID{"1.0.0", "abc", "stable"}

	env.Req.Params = json.RawMessage(`{"version":"1.1.0-rc.2","channel":"rc"}`)
	if _, err := handleUpgradeInstall(context.Background(), env); err == nil {
		t.Fatal("stable to prerelease accepted")
	}

	buildinfo.Version, buildinfo.Channel = "1.0.0-rc.3", "rc"

	env.Req.Params = json.RawMessage(`{"version":"1.0.0-rc.3","channel":"rc"}`)
	if _, err := handleUpgradeInstall(context.Background(), env); err == nil {
		t.Fatal("stale running process reported a downgrade as a no-op")
	}

	buildinfo.Version = "1.0.0-rc.1"

	env.Req.Params = json.RawMessage(`{"version":"1.0.0","channel":"stable"}`)
	if _, err := handleUpgradeInstall(context.Background(), env); err == nil {
		t.Fatal("old running process accepted")
	}
}

func TestInstallReleaseSwapAndRollback(t *testing.T) { //nolint:gocognit // Keep the swap, verification, and rollback cases together.
	oldFetch, oldVersion, oldCheck := installFetch, installVersion, installSelfcheck

	t.Cleanup(func() { installFetch, installVersion, installSelfcheck = oldFetch, oldVersion, oldCheck })

	current := BuildID{"1.0.0-rc.2", "old", "rc"}
	candidate := BuildID{"1.0.0-rc.3", "new", "rc"}
	p := installParams{candidate.Version, candidate.Channel}

	for _, tc := range []struct {
		name                     string
		check, mismatch, badHash bool
	}{
		{"success", true, false, false}, {"rollback", false, false, false},
		{"wrong build", true, true, false}, {"wrong checksum", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			binary := filepath.Join(dir, "vaire")
			if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}

			installFetch = func(_ context.Context, got installParams, cache string) (string, string, error) {
				if got != p || filepath.Dir(cache) != dir {
					t.Fatalf("fetch: %#v %s", got, cache)
				}

				src := filepath.Join(cache, "download")
				if err := os.WriteFile(src, []byte("new"), 0o700); err != nil {
					t.Fatal(err)
				}

				sum := sha256.Sum256([]byte("new"))

				if tc.badHash {
					return src, strings.Repeat("0", 64), nil
				}

				return src, hex.EncodeToString(sum[:]), nil
			}
			installVersion = func(_ context.Context, _ string) (BuildID, error) {
				if tc.mismatch {
					return current, nil
				}

				return candidate, nil
			}
			installSelfcheck = func(_ context.Context, path string) (any, bool) {
				b, err := os.ReadFile(path)
				if err != nil || string(b) != "new" {
					t.Fatalf("selfcheck sees %q: %v", b, err)
				}

				return "checks", tc.check
			}
			res, err := installReleaseAt(context.Background(), p, current, binary)

			if tc.name == "success" {
				data, ok := res.Data.(InstallResult)
				if err != nil || !ok || !data.Changed {
					t.Fatalf("result %#v: %v", res, err)
				}

				prev, err := os.ReadFile(binary + ".prev")
				if err != nil || string(prev) != "old" {
					t.Fatalf("previous %q: %v", prev, err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}

			b, err := os.ReadFile(binary)

			want := "old"
			if tc.name == "success" {
				want = "new"
			}

			if err != nil || string(b) != want {
				t.Fatalf("installed %q, want %q: %v", b, want, err)
			}
		})
	}
}

func TestInstallCancellationRollsBack(t *testing.T) {
	oldFetch, oldVersion, oldCheck := installFetch, installVersion, installSelfcheck

	t.Cleanup(func() { installFetch, installVersion, installSelfcheck = oldFetch, oldVersion, oldCheck })
	dir := t.TempDir()

	binary := filepath.Join(dir, "vaire")
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	installFetch = func(_ context.Context, _ installParams, cache string) (string, string, error) {
		src := filepath.Join(cache, "download")
		if err := os.WriteFile(src, []byte("new"), 0o700); err != nil {
			t.Fatal(err)
		}

		sum := sha256.Sum256([]byte("new"))

		return src, hex.EncodeToString(sum[:]), nil
	}
	installVersion = func(context.Context, string) (BuildID, error) { return BuildID{"1.0.0", "new", "stable"}, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	installSelfcheck = func(context.Context, string) (any, bool) { cancel(); return nil, false }

	_, err := installReleaseAt(ctx, installParams{"1.0.0", "stable"}, BuildID{"1.0.0-rc.2", "old", "rc"}, binary)
	if err == nil {
		t.Fatal("expected failure")
	}

	b, err := os.ReadFile(binary)
	if err != nil || string(b) != "old" {
		t.Fatalf("rollback: %q %v", b, err)
	}
}
