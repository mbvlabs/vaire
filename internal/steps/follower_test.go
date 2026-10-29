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

func floorMap(t *testing.T, floors []render.Floor, err error) map[string]string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
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
	floors, err := FollowerFloors(cfg, f, app, followerHost)
	got := floorMap(t, floors, err)
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
	floors, err = FollowerFloors(cfg, f, app, followerHost)
	got = floorMap(t, floors, err)
	if got["max_locks_per_transaction"] != "256" || got["max_prepared_transactions"] != "5" {
		t.Errorf("settings: %v", got)
	}
}

func TestFollowerConfFile(t *testing.T) {
	cfg := followerConfig()
	f, _ := cfg.ClusterByName("app_follower")
	s, ok, err := followerSettings(cfg, f, followerHost, backup.Records{})
	if err != nil || !ok {
		t.Fatalf("no settings for a follower: %v", err)
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
	s, _, _ = followerSettings(cfg, f, followerHost, backup.Records{Timelines: map[string]backup.TimelineSwitch{"app": {Stanza: "app-abc123", Timeline: "3"}}})
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
		files, err := confFiles(cfg, spec, rt)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
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

func TestFloorValueReadsPostgreSQLSyntax(t *testing.T) {
	// PostgreSQL 18's parse_int for a setting without a unit: strtol base 0
	// (space, sign, 0x hexadecimal, leading-0 octal or decimal), the whole
	// value with strtod when that stops at '.', 'e' or 'E' or overflows, then
	// rint (ties to even), trailing space only, and a 32-bit range.
	accepted := []struct {
		in   string
		want int
	}{
		{"100", 100},
		{"+100", 100},
		{" 100 ", 100},
		{"0x100", 256},
		{"0x1A", 26},
		{"-0x10", -16},
		{"0100", 64},
		{"010", 8},
		{"0", 0},
		{"100.4", 100},
		{"100.6", 101},
		{"100.5", 100}, // rint ties to even
		{"101.5", 102},
		{"0.6", 1},
		{"010.5", 10}, // octal first, then the whole value as decimal 10.5
		{"0e2", 0},
		{"0e-400", 0}, // a genuine zero, not an underflow
		{"1e2", 100},
		{"0x10.5", 16}, // strtol stops at '.', strtod reads a hex float
		{".5", 0},      // strtol reads nothing, strtod starts at '.'
		{"-0.5", 0},
		{"2147483647", 2147483647},
		{"-2147483648", -2147483648},
		{"2147483647.4", 2147483647},
		{"-2147483648.4", -2147483648},
	}
	for _, setting := range FloorSettings {
		for _, tc := range accepted {
			got, err := floorValue(setting, tc.in)
			if err != nil || got != tc.want {
				t.Errorf("floorValue(%s, %q) = %d, %v; want %d", setting, tc.in, got, err, tc.want)
			}
		}
	}
	// A unit, trailing garbage, an unterminated exponent, NaN, a value outside
	// the 32-bit integer range and a float strtod underflows are not
	// PostgreSQL integers.
	rejected := []string{"", "abc", "0x", "08", "0x1p3", "0-5", "0x-5", "1MB", "30 s", "100MB", "1_000",
		"5000000000", "2147483648", "2147483647.5", "1e", "1e400", "1e-400", "-1e-400", "1e-310", "1.2.3", ".", "-.5", "NaN", "Inf", "Infinity"}
	for _, setting := range FloorSettings {
		for _, in := range rejected {
			if got, err := floorValue(setting, in); err == nil {
				t.Errorf("floorValue(%s, %q) = %d, want an error", setting, in, got)
			} else if !strings.Contains(err.Error(), setting) {
				t.Errorf("floorValue(%s, %q) error %q does not name the setting", setting, in, err)
			}
		}
	}
}

func TestFollowerFloorsReadPostgreSQLNumbers(t *testing.T) {
	cfg := followerConfig()
	// The source's hexadecimal max_connections is 256, above the follower's
	// 30: the floor must be the source's 256, not a dropped setting.
	cfg.Clusters[0].Settings = map[string]string{"max_connections": "0x100"}
	app, _ := cfg.ClusterByName("app")
	f, _ := cfg.ClusterByName("app_follower")
	floors, err := FollowerFloors(cfg, f, app, followerHost)
	if err != nil {
		t.Fatal(err)
	}
	if got := floorMap(t, floors, nil)["max_connections"]; got != "256" {
		t.Errorf("max_connections %q, want the source's hexadecimal 256", got)
	}
	// A follower's own hexadecimal or fractional override wins when larger.
	cfg.Clusters[0].Settings = map[string]string{"max_connections": "100", "max_prepared_transactions": "5.6"}
	cfg.Clusters[1].Settings = map[string]string{"max_locks_per_transaction": "0x200"}
	app, _ = cfg.ClusterByName("app")
	f, _ = cfg.ClusterByName("app_follower")
	floors, err = FollowerFloors(cfg, f, app, followerHost)
	if err != nil {
		t.Fatal(err)
	}
	got := floorMap(t, floors, nil)
	if got["max_locks_per_transaction"] != "512" || got["max_prepared_transactions"] != "6" {
		t.Errorf("floors %v, want the follower's 512 locks and the source's rounded 6", got)
	}
}

func TestFollowerFloorsFailOnUnreadableValues(t *testing.T) {
	cfg := followerConfig()
	cfg.Clusters[0].Settings = map[string]string{"max_wal_senders": "ten"}
	app, _ := cfg.ClusterByName("app")
	f, _ := cfg.ClusterByName("app_follower")
	_, err := FollowerFloors(cfg, f, app, followerHost)
	if err == nil || !strings.Contains(err.Error(), "max_wal_senders") || !strings.Contains(err.Error(), "app") {
		t.Fatalf("unreadable source value: %v, want an error naming max_wal_senders and app", err)
	}
	// The step renders nothing at all: a floor vaire cannot read fails it.
	rt := engine.Runtime{Host: followerHost}
	if _, err := confFiles(cfg, f, rt); err == nil {
		t.Fatal("confFiles accepted an unreadable floor value")
	}
	if _, err := (postgresConfig{clusterStep{cluster: "app_follower"}}).Inputs(cfg, rt); err == nil {
		t.Fatal("postgres-config-v1 accepted the source's unreadable floor value")
	}
	cfg.Clusters[0].Settings = nil
	cfg.Clusters[1].Settings = map[string]string{"max_connections": "1MB"}
	app, _ = cfg.ClusterByName("app")
	f, _ = cfg.ClusterByName("app_follower")
	if _, err := FollowerFloors(cfg, f, app, followerHost); err == nil || !strings.Contains(err.Error(), "app_follower") {
		t.Fatalf("unreadable follower value: %v, want an error naming app_follower", err)
	}
}

func TestFollowerFloorDrops(t *testing.T) {
	cfg := followerConfig()
	small := cfg.Clusters[0]
	small.Size = "small"
	large := small
	large.Size = "large"
	// A source that grows lowers nothing.
	drops, err := FollowerFloorDrops(cfg, "app", small, large, followerHost)
	if err != nil || len(drops) != 0 {
		t.Fatalf("small to large: drops %v, %v; want none", drops, err)
	}
	// A source that shrinks lowers the follower's floors.
	drops, err = FollowerFloorDrops(cfg, "app", large, small, followerHost)
	if err != nil || len(drops) == 0 {
		t.Fatalf("large to small: drops %v, %v; want some", drops, err)
	}
	found := false
	for _, d := range drops {
		if d.Follower == "app_follower" && d.Setting == "max_connections" && d.From == 100 && d.To == 30 {
			found = true
		}
	}
	if !found {
		t.Errorf("drops %v lack app_follower max_connections 100 -> 30", drops)
	}
	// A change that mixes a rise and a fall still falls: every floor counts.
	was := large
	was.Settings = map[string]string{"max_connections": "200"}
	now := large
	now.Settings = map[string]string{"max_connections": "100", "max_locks_per_transaction": "512"}
	drops, err = FollowerFloorDrops(cfg, "app", was, now, followerHost)
	if err != nil {
		t.Fatal(err)
	}
	var fell, rose bool
	for _, d := range drops {
		fell = fell || d.Setting == "max_connections"
		rose = rose || d.Setting == "max_locks_per_transaction"
	}
	if !fell || rose {
		t.Errorf("mixed change: drops %v, want max_connections only", drops)
	}
	// A database without followers has no floor to refuse.
	if drops, err := FollowerFloorDrops(cfg, "app_follower", large, small, followerHost); err != nil || len(drops) != 0 {
		t.Fatalf("no followers: drops %v, %v; want none", drops, err)
	}
	// An unreadable floor value fails the resize instead of being skipped.
	bad := large
	bad.Settings = map[string]string{"max_locks_per_transaction": "lots"}
	if _, err := FollowerFloorDrops(cfg, "app", small, bad, followerHost); err == nil {
		t.Fatal("FollowerFloorDrops accepted an unreadable value")
	}
}

func TestFollowerFloorsChange(t *testing.T) {
	cfg := followerConfig()
	small := cfg.Clusters[0]
	small.Size = "small"
	large := small
	large.Size = "large"
	f, _ := cfg.ClusterByName("app_follower")
	// A source value that stays below the follower's own leaves its floor,
	// and so its restart, alone.
	low, lower := small, small
	low.Settings = map[string]string{"max_connections": "20"}
	lower.Settings = map[string]string{"max_connections": "10"}
	if changed, err := FollowerFloorsChange(cfg, f, low, lower, followerHost); err != nil || changed {
		t.Fatalf("below the follower's own value: change %v, %v; want false", changed, err)
	}
	if changed, err := FollowerFloorsChange(cfg, f, small, large, followerHost); err != nil || !changed {
		t.Fatalf("small to large: change %v, %v; want true", changed, err)
	}
}

// A floor whose value differs between the file on disk and the rendered one
// needs the follower's restart; a changed reason alone, or a change outside
// the floors, does not (R1-3, a retried resize).
func TestFloorsDiffer(t *testing.T) {
	conf := func(connections, reason, slot string) []byte {
		return render.FollowerConf(render.FollowerSettings{
			Name: "app_follower", Source: "app", Conninfo: "host=/run/postgresql", Slot: slot,
			Floors: []render.Floor{
				{Name: "max_connections", Value: connections, Reason: reason},
				{Name: "max_wal_senders", Value: "10", Reason: "its own"},
			},
		})
	}
	path := FollowerConfPath("app_follower")
	cases := []struct {
		name      string
		have, now []byte
		want      bool
	}{
		{"raised floor", conf("50", "its own", "s"), conf("100", "app's, above its own 50", "s"), true},
		{"reason only", conf("100", "its own; app runs 50", "s"), conf("100", "its own; app runs 80", "s"), false},
		{"slot only", conf("100", "its own", "s"), conf("100", "its own", "t"), false},
		{"no file yet", nil, conf("100", "its own", "s"), true},
	}
	for _, tc := range cases {
		if got := FloorsDiffer(render.DiffBytes(path, tc.have, tc.now)); got != tc.want {
			t.Errorf("%s: FloorsDiffer = %v, want %v", tc.name, got, tc.want)
		}
	}
}
