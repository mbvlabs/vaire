package config

import (
	"strings"
	"testing"
)

func followerCluster() Cluster {
	cfg := DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Nodes = []NodeSpec{{Name: "db1", Role: RoleMaster, WGAddress: DefaultCentralWG}}
	cfg.Clusters = []ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-3f9a1c", Size: "small", StatementTimeout: "60s"},
		{Name: "app_follower", Node: "db1", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app"},
	}
	return cfg
}

// validationProblems is Validate's message, "" when it passes.
func validationProblems(cfg Cluster) string {
	if err := cfg.Validate(); err != nil {
		return err.Error()
	}
	return ""
}

func TestFollowerValidates(t *testing.T) {
	if p := validationProblems(followerCluster()); strings.Contains(p, "clusters[") {
		t.Fatalf("a valid follower is refused: %s", p)
	}
}

func TestFollowerValidationRefusals(t *testing.T) {
	cases := map[string]struct {
		edit func(*Cluster)
		want string
	}{
		"cascading": {func(c *Cluster) {
			c.Clusters = append(c.Clusters, ClusterSpec{Name: "f2", Node: "db1", Port: 5435, Size: "small", StatementTimeout: "60s", Follows: "app_follower"})
		}, "is itself a follower"},
		"self":                   {func(c *Cluster) { c.Clusters[1].Follows = "app_follower" }, "can't name the database itself"},
		"unknown source":         {func(c *Cluster) { c.Clusters[1].Follows = "nope" }, "is not a database"},
		"stanza":                 {func(c *Cluster) { c.Clusters[1].Stanza = "app_follower-0a0b0c" }, "must be empty for a follower"},
		"backup":                 {func(c *Cluster) { c.Clusters[1].Backup.RetentionFull = 2 }, "backup must be empty"},
		"primary without stanza": {func(c *Cluster) { c.Clusters[0].Stanza = "" }, "must be app-<6 hex>"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := followerCluster()
			tc.edit(&cfg)
			if p := validationProblems(cfg); !strings.Contains(p, tc.want) {
				t.Fatalf("want %q in %q", tc.want, p)
			}
		})
	}
}

func TestFollowerHelpers(t *testing.T) {
	cfg := followerCluster()
	f, _ := cfg.ClusterByName("app_follower")
	app, _ := cfg.ClusterByName("app")
	if !f.IsFollower() || app.IsFollower() {
		t.Fatal("IsFollower")
	}
	if f.DatabaseName() != "app" || app.DatabaseName() != "app" {
		t.Fatal("DatabaseName")
	}
	if got := cfg.FollowerNames("app"); len(got) != 1 || got[0] != "app_follower" {
		t.Fatalf("FollowerNames %v", got)
	}
	if got := cfg.StanzaClusters(); len(got) != 1 || got[0].Name != "app" {
		t.Fatalf("StanzaClusters %v", got)
	}
	if FollowerSlot("app_follower") != "vaire_app_follower" {
		t.Fatal("FollowerSlot")
	}
	if got := cfg.Sources(); len(got) != 1 || got[0] != "app" {
		t.Fatalf("Sources %v", got)
	}
	long := strings.Repeat("a", 55)
	if err := CheckDatabaseName(long); err != nil || len(FollowerSlot(long)) > 63 {
		t.Fatal("the longest follower name's slot must fit in 63 bytes")
	}
}

func TestFollowsIsImmutable(t *testing.T) {
	cfg := followerCluster()
	if err := cfg.Set("clusters.app_follower.follows", "x"); err == nil {
		t.Fatal("config set of follows was accepted")
	}
	updated := followerCluster()
	updated.Clusters[1].Follows = ""
	changed := ImmutableChanges(cfg, updated)
	found := false
	for _, k := range changed {
		found = found || k == "clusters.app_follower.follows"
	}
	if !found {
		t.Fatalf("config edit clearing follows is not refused: %v", changed)
	}
}

func TestNewFollowerEntryIsRefusedInConfigEdit(t *testing.T) {
	old := followerCluster()
	old.Clusters = old.Clusters[:1]
	updated := followerCluster()
	changed := ImmutableChanges(old, updated)
	if len(changed) != 1 || changed[0] != "clusters.app_follower.follows" {
		t.Fatalf("a hand-written follower entry is not refused: %v", changed)
	}
	plain := followerCluster()
	plain.Clusters[1].Follows = ""
	if changed := ImmutableChanges(old, plain); len(changed) != 0 {
		t.Fatalf("a new plain entry is refused: %v", changed)
	}
}

func TestFollowerSettingsAreReserved(t *testing.T) {
	for _, key := range []string{"primary_conninfo", "primary_slot_name", "hot_standby", "restore_command", "recovery_target_timeline"} {
		if !isReservedSetting(key) {
			t.Errorf("%s is not reserved", key)
		}
	}
	if isReservedSetting("hot_standby_feedback") {
		t.Error("hot_standby_feedback must stay an operator setting")
	}
}

func TestStanzasSkipFollowers(t *testing.T) {
	if got := followerCluster().Stanzas(); len(got) != 1 || got[0] != "app-3f9a1c" {
		t.Fatalf("Stanzas %v", got)
	}
}
