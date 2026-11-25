package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestMemberConfigValidateRejectsSuppliedConfiguration(t *testing.T) {
	env := &Env{DeviceIdentity: &rpc.WhoamiReply{
		IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "op-member",
	}}
	env.Req = request("config.validate", map[string]any{"toml": "private-value"})

	if _, err := handleConfigValidate(t.Context(), env); err == nil {
		t.Fatal("member supplied configuration to the remote validator")
	}
}

func TestMemberConfigScopedAndUsable(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Domain = "example.test"
	cfg.Postgres.DefaultSize = "small"
	cfg.Server.DataRoot = "/secret-path"
	cfg.Backup.Bucket = "secret-bucket"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "secret-address"}}
	cfg.Clusters = []config.ClusterSpec{
		{Name: "mine", Node: "db1", CreatorOperatorID: "op-member", Settings: map[string]string{"password": "secret-setting"}},
		{Name: "theirs", CreatorOperatorID: "op-other"},
		{Name: "legacy"},
		{Name: "duplicate", CreatorOperatorID: "op-member"},
		{Name: "duplicate", CreatorOperatorID: "op-member"},
	}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "op-member"}}

	raw, err := memberConfig(env, cfg).Encode()
	if err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{"theirs", "legacy", "duplicate", "secret-path", "secret-bucket", "secret-address", "secret-setting"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("config contains %q: %s", secret, raw)
		}
	}

	parsed, err := config.ParseCluster(raw)
	if err != nil {
		t.Fatal(err)
	}

	if parsed.DatabaseHostname("newdb") != "db1.example.test" || parsed.DatabaseHostname("mine") != "db1.example.test" {
		t.Fatalf("missing URL placement: %+v", parsed)
	}

	if r, err := parsed.Resources(config.ClusterSpec{}); err != nil || r.Size != "small" {
		t.Fatalf("missing default sizing: %+v %v", r, err)
	}

	env.DeviceIdentity.IdentityKind = ""
	if len(memberConfig(env, cfg).Clusters) != 0 {
		t.Fatal("unverified identity saw a database")
	}
}

func TestMemberStatusScoped(t *testing.T) {
	old := clusterRunning
	clusterRunning = func(config.ClusterSpec) bool { return false }

	t.Cleanup(func() { clusterRunning = old })

	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "mine", CreatorOperatorID: "op-member"}, {Name: "theirs", CreatorOperatorID: "op-other"}}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "op-member"}}
	env.Req = request("status", nil)

	res, err := handleStatus(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "mine") || strings.Contains(string(raw), "theirs") || strings.Contains(string(raw), cfg.Server.DataRoot) {
		t.Fatalf("unexpected member status: %s", raw)
	}

	env.Req = request("status", map[string]any{"db": "theirs"})
	if _, err := handleStatus(context.Background(), env); err == nil {
		t.Fatal("other database was accessible")
	}

	res, err = handleDBList(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	rows := mustAs[[]databaseInfo](t, mustAs[map[string]any](t, res.Data)["databases"])
	if len(rows) != 1 || rows[0].Name != "mine" {
		t.Fatalf("unexpected member database list: %+v", rows)
	}
}
