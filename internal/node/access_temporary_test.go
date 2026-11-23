package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const temporaryTestKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE="

func TestTemporaryAccessHostOrder(t *testing.T) {
	old := temporaryAccessCommand

	t.Cleanup(func() { temporaryAccessCommand = old })

	var calls []string

	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")

		calls = append(calls, call)
		if strings.Contains(call, " -C ") {
			return "", errors.New("not found")
		}

		return "", nil
	}

	if err := addTemporaryAccessPeer(context.Background(), "10.0.0.1", "10.0.0.2", temporaryTestKey); err != nil {
		t.Fatal(err)
	}

	if len(calls) != 10 || !strings.HasPrefix(calls[9], "wg set wg0 peer ") {
		t.Fatalf("expected four drop rules before peer: %v", calls)
	}

	for _, want := range []string{"-t security -I FORWARD -i wg0 -s 10.0.0.2/32 -j DROP", "! -d 10.0.0.1/32", "! -p tcp", "! --dport 6418"} {
		if !strings.Contains(strings.Join(calls, "\n"), want) {
			t.Errorf("missing isolation %q: %v", want, calls)
		}
	}

	calls = nil
	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}

	if err := removeTemporaryAccessPeer(context.Background(), "10.0.0.1", "10.0.0.2", temporaryTestKey); err != nil {
		t.Fatal(err)
	}

	if len(calls) != 10 || !strings.Contains(calls[1], " remove") || !strings.Contains(calls[3], " -D ") {
		t.Fatalf("expected peer removal before rule cleanup: %v", calls)
	}
}

func TestTemporaryAccessFailureIsClosed(t *testing.T) {
	old := temporaryAccessCommand

	t.Cleanup(func() { temporaryAccessCommand = old })

	var calls []string

	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")

		calls = append(calls, call)
		if strings.Contains(call, " -C ") || strings.Contains(call, " -I ") {
			return "", errors.New("iptables failed")
		}

		return "", nil
	}

	if err := addTemporaryAccessPeer(context.Background(), "10.0.0.1", "10.0.0.2", temporaryTestKey); err == nil {
		t.Fatal("expected firewall failure")
	}

	if strings.Contains(strings.Join(calls, "\n"), "wg set") {
		t.Fatalf("installed peer without isolation: %v", calls)
	}

	calls = nil
	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")

		calls = append(calls, call)
		if strings.Contains(call, " remove") {
			return "", errors.New("wg failed")
		}

		return "", nil
	}

	if err := removeTemporaryAccessPeer(context.Background(), "10.0.0.1", "10.0.0.2", temporaryTestKey); err == nil {
		t.Fatal("expected removal failure")
	}

	if len(calls) != 2 {
		t.Fatalf("removed firewall after failed peer removal: %v", calls)
	}
}

func TestTemporaryAccessUnits(t *testing.T) {
	oldDir, oldCommand := temporaryAccessUnitDir, temporaryAccessCommand

	t.Cleanup(func() { temporaryAccessUnitDir, temporaryAccessCommand = oldDir, oldCommand })
	temporaryAccessUnitDir = t.TempDir()

	var calls []string

	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}

	if err := prepareTemporaryAccess(context.Background(), "10.0.0.1"); err != nil {
		t.Fatal(err)
	}

	for name, expected := range map[string][]string{
		"vaire-access-enroll.socket":   {"ListenStream=10.0.0.1:6418", "Accept=yes", "MaxConnections=32", "MaxConnectionsPerSource=4"},
		"vaire-access-enroll@.service": {"StandardInput=socket", "StandardOutput=socket", "StandardError=journal", "/usr/local/bin/vaire node access-enroll", "RuntimeMaxSec=30s"},
		"vaire-access-expire.timer":    {"OnBootSec=1min", "OnUnitActiveSec=1min"},
		"vaire-access-expire.service":  {"/usr/local/bin/vaire node access-expire"},
	} {
		data, err := os.ReadFile(filepath.Join(temporaryAccessUnitDir, name))
		if err != nil {
			t.Fatal(err)
		}

		for _, want := range expected {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}

	calls = nil

	if err := prepareTemporaryAccess(context.Background(), "10.0.0.1"); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(strings.Join(calls, "\n"), "systemctl restart") {
		t.Fatalf("restarted unchanged socket: %v", calls)
	}

	calls = nil

	if err := prepareTemporaryAccess(context.Background(), "10.0.0.3"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.Join(calls, "\n"), "systemctl restart vaire-access-enroll.socket") {
		t.Fatalf("changed socket was not restarted: %v", calls)
	}
}

//nolint:gocognit // Table-driven cases exercise distinct repair and collision outcomes.
func TestTemporaryAccessReconcile(t *testing.T) {
	old := temporaryAccessCommand

	t.Cleanup(func() { temporaryAccessCommand = old })

	for _, tc := range []struct {
		name, allowed, missing string
		wantErr                bool
		wantRemove, wantAdd    bool
	}{
		{"converged", temporaryTestKey + "\t10.0.0.2/32\n", "", false, false, false},
		{"repair forward", temporaryTestKey + "\t10.0.0.2/32\n", "-t security -C FORWARD", false, true, true},
		{"wrong address", temporaryTestKey + "\t10.0.0.3/32\n", "", true, false, false},
		{"conflicting address", "other\t10.0.0.0/24\n", "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string

			temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
				call := name + " " + strings.Join(args, " ")

				calls = append(calls, call)
				if strings.HasPrefix(call, "wg show ") {
					return tc.allowed, nil
				}

				if tc.missing != "" && strings.Contains(call, tc.missing) {
					return "", errors.New("missing")
				}

				return "", nil
			}

			err := addTemporaryAccessPeer(context.Background(), "10.0.0.1", "10.0.0.2", temporaryTestKey)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v, calls %v", err, calls)
			}

			all := strings.Join(calls, "\n")
			remove := strings.Index(all, "wg set wg0 peer "+temporaryTestKey+" remove")

			add := strings.Index(all, "wg set wg0 peer "+temporaryTestKey+" allowed-ips")
			if (remove >= 0) != tc.wantRemove || (add >= 0) != tc.wantAdd {
				t.Fatalf("unexpected peer changes: %v", calls)
			}

			if tc.wantRemove && (remove >= strings.Index(all, "-t security -I FORWARD") || strings.Index(all, "-t security -I FORWARD") >= add) {
				t.Fatalf("repair must remove peer, restore guard, then add peer: %v", calls)
			}
		})
	}
}

func TestTemporaryAccessSocketRestartRetry(t *testing.T) {
	oldDir, oldCommand := temporaryAccessUnitDir, temporaryAccessCommand

	t.Cleanup(func() { temporaryAccessUnitDir, temporaryAccessCommand = oldDir, oldCommand })
	temporaryAccessUnitDir = t.TempDir()
	failRestart := false
	restarts := 0
	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "restart" {
			restarts++

			if failRestart {
				return "", errors.New("restart failed")
			}
		}

		return "", nil
	}

	if err := prepareTemporaryAccess(context.Background(), "10.0.0.1"); err != nil {
		t.Fatal(err)
	}

	failRestart = true

	if err := prepareTemporaryAccess(context.Background(), "10.0.0.3"); err == nil {
		t.Fatal("expected restart failure")
	}

	socket, err := os.ReadFile(filepath.Join(temporaryAccessUnitDir, "vaire-access-enroll.socket"))
	if err != nil || !strings.Contains(string(socket), "ListenStream=10.0.0.1:6418") {
		t.Fatalf("socket unit was not restored: %s, %v", socket, err)
	}

	failRestart = false

	if err := prepareTemporaryAccess(context.Background(), "10.0.0.3"); err != nil {
		t.Fatal(err)
	}

	if restarts != 2 {
		t.Fatalf("expected failed restart and retry, got %d", restarts)
	}
}
