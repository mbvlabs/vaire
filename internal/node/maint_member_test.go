package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/maint"
	"github.com/mbvlabs/vaire/internal/rpc"
)

type memberMaintPG struct {
	query func(string, string) ([]map[string]string, error)
}

func (p memberMaintPG) Query(_ context.Context, db, sql string, _ map[string]string) ([]map[string]string, error) {
	return p.query(db, sql)
}
func (memberMaintPG) Exec(context.Context, string, string, map[string]string) error {
	panic("report must not execute mutations")
}

func TestMemberMaintReportScope(t *testing.T) {
	oldPG, oldRunning := maintReportPG, clusterRunning

	t.Cleanup(func() { maintReportPG, clusterRunning = oldPG, oldRunning })

	clusterRunning = func(config.ClusterSpec) bool { return true }

	var queries []string

	maintReportPG = func(spec config.ClusterSpec) sqlRunner {
		if spec.Name != "mine" {
			t.Fatalf("queried another cluster: %s", spec.Name)
		}

		return memberMaintPG{query: func(db, sql string) ([]map[string]string, error) {
			if db != "mine" {
				t.Fatalf("queried database %s", db)
			}

			queries = append(queries, sql)

			return []map[string]string{{"name": "mine", "xid_age": "1"}}, nil
		}}
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "mine", CreatorOperatorID: "member"}, {Name: "private", CreatorOperatorID: "other"}}

	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{
		IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "member",
	}}
	for _, kind := range maint.ReportKinds {
		env.Req = request("maint.report", map[string]any{"kind": kind})

		result, err := handleMaintReport(context.Background(), env)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}

		raw, _ := json.Marshal(result)
		if strings.Contains(string(raw), "private") || !strings.Contains(string(raw), "mine") {
			t.Fatalf("%s: unexpected result %s", kind, raw)
		}
	}

	for _, params := range []map[string]any{
		{"kind": "bloat", "db": "private"},
		{"kind": "bloat", "db": "missing"},
		{"kind": "bloat", "db": "private", "exact": true, "table": "x"},
	} {
		before := len(queries)

		env.Req = request("maint.report", params)
		if _, err := handleMaintReport(context.Background(), env); err == nil || len(queries) != before {
			t.Fatalf("accepted or queried for %v", params)
		}
	}

	env.DeviceIdentity.IdentityKind = ""

	env.Req = request("maint.report", map[string]any{"kind": "bloat", "db": "mine"})
	if _, err := handleMaintReport(context.Background(), env); err == nil {
		t.Fatal("accepted unverified identity")
	}

	env.DeviceIdentity.IdentityKind = rpc.IdentityVerifiedDevice

	env.Cluster.Clusters = append(env.Cluster.Clusters, config.ClusterSpec{Name: "replica", Follows: "private", CreatorOperatorID: "member"})
	for _, db := range []string{"", "replica"} {
		env.Req = request("maint.report", map[string]any{"kind": "bloat", "db": db})
		if _, err := handleMaintReport(context.Background(), env); err == nil {
			t.Fatalf("accepted ambiguous or follower database %q", db)
		}
	}
}

func TestAdminMaintReportRemainsUnscoped(t *testing.T) {
	oldPG, oldRunning := maintReportPG, clusterRunning

	t.Cleanup(func() { maintReportPG, clusterRunning = oldPG, oldRunning })

	clusterRunning = func(config.ClusterSpec) bool { return true }

	var seen []string

	maintReportPG = func(spec config.ClusterSpec) sqlRunner {
		return memberMaintPG{query: func(db, sql string) ([]map[string]string, error) {
			seen = append(seen, db)

			if strings.Contains(sql, "datname = current_database()") {
				t.Fatalf("admin report unexpectedly scoped: %s", sql)
			}

			return nil, errors.New("original error")
		}}
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "one"}, {Name: "two"}}
	env := &Env{Cluster: cfg, Req: request("maint.report", map[string]any{"kind": "wraparound"})}

	result, err := handleMaintReport(context.Background(), env)
	if err != nil || len(seen) != 2 {
		t.Fatalf("admin report: %v, databases %v", err, seen)
	}

	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "original error") {
		t.Fatalf("admin error was redacted: %s", raw)
	}
}

func TestMemberMaintReportSanitizesErrors(t *testing.T) {
	oldPG, oldRunning := maintReportPG, clusterRunning

	t.Cleanup(func() { maintReportPG, clusterRunning = oldPG, oldRunning })

	clusterRunning = func(config.ClusterSpec) bool { return true }
	maintReportPG = func(config.ClusterSpec) sqlRunner {
		return memberMaintPG{query: func(string, string) ([]map[string]string, error) {
			return nil, errors.New("private database on secret host")
		}}
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "mine", CreatorOperatorID: "member"}}

	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{
		IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "member",
	}}
	for _, exact := range []bool{false, true} {
		env.Req = request("maint.report", map[string]any{"kind": "bloat", "db": "mine", "table": map[bool]string{true: "events"}[exact], "exact": exact})
		result, err := handleMaintReport(context.Background(), env)

		raw, _ := json.Marshal(result)
		if strings.Contains(string(raw), "secret") || (err != nil && strings.Contains(err.Error(), "secret")) {
			t.Fatalf("leaked error: %s %v", raw, err)
		}
	}
}
