package node

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func followerCfg() config.Cluster {
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, WGAddress: config.DefaultCentralWG}}
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-3f9a1c", Size: "large", StatementTimeout: "60s",
			Extensions: []string{"pg_stat_statements"}, Settings: map[string]string{"work_mem": "8MB"},
			Backup: config.ClusterBackup{RetentionFull: 4}},
		{Name: "app_follower", Node: "db1", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app"},
	}
	return cfg
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var ce *output.CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("not a CLI error: %v", err)
	}
	d, _ := ce.Details.(map[string]any)
	r, _ := d["reason"].(string)
	return r
}

func request(method string, params map[string]any) rpc.Request {
	raw, _ := json.Marshal(params)
	return rpc.Request{Method: method, Params: raw}
}

func TestRefuseFollower(t *testing.T) {
	cfg := followerCfg()
	refused := []rpc.Request{
		request("backup.now", map[string]any{"database": "app_follower"}),
		request("backup.verify", map[string]any{"database": "app_follower"}),
		request("backup.list", map[string]any{"database": "app_follower"}),
		request("restore.start", map[string]any{"database": "app_follower"}),
		request("restore.drill", map[string]any{"database": "app_follower"}),
		request("dump.create", map[string]any{"database": "app_follower"}),
		request("db.fork", map[string]any{"source": "app_follower", "name": "x"}),
		request("maint.run", map[string]any{"db": "app_follower", "kind": "vacuum"}),
		request("db.credentials", map[string]any{"name": "app_follower", "rotate": true}),
	}
	for _, req := range refused {
		err := refuseFollower(req, cfg)
		if err == nil {
			t.Errorf("%s is not refused for a follower", req.Method)
			continue
		}
		if got := reason(t, err); got != "is_follower" {
			t.Errorf("%s: reason %q", req.Method, got)
		}
	}
	allowed := []rpc.Request{
		request("backup.now", map[string]any{"database": "app"}),
		request("db.fork", map[string]any{"source": "app", "name": "x"}),
		request("db.credentials", map[string]any{"name": "app_follower"}),
		request("db.resize", map[string]any{"name": "app_follower", "size": "medium"}),
		request("maint.report", map[string]any{"db": "app_follower"}),
		request("sql", map[string]any{"db": "app_follower"}),
		// D10: a follower may be unfollowed, protected or not. db.follow's
		// refusal of a follower is its handler's (follows_follower), not
		// this list's.
		request("db.unfollow", map[string]any{"name": "app_follower"}),
		request("db.follow", map[string]any{"source": "app", "name": "app_follower"}),
	}
	for _, req := range allowed {
		if err := refuseFollower(req, cfg); err != nil {
			t.Errorf("%s is refused: %v", req.Method, err)
		}
	}
}

func TestRefuseSourceDrop(t *testing.T) {
	cfg := followerCfg()
	err := refuseSourceDrop(cfg, "app")
	if err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("dropping a source with followers: %v", err)
	}
	if err := refuseSourceDrop(cfg, "app_follower"); err != nil {
		t.Fatalf("dropping a follower: %v", err)
	}
}

func TestCheckFollowSourceRefusesCascading(t *testing.T) {
	cfg := followerCfg()
	f, _ := cfg.ClusterByName("app_follower")
	err := checkFollowSource(f, "db follow")
	if err == nil || reason(t, err) != "follows_follower" {
		t.Fatalf("following a follower: %v", err)
	}
}

func TestUnfollowToken(t *testing.T) {
	if got := unfollowToken(config.Cluster{}, json.RawMessage(`{"name":"app_follower"}`)); got != "app_follower/unfollow" {
		t.Fatalf("token %q", got)
	}
	if got := unfollowToken(config.Cluster{}, json.RawMessage(`{}`)); got != "" {
		t.Fatalf("token without a name %q", got)
	}
}

func TestFollowerSpecAndCommand(t *testing.T) {
	cfg := followerCfg()
	src, _ := cfg.ClusterByName("app")
	spec := followerSpec(src, "f", "")
	if spec.Follows != "app" || spec.Size != "large" || spec.Stanza != "" || spec.Backup != (config.ClusterBackup{}) || spec.Settings["work_mem"] != "8MB" {
		t.Fatalf("follower spec %+v", spec)
	}
	if small := followerSpec(src, "f", "small"); small.Size != "small" || small.Memory != "" {
		t.Fatalf("--size: %+v", small)
	}
	if got := followerName(dbFollowParams{Source: "app"}); got != "app_follower" {
		t.Fatalf("default name %q", got)
	}
	if got := followCommand(dbFollowParams{Source: "app", Name: "f"})("small"); got != "vaire db follow app --name f --size small" {
		t.Fatalf("suggested command %q", got)
	}
}

func TestPlaceFollowerHasNoStanza(t *testing.T) {
	cfg := followerCfg()
	src, _ := cfg.ClusterByName("app")
	spec := followerSpec(src, "f2", "small")
	if err := placeCluster(cfg, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Stanza != "" || spec.Port != 5435 || spec.Node != "db1" {
		t.Fatalf("placed %+v", spec)
	}
	cfg.Clusters = append(cfg.Clusters, spec)
	if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "clusters[") {
		t.Fatalf("the placed follower does not validate: %v", err)
	}
}

func TestFollowersDiverged(t *testing.T) {
	cfg := followerCfg()
	w := followersDiverged(cfg, "app")
	if len(w) != 1 || w[0].Code != "followers_diverged" || !strings.Contains(w[0].Message, "vaire db follow app --name app_follower") {
		t.Fatalf("%+v", w)
	}
	if followersDiverged(cfg, "app_follower") != nil {
		t.Fatal("a follower has no followers")
	}
}

func TestOwnedSlots(t *testing.T) {
	cfg := followerCfg()
	cfg.Awaiting = []config.ClusterSpec{{Name: "later", Follows: "app"}}
	got := ownedSlots(cfg, "app")
	if len(got) != 2 || got[0] != "vaire_app_follower" || got[1] != "vaire_later" {
		t.Fatalf("owned %v", got)
	}
}

func TestFollowerFamilies(t *testing.T) {
	fams := followerFamilies(map[string]followerReading{
		"app_follower": {Source: "app", Streaming: true, LagBytes: 10, SlotExists: true},
		"old":          {Source: "app", SlotExists: true, SlotWALStatus: "lost"},
	})
	byName := map[string]float64{}
	for _, f := range fams {
		for _, s := range f.Samples {
			byName[f.Name+"/"+s.Labels[0].Value] = s.Value
		}
	}
	if byName["vaire_follower_streaming/app_follower"] != 1 || byName["vaire_follower_lag_bytes/app_follower"] != 10 ||
		byName["vaire_follower_slot_lost/old"] != 1 || byName["vaire_follower_slot_lost/app_follower"] != 0 {
		t.Fatalf("%v", byName)
	}
}

func TestUnfollowRenames(t *testing.T) {
	r := unfollowRenames("app", "app_follower")
	if r["app"] != "app_follower" || r["app_migrate"] != "app_follower_migrate" || r["app_owner"] != "app_follower_owner" {
		t.Fatalf("%v", r)
	}
}

// A completed copy may only be resumed on a slot that still retains WAL: a
// missing, lost, unreserved or restartless slot is not usable, nor is a status
// vaire does not know, while an ordinary inactive slot with an older position
// is.
func TestFollowerSlotState(t *testing.T) {
	for name, test := range map[string]struct {
		rows []map[string]string
		want followerSlotState
	}{
		"missing":        {want: followerSlotState{}},
		"retaining":      {rows: []map[string]string{{"wal_status": "reserved", "has_restart": "true"}}, want: followerSlotState{Exists: true, Usable: true}},
		"inactive":       {rows: []map[string]string{{"wal_status": "extended", "has_restart": "true"}}, want: followerSlotState{Exists: true, Usable: true}},
		"lost":           {rows: []map[string]string{{"wal_status": "lost", "has_restart": "true"}}, want: followerSlotState{Exists: true}},
		"unreserved":     {rows: []map[string]string{{"wal_status": "unreserved", "has_restart": "true"}}, want: followerSlotState{Exists: true}},
		"no restart":     {rows: []map[string]string{{"wal_status": "reserved", "has_restart": "false"}}, want: followerSlotState{Exists: true}},
		"unknown status": {rows: []map[string]string{{"wal_status": "future_status", "has_restart": "true"}}, want: followerSlotState{Exists: true}},
	} {
		if got := followerSlotStateOf(test.rows); got != test.want {
			t.Errorf("%s: %+v", name, got)
		}
	}
}
