package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestMemberProtectStatusScope(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "mine", CreatorOperatorID: "me", Protected: true, Follows: "theirs"},
		{Name: "theirs", CreatorOperatorID: "other", Protected: true},
		{Name: "legacy", Protected: true},
	}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "me"}}

	result, err := handleProtectStatus(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"mine"`) || strings.Contains(string(raw), "theirs") || strings.Contains(string(raw), "legacy") {
		t.Fatalf("unexpected status: %s", raw)
	}

	env.DeviceIdentity.IdentityKind = ""

	result, err = handleProtectStatus(context.Background(), env)
	if err != nil || len(mustAs[protectStatus](t, result.Data).Databases) != 0 {
		t.Fatalf("unverified status: %+v, %v", result, err)
	}
}
