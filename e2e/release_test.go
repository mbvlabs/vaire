//go:build e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/e2e/harness"
)

const (
	edgeV1 = "edge-000000000001"
	edgeV2 = "edge-000000000002"
	vmPub  = "/etc/eru-e2e/cosign.pub"
)

func TestE25Release(t *testing.T) {
	svc := harness.StartServices(t)
	if svc.PebbleDirectory == "" {
		t.Fatal("pebble was not started")
	}
	baseURL := harness.ReleaseServer(t)
	keyDir := t.TempDir()
	harness.BuildRelease(t, edgeV1, baseURL, keyDir)

	vm := harness.NewVM(t, harness.Image())
	prepareReleaseVM(t, vm, keyDir)
	env := releaseEnv(baseURL)

	t.Run("1 root install", func(t *testing.T) {
		out, code := vm.ExecCode(append(env, "bash", "-lc", `curl -fsSL "$ERU_TEST_RELEASE_BASE_URL/edge/install.sh" | bash`)...)
		if code != 0 {
			t.Fatalf("root install exit %d\n%s", code, out)
		}
		if !strings.Contains(out, "/usr/local/bin") {
			t.Fatalf("root install path:\n%s", out)
		}
		got := versionOf(t, vm, "/usr/local/bin/eru")
		if got != edgeV1 {
			t.Fatalf("root version %q", got)
		}
	})

	t.Run("2 non-root install", func(t *testing.T) {
		out, code := vm.ExecCode(append([]string{"runuser", "-u", "eruop", "--"},
			append(env, "bash", "-lc", `curl -fsSL "$ERU_TEST_RELEASE_BASE_URL/edge/install.sh" | bash`)...)...)
		if code != 0 {
			t.Fatalf("user install exit %d\n%s", code, out)
		}
		if !strings.Contains(out, "/home/eruop/.local/bin") {
			t.Fatalf("user install path:\n%s", out)
		}
		if !strings.Contains(out, "is not on PATH") || !strings.Contains(out, "export PATH=") {
			t.Fatalf("missing PATH hint:\n%s", out)
		}
		got := versionOf(t, vm, "/home/eruop/.local/bin/eru")
		if got != edgeV1 {
			t.Fatalf("user version %q", got)
		}
	})

	harness.BuildRelease(t, edgeV2, baseURL, keyDir)
	linuxArtifact := filepath.Join(harness.ReleaseDir(baseURL), "releases", edgeV2, "linux", vmArch(t, vm), "eru")
	channelManifest := filepath.Join(harness.ReleaseDir(baseURL), "edge", "manifest.json")

	t.Run("7 missing version", func(t *testing.T) {
		env := eruJSON(t, vm, nil, "/usr/local/bin/eru", baseURL, "upgrade", "--version", "edge-000000000099")
		if env.Exit != 7 || env.Error == nil || env.Error.Code != "validation_failed" {
			t.Fatalf("absent version: exit %d %s", env.Exit, env.Stdout)
		}
	})

	t.Run("8 dry-run", func(t *testing.T) {
		beforeBin := fileSHA(t, vm, "/usr/local/bin/eru")
		beforeCache, _ := vm.Exec("bash", "-lc", "find /root/.config/eru/cache -type f 2>/dev/null | sort || true")
		env := eruJSON(t, vm, nil, "/usr/local/bin/eru", baseURL, "upgrade", "--dry-run")
		if env.Exit != 0 || !env.OK {
			t.Fatalf("dry-run: %s", env.Stdout)
		}
		data := upgradePayload(t, env)
		if !data.DryRun || !data.Changed || data.To.Version != edgeV2 {
			t.Fatalf("dry-run payload %#v", data)
		}
		afterBin := fileSHA(t, vm, "/usr/local/bin/eru")
		if beforeBin != afterBin {
			t.Fatal("dry-run changed the installed binary")
		}
		afterCache, _ := vm.Exec("bash", "-lc", "find /root/.config/eru/cache -type f 2>/dev/null | sort || true")
		if beforeCache != afterCache {
			t.Fatalf("dry-run wrote cache\nbefore:\n%s\nafter:\n%s", beforeCache, afterCache)
		}
	})

	t.Run("6 tamper", func(t *testing.T) {
		before := fileSHA(t, vm, "/usr/local/bin/eru")
		origArt := flipFileByte(t, linuxArtifact)
		env := eruJSON(t, vm, nil, "/usr/local/bin/eru", baseURL, "upgrade")
		if env.Exit != 14 || env.Error == nil || env.Error.Code != "release_verification_failed" {
			t.Fatalf("tampered artifact: exit %d %s", env.Exit, env.Stdout)
		}
		if fileSHA(t, vm, "/usr/local/bin/eru") != before {
			t.Fatal("tampered artifact changed the installed binary")
		}
		if err := os.WriteFile(linuxArtifact, origArt, 0o755); err != nil { //nolint:gosec // G306: restore the published test binary.
			t.Fatal(err)
		}

		origMan := flipFileByte(t, channelManifest)
		env = eruJSON(t, vm, nil, "/usr/local/bin/eru", baseURL, "upgrade")
		if env.Exit != 14 || env.Error == nil || env.Error.Code != "release_verification_failed" {
			t.Fatalf("tampered manifest: exit %d %s", env.Exit, env.Stdout)
		}
		if fileSHA(t, vm, "/usr/local/bin/eru") != before {
			t.Fatal("tampered manifest changed the installed binary")
		}
		if err := os.WriteFile(channelManifest, origMan, 0o644); err != nil { //nolint:gosec // G306: restore the published test manifest.
			t.Fatal(err)
		}
	})

	t.Run("3 upgrade", func(t *testing.T) {
		env := eruJSON(t, vm, nil, "/usr/local/bin/eru", baseURL, "upgrade")
		if env.Exit != 0 || !env.OK {
			t.Fatalf("upgrade: %s", env.Stdout)
		}
		data := upgradePayload(t, env)
		if !data.Changed || data.To.Version != edgeV2 || data.From.Version != edgeV1 {
			t.Fatalf("upgrade payload %#v", data)
		}
		if data.PreviousPath != "/usr/local/bin/eru.prev" {
			t.Fatalf("previous_path %q", data.PreviousPath)
		}
		if versionOf(t, vm, "/usr/local/bin/eru") != edgeV2 {
			t.Fatal("upgraded binary is not v2")
		}
		if versionOf(t, vm, "/usr/local/bin/eru.prev") != edgeV1 {
			t.Fatal(".prev is not v1")
		}
	})

	t.Run("4 noop", func(t *testing.T) {
		env := eruJSON(t, vm, nil, "/usr/local/bin/eru", baseURL, "upgrade")
		if env.Exit != 0 || !env.OK {
			t.Fatalf("noop: %s", env.Stdout)
		}
		data := upgradePayload(t, env)
		if data.Changed {
			t.Fatalf("expected no-op, got %#v", data)
		}
	})

	t.Run("5 escalate", func(t *testing.T) {
		if _, err := vm.Exec("cp", "/usr/local/bin/eru.prev", "/usr/local/bin/eru"); err != nil {
			t.Fatal(err)
		}
		if versionOf(t, vm, "/usr/local/bin/eru") != edgeV1 {
			t.Fatal("expected restored v1 before escalate")
		}
		asUser := []string{"runuser", "-u", "eruop", "--"}
		denied := eruJSON(t, vm, asUser, "/usr/local/bin/eru", baseURL, "upgrade")
		if denied.Exit != 11 || denied.Error == nil || denied.Error.Code != "precondition_failed" {
			t.Fatalf("without sudo: exit %d %s", denied.Exit, denied.Stdout)
		}
		if detailReason(t, denied) != "root_required" {
			t.Fatalf("without sudo reason %q %s", detailReason(t, denied), denied.Stdout)
		}

		if _, err := vm.Exec("bash", "-lc", `echo 'eruop ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/eruop && chmod 440 /etc/sudoers.d/eruop`); err != nil {
			t.Fatal(err)
		}
		ok := eruJSON(t, vm, asUser, "/usr/local/bin/eru", baseURL, "upgrade", "--sudo")
		if ok.Exit != 0 || !ok.OK {
			t.Fatalf("with sudo: %s", ok.Stdout)
		}
		data := upgradePayload(t, ok)
		if !data.Changed || !data.Escalated || data.To.Version != edgeV2 {
			t.Fatalf("escalated payload %#v\n%s", data, ok.Stdout)
		}
		if versionOf(t, vm, "/usr/local/bin/eru") != edgeV2 {
			t.Fatal("escalated upgrade did not install v2")
		}
	})

	t.Run("9 platforms", func(t *testing.T) {
		env := eruJSON(t, vm, nil, "/usr/local/bin/eru", baseURL, "commands")
		if env.Exit != 0 || !env.OK {
			t.Fatalf("commands: %s", env.Stdout)
		}
		var payload struct {
			Catalog []struct {
				Audience  string   `json:"audience"`
				Platforms []string `json:"platforms"`
				Path      string   `json:"path"`
			} `json:"catalog"`
		}
		if err := json.Unmarshal(env.Data, &payload); err != nil {
			t.Fatal(err)
		}
		var node int
		for _, rec := range payload.Catalog {
			if rec.Audience != "node" {
				continue
			}
			node++
			if len(rec.Platforms) != 1 || rec.Platforms[0] != "linux" {
				t.Fatalf("%s platforms %#v", rec.Path, rec.Platforms)
			}
		}
		if node == 0 {
			t.Fatal("no node-audience commands")
		}
	})
}

func prepareReleaseVM(t *testing.T, vm *harness.VM, keyDir string) {
	t.Helper()
	if _, err := vm.Exec("bash", "-lc", "export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq curl ca-certificates sudo"); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Exec("useradd", "-m", "-s", "/bin/bash", "eruop"); err != nil {
		t.Fatal(err)
	}
	gw, err := vm.Exec("bash", "-lc", "ip -4 route show default | awk '{print $3; exit}'")
	if err != nil {
		t.Fatal(err)
	}
	gw = strings.TrimSpace(gw)
	if gw == "" {
		t.Fatal("vm has no default gateway")
	}
	if _, err := vm.Exec("bash", "-lc", "printf '%s release.eru.test\\n' '"+gw+"' >> /etc/hosts"); err != nil {
		t.Fatal(err)
	}
	rootPEM := harness.PebbleRootPEM(t)
	tmp := filepath.Join(t.TempDir(), "pebble-acme.crt")
	if err := os.WriteFile(tmp, rootPEM, 0o644); err != nil { //nolint:gosec // G306: CA file is installed into the VM trust store.
		t.Fatal(err)
	}
	if err := vm.Push(tmp, "/usr/local/share/ca-certificates/pebble-acme.crt", "0644"); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Exec("update-ca-certificates"); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Exec("mkdir", "-p", "/etc/eru-e2e"); err != nil {
		t.Fatal(err)
	}
	if err := vm.Push(filepath.Join(keyDir, "cosign.pub"), vmPub, "0644"); err != nil {
		t.Fatal(err)
	}
	sudoers := `Defaults env_keep += "ERU_TEST_RELEASE_BASE_URL ERU_TEST_RELEASE_PUBKEY ERU_TEST_RELEASE_PUBKEY_URL"` + "\n"
	tmpSudo := filepath.Join(t.TempDir(), "eru-e2e")
	if err := os.WriteFile(tmpSudo, []byte(sudoers), 0o440); err != nil { //nolint:gosec // G306: sudoers files are 0440.
		t.Fatal(err)
	}
	if err := vm.Push(tmpSudo, "/etc/sudoers.d/eru-e2e", "0440"); err != nil {
		t.Fatal(err)
	}
}

func releaseEnv(baseURL string) []string {
	return []string{
		"env",
		"ERU_TEST_RELEASE_BASE_URL=" + baseURL,
		"ERU_TEST_RELEASE_PUBKEY=" + vmPub,
		"ERU_TEST_RELEASE_PUBKEY_URL=" + baseURL + "/cosign.pub",
	}
}

func eruJSON(t *testing.T, vm *harness.VM, prefix []string, bin, baseURL string, args ...string) harness.Envelope {
	t.Helper()
	cmd := append([]string{}, prefix...)
	cmd = append(cmd, releaseEnv(baseURL)...)
	cmd = append(cmd, bin)
	cmd = append(cmd, args...)
	cmd = append(cmd, "--output", "json")
	out, code := vm.ExecCode(cmd...)
	env := harness.ParseEnvelope(t, out)
	env.Stdout = out
	env.Exit = code
	return env
}

func versionOf(t *testing.T, vm *harness.VM, bin string) string {
	t.Helper()
	out, err := vm.Exec(bin, "version", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	env := harness.ParseEnvelope(t, out)
	var data map[string]string
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data["version"]
}

type upgradeInfo struct {
	From struct {
		Version string `json:"version"`
		Channel string `json:"channel"`
	} `json:"from"`
	To struct {
		Version string `json:"version"`
		Channel string `json:"channel"`
	} `json:"to"`
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path"`
	Changed      bool   `json:"changed"`
	DryRun       bool   `json:"dry_run"`
	Escalated    bool   `json:"escalated"`
}

func upgradePayload(t *testing.T, env harness.Envelope) upgradeInfo {
	t.Helper()
	var data upgradeInfo
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("upgrade data: %v\n%s", err, env.Stdout)
	}
	return data
}

func detailReason(t *testing.T, env harness.Envelope) string {
	t.Helper()
	if env.Error == nil {
		return ""
	}
	var details struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(env.Error.Details, &details); err != nil {
		t.Fatalf("details: %v\n%s", err, env.Stdout)
	}
	return details.Reason
}

func fileSHA(t *testing.T, vm *harness.VM, path string) string {
	t.Helper()
	out, err := vm.Exec("sha256sum", path)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("sha256sum %s: %s", path, out)
	}
	return fields[0]
}

func vmArch(t *testing.T, vm *harness.VM) string {
	t.Helper()
	out, err := vm.Exec("uname", "-m")
	if err != nil {
		t.Fatal(err)
	}
	switch strings.TrimSpace(out) {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		t.Fatalf("unsupported VM architecture %q", out)
		return ""
	}
}

func flipFileByte(t *testing.T, path string) []byte {
	t.Helper()
	orig, err := os.ReadFile(path) //nolint:gosec // G304: e2e tampers a file under the release temp dir.
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) == 0 {
		t.Fatalf("empty file %s", path)
	}
	next := append([]byte(nil), orig...)
	next[0] ^= 0xff
	if err := os.WriteFile(path, next, 0o644); err != nil { //nolint:gosec // G306: e2e tampers a published test artifact.
		t.Fatal(err)
	}
	return orig
}
