package steps

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/render"
)

// followerConfig is a server with app (large) and app_follower (small)
// following it.
func followerConfig() config.Cluster {
	cfg := metricsCluster("app")
	cfg.Server.DataRoot = "/srv/pg"
	cfg.Clusters[0].Size = "large"
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: "app_follower", Node: "db1", Port: 5434, Size: "small", Follows: "app"})
	return cfg
}

var followerHost = hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}

func floorMap(floors []render.Floor) map[string]string {
	out := map[string]string{}
	for _, f := range floors {
		out[f.Name] = f.Value
	}
	return out
}

func TestFollowerFloorsTakeTheLarger(t *testing.T) {
	cfg := followerConfig()
	app, _ := cfg.ClusterByName("app")
	f, _ := cfg.ClusterByName("app_follower")
	got := floorMap(FollowerFloors(cfg, f, app, followerHost))
	// large has 100 connections, small 30: the follower gets the source's.
	if got["max_connections"] != "100" {
		t.Errorf("max_connections %q, want the source's 100", got["max_connections"])
	}
	if got["max_wal_senders"] != "10" || got["max_prepared_transactions"] != "0" || got["max_locks_per_transaction"] != "64" {
		t.Errorf("floors %v", got)
	}
	// A source setting wins over its tuning, and the larger still wins.
	cfg.Clusters[0].Settings = map[string]string{"max_locks_per_transaction": "256"}
	cfg.Clusters[1].Settings = map[string]string{"max_prepared_transactions": "5"}
	app, _ = cfg.ClusterByName("app")
	f, _ = cfg.ClusterByName("app_follower")
	got = floorMap(FollowerFloors(cfg, f, app, followerHost))
	if got["max_locks_per_transaction"] != "256" || got["max_prepared_transactions"] != "5" {
		t.Errorf("settings: %v", got)
	}
}

func TestFollowerConfFile(t *testing.T) {
	cfg := followerConfig()
	f, _ := cfg.ClusterByName("app_follower")
	s, ok := followerSettings(cfg, f, followerHost, backup.Records{})
	if !ok {
		t.Fatal("no settings for a follower")
	}
	conf := string(render.FollowerConf(s))
	for _, want := range []string{
		"hot_standby = on",
		"primary_conninfo = 'host=/var/run/postgresql port=5433 user=postgres application_name=app_follower'",
		"primary_slot_name = 'vaire_app_follower'",
		"max_connections = 100",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("60-vaire-follower.conf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "restore_command") {
		t.Error("restore_command without backups")
	}
	cfg.Backup.Provider = "r2"
	cfg.Backup.Bucket = "b"
	s, _ = followerSettings(cfg, f, followerHost, backup.Records{Timelines: map[string]backup.TimelineSwitch{"app": {Stanza: "app-abc123", Timeline: "3"}}})
	conf = string(render.FollowerConf(s))
	if !strings.Contains(conf, `restore_command = '/usr/bin/pgbackrest --pg1-path=/srv/pg/app_follower --stanza=app-abc123 archive-get %f "%p"'`) {
		t.Errorf("restore_command:\n%s", conf)
	}
	if !strings.Contains(conf, "recovery_target_timeline = '3'") {
		t.Errorf("the recorded timeline is missing:\n%s", conf)
	}
}

func TestConfFilesHaveTheFollowerFileOnlyForFollowers(t *testing.T) {
	cfg := followerConfig()
	rt := engine.Runtime{Host: followerHost}
	has := func(name string) bool {
		spec, _ := cfg.ClusterByName(name)
		for _, f := range confFiles(cfg, spec, rt) {
			if strings.HasSuffix(f.Path, "/"+render.FollowerConfName) {
				return true
			}
		}
		return false
	}
	if !has("app_follower") || has("app") {
		t.Fatal("60-vaire-follower.conf must be a follower's only")
	}
}

func TestRolesSkipFollowers(t *testing.T) {
	cfg := followerConfig()
	if got := (postgresRoles{clusterStep{cluster: "app_follower"}}).SkipReason(cfg); got != "follower" {
		t.Errorf("follower: %q", got)
	}
	if got := (postgresRoles{clusterStep{cluster: "app"}}).SkipReason(cfg); got != "" {
		t.Errorf("source: %q", got)
	}
}

func TestFollowerRouting(t *testing.T) {
	cfg := followerConfig()
	for _, session := range []bool{false, true} {
		var found bool
		for _, db := range poolerDatabases(cfg, session) {
			if db.Name == "app_follower" {
				found = true
				if db.DBName != "app" || db.Port != 5434 {
					t.Errorf("entry %+v", db)
				}
			}
		}
		if !found {
			t.Errorf("session=%v: no entry for the follower", session)
		}
	}
	ini := string(render.PgbouncerIni(cfg, followerHost, poolerDatabases(cfg, false)))
	if !strings.Contains(ini, "app_follower = host=/var/run/postgresql port=5434 dbname=app auth_dbname=app_follower__auth") {
		t.Errorf("pgbouncer.ini:\n%s", ini)
	}
	if !strings.Contains(ini, "auth_dbname = app__auth") {
		t.Errorf("the admin auth database must be a primary's:\n%s", ini)
	}
}

func TestFollowersHaveNoStanzaOrTimers(t *testing.T) {
	cfg := followerConfig()
	cfg.Backup.Provider = "r2"
	cfg.Backup.Bucket = "b"
	conf := string(render.PgBackRestConf(cfg, followerHost, render.BackupSecrets{}))
	if strings.Contains(conf, "app_follower") || !strings.Contains(conf, "[app-abc123]") {
		t.Errorf("pgbackrest.conf:\n%s", conf)
	}
	for name := range timerFiles(cfg) {
		if strings.Contains(name, "app_follower") {
			t.Errorf("a timer for the follower: %s", name)
		}
	}
	f, _ := cfg.ClusterByName("app_follower")
	if !strings.Contains(string(render.BackupConf(cfg, f)), "archive_command = '/bin/true'") {
		t.Error("a follower archives nowhere")
	}
}

func TestFollowerProbeOpensTheSourceDatabase(t *testing.T) {
	cfg := followerConfig()
	f, _ := cfg.ClusterByName("app_follower")
	if target := render.ProbeTarget(PGSocketDir, f); !strings.Contains(target, "/app?") {
		t.Errorf("probe target %s", target)
	}
}
