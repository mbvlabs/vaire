package node

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
	"golang.org/x/crypto/ssh"
)

func TestDispatcherDeniesManagedMethodBeforeHandler(t *testing.T) {
	oldLookup, oldLoad, oldCluster := lookupDeviceUID, loadDeviceRegistry, loadGuardCluster
	oldRoot := serveRunsAsRoot

	t.Cleanup(func() {
		lookupDeviceUID, loadDeviceRegistry, loadGuardCluster = oldLookup, oldLoad, oldCluster
		serveRunsAsRoot = oldRoot
	})
	t.Setenv("SUDO_UID", "123")

	serveRunsAsRoot = func() bool { return true }
	lookupDeviceUID = func(string) (*user.User, error) { return &user.User{Uid: "123", Username: "vdev-1"}, nil }
	loadDeviceRegistry = func(string) (config.AccessRegistry, error) { return deviceRegistryFixture(t), nil }
	loadGuardCluster = func() (config.Cluster, error) { return config.Cluster{}, nil }

	var out bytes.Buffer

	code := Serve(strings.NewReader(`{"schema":"vaire-rpc/v1","method":"apply","params":{"identity_kind":"legacy_shared"}}`), &out)
	if code == 0 || !strings.Contains(out.String(), "device_access_denied") {
		t.Fatalf("dispatcher code %d, output %s", code, out.String())
	}
}

func deviceRegistryFixture(t *testing.T) config.AccessRegistry {
	t.Helper()

	key, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, 32)).Public())
	if err != nil {
		t.Fatal(err)
	}

	wg := make([]byte, 32)
	wg[0] = 1

	return config.AccessRegistry{
		Operators: []config.AccessOperator{{ID: "op-1", Name: "alice", Role: config.AccessRoleMember}},
		Devices: []config.AccessDevice{{ID: "dev-1", OperatorID: "op-1", Label: "laptop", State: config.DeviceActive,
			UnixLogin: "vdev-1", SSHPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
			WGPublicKey: base64.StdEncoding.EncodeToString(wg), Address: "10.0.0.1", RequestDigest: strings.Repeat("a", 64)}},
	}
}

//nolint:gocognit // Exercise the authorization matrix with one shared fixture.
func TestDeviceAuthorization(t *testing.T) {
	oldLookup, oldLoad, oldCluster := lookupDeviceUID, loadDeviceRegistry, loadGuardCluster

	t.Cleanup(func() { lookupDeviceUID, loadDeviceRegistry, loadGuardCluster = oldLookup, oldLoad, oldCluster })

	lookupDeviceUID = func(uid string) (*user.User, error) {
		if uid != "123" {
			return nil, errors.New("unknown")
		}

		return &user.User{Uid: uid, Username: "vdev-1"}, nil
	}
	loadGuardCluster = func() (config.Cluster, error) { return config.Cluster{}, nil }
	registry := deviceRegistryFixture(t)
	loadDeviceRegistry = func(string) (config.AccessRegistry, error) { return registry, nil }

	got, err := authorizeDeviceCaller("123", "whoami")
	if err != nil || got == nil || got.IdentityKind != rpc.IdentityVerifiedDevice ||
		got.SSHAccount != "vdev-1" || got.OperatorID != "op-1" || got.Operator != "alice" ||
		got.DeviceID != "dev-1" || got.Device != "laptop" || got.Role != config.AccessRoleMember {
		t.Fatalf("identity = %+v, %v", got, err)
	}

	for name := range methods() {
		if name == "whoami" {
			continue
		}

		if _, err := authorizeDeviceCaller("123", name); err == nil {
			t.Errorf("allowed %s", name)
		}
	}

	for _, state := range []string{config.DevicePending, config.DeviceRevoked} {
		registry.Devices[0].State = state
		if _, err := authorizeDeviceCaller("123", "whoami"); err == nil {
			t.Errorf("allowed %s", state)
		}
	}

	registry.Devices[0].State = config.DeviceActive
	registry.Devices[0].UnixLogin = "vdev-other"

	if _, err := authorizeDeviceCaller("123", "whoami"); err == nil {
		t.Error("accepted a label or unmatched login")
	}

	for _, uid := range []string{"00", "000123", "bad", "124"} {
		if _, err := authorizeDeviceCaller(uid, "whoami"); err == nil {
			t.Errorf("accepted UID %q", uid)
		}
	}

	for _, uid := range []string{"", "0"} {
		if got, err := authorizeDeviceCaller(uid, "apply"); err != nil || got != nil {
			t.Errorf("root %q: %+v %v", uid, got, err)
		}
	}

	loadGuardCluster = func() (config.Cluster, error) {
		var c config.Cluster

		c.Server.AdminUser = "vdev-1"

		return c, nil
	}

	if got, err := authorizeDeviceCaller("123", "apply"); err != nil || got != nil {
		t.Errorf("legacy collision: %+v %v", got, err)
	}
}

func TestDeviceRegistryFileSafety(t *testing.T) {
	old := rootOwned
	rootOwned = func(os.FileInfo) bool { return true }

	t.Cleanup(func() { rootOwned = old })

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "registry")

	raw, err := deviceRegistryFixture(t).Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceRegistry(path); err != nil {
		t.Fatalf("secure registry: %v", err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceRegistry(path); err == nil {
		t.Error("accepted readable registry")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceRegistry(path); err == nil {
		t.Error("accepted symlink")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceRegistry(path); err == nil {
		t.Error("accepted absent registry")
	}

	if err := os.WriteFile(path, []byte("not valid TOML"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceRegistry(path); err == nil {
		t.Error("accepted malformed registry")
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	rootOwned = func(os.FileInfo) bool { return false }

	if _, err := readDeviceRegistry(path); err == nil {
		t.Error("accepted unowned registry or directory")
	}

	rootOwned = func(os.FileInfo) bool { return true }

	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceRegistry(path); err == nil {
		t.Error("accepted writable directory")
	}
}
