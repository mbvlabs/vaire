package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
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

func TestManagedRecheckAfterLock(t *testing.T) {
	oldLookup, oldLoad, oldCluster, oldAcquire := lookupDeviceUID, loadDeviceRegistry, loadGuardCluster, serveAcquire

	t.Cleanup(func() {
		lookupDeviceUID, loadDeviceRegistry, loadGuardCluster, serveAcquire = oldLookup, oldLoad, oldCluster, oldAcquire
	})

	t.Setenv("SUDO_UID", "123")

	registry := deviceRegistryFixture(t)
	registry.Operators[0].Role = config.AccessRoleOwner
	lookupDeviceUID = func(string) (*user.User, error) { return &user.User{Uid: "123", Username: "vdev-1"}, nil }
	loadDeviceRegistry = func(string) (config.AccessRegistry, error) { return registry, nil }
	loadGuardCluster = func() (config.Cluster, error) { return config.Cluster{}, nil }

	identity, err := authorizeDeviceCaller("123", "doctor.run")
	if err != nil {
		t.Fatal(err)
	}

	locked := false
	serveAcquire = func(_ context.Context, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
		locked = mode == engine.LockShared
		registry.Devices[0].State = config.DeviceRevoked

		return func() {}, nil
	}

	req := rpc.Request{Method: "doctor.run"}
	env := &Env{DeviceIdentity: identity}
	out := &eventWriter{w: &bytes.Buffer{}}
	releases, _, ok := acquireServeLocks(context.Background(), method{}, req, env, nil, out)

	for _, release := range releases {
		defer release()
	}

	if !ok || !locked {
		t.Fatal("managed lockless RPC did not acquire a shared global lock")
	}

	if _, ok := serveRecheck(context.Background(), method{}, req, env, nil, out); ok {
		t.Fatal("revoked device passed the locked recheck")
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
			UnixLogin: "vdev-1", UnixUID: "123", SSHPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
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

	for _, name := range []string{"apply", "sql", "ssh.confirm", "upgrade.install", "user.add", "device.list", "unknown",
		"db.create", "db.write-access", "auth.setup", "protect.verify", "db.drop", "db.resize", "job.list", "job.follow", "metrics.show", "queries.top", "maint.report", "backup.now", "backup.verify"} {
		if _, err := authorizeDeviceCaller("123", name); err == nil {
			t.Errorf("member allowed %s", name)
		}
	}

	for _, name := range []string{"whoami", "status", "config.get", "db.list", "db.access.show", "ext.list", "tune.show",
		"tls.status", "backup.list", "restore.status", "activity.list", "activity.show", "activity.health", "protect.status"} {
		if _, err := authorizeDeviceCaller("123", name); err != nil {
			t.Errorf("member denied %s: %v", name, err)
		}
	}

	registry.Operators[0].Role = config.AccessRoleOwner

	for _, name := range []string{"user.add", "user.list", "device.add", "device.list", "db.create", "db.write-access", "protect.verify", "backup.now", "upgrade.install"} {
		if _, err := authorizeDeviceCaller("123", name); err != nil {
			t.Errorf("owner denied %s: %v", name, err)
		}
	}

	if _, err := authorizeDeviceCaller("123", "config.set"); err != nil {
		t.Errorf("owner denied config.set: %v", err)
	}

	registry.Operators[0].Revoked = true

	if _, err := authorizeDeviceCaller("123", "config.set"); err == nil {
		t.Error("allowed revoked operator")
	}

	registry.Operators[0].Revoked = false
	registry.Devices[0].UnixUID = "124"

	if _, err := authorizeDeviceCaller("123", "whoami"); err == nil {
		t.Error("allowed changed Unix UID")
	}

	registry.Devices[0].UnixUID = "123"

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

// Classify every registered RPC explicitly. A new method must be reviewed
// here as well as in managedDeviceMethod; unknown methods remain denied.
func TestManagedDeviceMethodCoverage(t *testing.T) {
	member := []string{
		"whoami", "status", "config.get", "db.list", "db.access.show", "ext.list",
		"tune.show", "tls.status", "backup.list", "restore.status", "activity.list",
		"activity.show", "activity.health", "protect.status",
	}
	owner := []string{
		"user.add", "user.list", "user.revoke", "device.add", "device.list", "device.revoke",
		"access.recovery-issue", "access.complete-setup", "plan", "db.create", "db.recover",
		"db.write-access", "auth.status", "auth.setup", "protect.verify", "backup.now",
		"master-backup.now", "master-backup.list", "master-backup.get", "upgrade.install",
		"apply", "config.set", "config.validate", "db.copy.preflight", "db.copy", "db.drop",
		"db.resize", "db.credentials", "tune.apply", "ext.add", "ext.remove", "logs",
		"backup.verify", "job.list", "job.follow", "restore.start", "restore.promote",
		"restore.undo", "restore.abort", "restore.cleanup", "restore.drill", "db.fork",
		"db.follow", "db.unfollow", "sql", "metrics.show", "doctor.run", "queries.top",
		"maint.report", "maint.run", "maint.kill", "queries.reset", "metrics.rotate",
	}
	// Setup-only, node-internal and obsolete protection setup RPCs.
	denied := []string{"selfcheck", "access.bootstrap-owner", "cluster.bootstrap",
		"cluster.confirm-secrets", "ssh.lockdown", "ssh.confirm", "ssh.status",
		"protect.init", "protect.confirm"}

	table := methods()
	seen := make(map[string]bool)

	for _, group := range []struct {
		names             []string
		memberOK, ownerOK bool
	}{{member, true, true}, {owner, false, true}, {denied, false, false}} {
		for _, name := range group.names {
			if seen[name] {
				t.Errorf("duplicate classification for %s", name)
			}

			seen[name] = true
			if _, ok := table[name]; !ok {
				t.Errorf("classified unregistered method %s", name)
			}

			assertManagedDeviceRoles(t, name, group.memberOK, group.ownerOK)
		}
	}

	for name := range table {
		if !seen[name] {
			t.Errorf("registered method %s has no authorization classification", name)
		}
	}

	assertManagedDeviceRoles(t, "unknown", false, false)
}

func assertManagedDeviceRoles(t *testing.T, name string, memberOK, adminOK bool) {
	t.Helper()

	for role, want := range map[string]bool{config.AccessRoleMember: memberOK, config.AccessRoleOwner: adminOK, "unknown": false} {
		if got := managedDeviceMethod(role, name); got != want {
			t.Errorf("%s role %s: got %t, want %t", name, role, got, want)
		}
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
