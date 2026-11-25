package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/top"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestMemberQueriesAndTopScoped(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "mine", CreatorOperatorID: "member"},
		{Name: "theirs", CreatorOperatorID: "other"},
		{Name: "legacy"},
	}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "member"}}
	old := queriesLocalQuery

	t.Cleanup(func() { queriesLocalQuery = old })

	var queried []string

	queriesLocalQuery = func(_ context.Context, spec config.ClusterSpec, sql string) ([]map[string]string, error) {
		queried = append(queried, spec.Name)

		switch {
		case strings.Contains(sql, "pg_extension"):
			return []map[string]string{{"n": "1"}}, nil
		case strings.Contains(sql, "pg_stat_activity"):
			return []map[string]string{
				{"pid": "12", "role": "mine_app", "query": "owned", "blocked_by": "13,99"},
				{"pid": "13", "role": "mine_app", "query": "owned too"},
				{"pid": "99", "role": "postgres", "query": "secret"},
			}, nil
		case strings.Contains(sql, "pg_stat_database"):
			return []map[string]string{{"xacts": "1"}}, nil
		default:
			return []map[string]string{{"role": "mine_app", "queryid": "1", "query": "owned", "calls": "1"}}, nil
		}
	}

	for _, sample := range []bool{false, true} {
		queried = nil
		env.Req = request("queries.top", map[string]any{"sample": sample})
		assertMemberQueryResult(t, env, &queried)
	}

	assertMemberQueryDenials(t, env, &queried)
}

func assertMemberQueryResult(t *testing.T, env *Env, queried *[]string) {
	t.Helper()

	res, err := handleQueriesTop(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "owned") || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "theirs") || strings.Contains(string(raw), "99") {
		t.Fatalf("unsafe result: %s", raw)
	}

	if strings.Contains(string(env.Req.Params), `"sample":true`) {
		var samples map[string]top.Sample

		encoded, err := json.Marshal(res.Data)
		if err != nil {
			t.Fatal(err)
		}

		if err := json.Unmarshal(encoded, &samples); err != nil {
			t.Fatal(err)
		}

		s := samples["mine"]
		if len(s.Sessions) != 2 || len(s.Sessions[0].BlockedBy) != 1 || s.Sessions[0].BlockedBy[0] != 13 || s.Statements["1"].Query != "owned" {
			t.Fatalf("invalid sample after RPC JSON round trip: %+v", s)
		}
	}

	for _, db := range *queried {
		if db != "mine" {
			t.Fatalf("queried unowned database %q", db)
		}
	}
}

func assertMemberQueryDenials(t *testing.T, env *Env, queried *[]string) {
	t.Helper()

	for _, params := range []map[string]any{
		{"db": "theirs"}, {"db": "legacy"}, {"db": "missing"}, {"all_sessions": true}, {"sample": true, "all_sessions": true},
	} {
		*queried = nil

		env.Req = request("queries.top", params)
		if _, err := handleQueriesTop(context.Background(), env); err == nil || len(*queried) != 0 {
			t.Fatalf("request %#v was not denied before querying: %v, %v", params, err, *queried)
		}
	}

	env.Req = request("queries.reset", map[string]any{"db": "mine"})
	if _, err := handleQueriesReset(context.Background(), env); err == nil {
		t.Fatal("member reset allowed")
	}

	env.DeviceIdentity.IdentityKind = ""

	env.Req = request("queries.top", map[string]any{"db": "mine"})
	if _, err := handleQueriesTop(context.Background(), env); err == nil {
		t.Fatal("unverified identity allowed")
	}
}
