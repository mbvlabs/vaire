package node

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestUserManagement(t *testing.T) {
	account, err := user.Current()
	if err != nil || account.Uid == "0" {
		t.Skip("requires a non-root test account")
	}

	t.Setenv("SUDO_UID", account.Uid)

	oldPath, oldRoot := deviceRegistryPath, rootOwned
	deviceRegistryPath = filepath.Join(t.TempDir(), "access-devices.toml")
	rootOwned = func(os.FileInfo) bool { return true }

	t.Cleanup(func() { deviceRegistryPath, rootOwned = oldPath, oldRoot })

	env := &Env{}
	env.Cluster.Server.AdminUser = account.Username

	add := func(name, role string, dry bool) error {
		raw, _ := json.Marshal(rpc.UserAddParams{Name: name, Role: role})
		env.Req = rpc.Request{Params: raw, DryRun: dry}
		_, err := handleUserAdd(context.Background(), env)

		return err
	}
	if err := add("alice", "", true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(deviceRegistryPath); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote registry: %v", err)
	}

	if err := add("alice", "", false); err != nil {
		t.Fatal(err)
	}

	if err := add("bob", "admin", false); err != nil {
		t.Fatal(err)
	}

	for _, p := range [][2]string{{"alice", "admin"}, {"Bad Name", "member"}, {"charlie", "owner"}} {
		if err := add(p[0], p[1], false); err == nil {
			t.Fatalf("accepted invalid or duplicate user %v", p)
		}
	}

	result, err := handleUserList(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	reply, ok := result.Data.(rpc.UsersReply)
	if !ok {
		t.Fatalf("unexpected list response: %T", result.Data)
	}

	users := reply.Users
	if len(users) != 2 || users[0].Name != "alice" || users[0].Role != "member" || users[1].Role != "admin" || users[0].Status != "active" {
		t.Fatalf("users: %+v", users)
	}

	info, err := os.Stat(deviceRegistryPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("registry mode: %v %v", info, err)
	}

	env.Cluster.Server.AdminUser = "another-account"
	if _, err := handleUserList(context.Background(), env); err == nil {
		t.Fatal("non-admin listed users")
	}

	if err := add("mallory", "admin", false); err == nil {
		t.Fatal("non-admin added user")
	}

	env.Cluster.Server.AdminUser = account.Username
	env.DeviceIdentity = &rpc.WhoamiReply{Role: config.AccessRoleOwner}

	if err := add("mallory", "admin", false); err == nil {
		t.Fatal("unbound device owner authorized")
	}

	env.DeviceIdentity = nil

	if err := os.Chmod(deviceRegistryPath, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := handleUserList(context.Background(), env); err == nil {
		t.Fatal("unsafe registry accepted")
	}
}

func TestUserMethodsUseNodeLocks(t *testing.T) {
	for name, mode := range map[string]engine.LockMode{"user.add": engine.LockExclusive, "user.list": engine.LockShared} {
		m := methods()[name]
		if m.lock == nil || m.lock(rpc.Request{}) != mode || !m.cluster {
			t.Fatalf("%s has incorrect lock or cluster", name)
		}
	}
}
