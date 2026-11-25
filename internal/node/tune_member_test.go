package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
)

type memberTunePG struct{}

func (memberTunePG) Query(_ context.Context, _ string, query string, _ map[string]string) ([]map[string]string, error) {
	if strings.Contains(query, "pending_restart") {
		return []map[string]string{{"name": "work_mem"}, {"name": "secret_setting"}}, nil
	}

	return []map[string]string{{"name": "work_mem", "setting": "42"}, {"name": "secret_setting", "setting": "secret_value"}}, nil
}

func (memberTunePG) Exec(context.Context, string, string, map[string]string) error { return nil }

func TestMemberTuneShowScoped(t *testing.T) {
	oldHost, oldPG := detectHost, snapshotPG
	detectHost = func(string) (hostinfo.Info, error) {
		return hostinfo.Info{VCPU: 99}, nil
	}
	snapshotPG = func(spec config.ClusterSpec) sqlRunner {
		if spec.Name != "mine" {
			t.Fatalf("queried another database: %s", spec.Name)
		}

		return memberTunePG{}
	}

	t.Cleanup(func() { detectHost, snapshotPG = oldHost, oldPG })

	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "mine", CreatorOperatorID: "member"},
		{Name: "private_database", CreatorOperatorID: "other"},
	}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{
		IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "member",
	}}

	for _, params := range []map[string]any{nil, {"db": "mine"}} {
		env.Req = request("tune.show", params)

		res, err := handleTuneShow(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}

		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}

		for _, secret := range []string{"private_database", "secret_setting", "secret_value", `"server"`, `"vcpu_share"`, `"disk_gib"`, "99 vCPU", "clusters"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("response leaks %q: %s", secret, raw)
			}
		}

		data := mustAs[map[string]any](t, res.Data)
		if data["database"] != "mine" || mustAs[map[string]string](t, data["current"])["work_mem"] != "42" {
			t.Fatalf("missing database settings: %s", raw)
		}

		pending := mustAs[[]string](t, data["pending_restart"])
		if len(pending) != 1 || pending[0] != "work_mem" {
			t.Fatalf("unexpected pending settings: %v", pending)
		}
	}

	testMemberTuneRefusals(t, env)
}

func testMemberTuneRefusals(t *testing.T, env *Env) {
	t.Helper()

	for _, params := range []map[string]any{
		{"db": "private_database"},
		{"db": "missing"},
		{"db": "mine", "unsupported": true},
	} {
		env.Req = request("tune.show", params)
		if _, err := handleTuneShow(context.Background(), env); err == nil {
			t.Fatalf("accepted unsupported request: %v", params)
		}
	}

	env.DeviceIdentity.IdentityKind = ""
	env.Req = request("tune.show", map[string]any{"db": "mine"})

	if _, err := handleTuneShow(context.Background(), env); err == nil {
		t.Fatal("accepted unverified member")
	}
}

func TestMemberTuneShowRequiresUnambiguousDatabase(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "one", CreatorOperatorID: "member"}, {Name: "two", CreatorOperatorID: "member"}}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{
		IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "member",
	}}

	env.Req = request("tune.show", nil)
	if _, err := handleTuneShow(context.Background(), env); err == nil {
		t.Fatal("accepted ambiguous database")
	}
}
