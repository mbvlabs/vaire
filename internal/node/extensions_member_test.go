package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestMemberExtensionInventoryScope(t *testing.T) {
	oldRunning, oldInstalled := extensionClusterRunning, extensionInstalled

	t.Cleanup(func() { extensionClusterRunning, extensionInstalled = oldRunning, oldInstalled })

	extensionClusterRunning = func(spec config.ClusterSpec) bool {
		if spec.Name != "mine" {
			t.Fatalf("queried another database: %s", spec.Name)
		}

		return true
	}
	extensionInstalled = func(_ context.Context, spec config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"owned_ext": {Name: "owned_ext", Version: "1"}}, nil
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "mine", CreatorOperatorID: "me"}, {Name: "theirs", CreatorOperatorID: "other"}, {Name: "legacy"}}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "me"}}
	env.Req = request("ext.list", nil)

	result, err := handleExtList(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "owned_ext") || strings.Contains(string(raw), "theirs") || strings.Contains(string(raw), "legacy") {
		t.Fatalf("unexpected inventory: %s", raw)
	}

	env.Req = request("ext.list", map[string]any{"db": "theirs"})
	if _, err := handleExtList(context.Background(), env); err == nil {
		t.Fatal("other database was accessible")
	}

	env.Req = request("ext.list", map[string]any{"db": "mine"})
	extensionInstalled = func(context.Context, config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
		return nil, errors.New("secret host path")
	}

	result, err = handleExtList(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	raw, _ = json.Marshal(result.Data)
	if strings.Contains(string(raw), "secret host path") {
		t.Fatalf("leaked error: %s", raw)
	}

	env.DeviceIdentity.IdentityKind = ""
	if _, err := handleExtList(context.Background(), env); err == nil {
		t.Fatal("unverified target was accessible")
	}
}
