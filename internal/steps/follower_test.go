package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
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

func TestPgbouncerWithholdsAnUnsettledUnfollowsAlias(t *testing.T) {
	old := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = old })
	cfg := followerConfig()
	setRecord := func(settled bool) {
		t.Helper()
		if err := backup.UpdateRecords(func(s *backup.Records) error {
			s.Unfollowing["app_follower"] = backup.Unfollow{
				Source: "app", Stanza: "app_follower-abc123", StartedAt: "2026-11-10T00:00:00Z", Settled: settled,
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries := func(session bool) map[string]render.PoolerDatabase {
		recs, err := backup.LoadRecords()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]render.PoolerDatabase{}
		for _, db := range poolerDatabases(cfg, recs, session) {
			out[db.Name] = db
		}
		return out
	}
	rendered := func(session bool) string {
		recs, err := backup.LoadRecords()
		if err != nil {
			t.Fatal(err)
		}
		if session {
			return string(render.SessionIni(cfg, poolerDatabases(cfg, recs, true), SessionSocketDir))
		}
		return string(render.PgbouncerIni(cfg, followerHost, poolerDatabases(cfg, recs, false)))
	}

	setRecord(false)
	for _, session := range []bool{false, true} {
		got := entries(session)
		if _, ok := got["app_follower"]; ok {
			t.Errorf("session=%v: an unsettled unfollow's alias is rendered", session)
		}
		if _, ok := got["app"]; !ok {
			t.Errorf("session=%v: the source's alias is missing", session)
		}
		if ini := rendered(session); strings.Contains(ini, "app_follower") {
			t.Errorf("session=%v: the rendered configuration names the unsettled alias:\n%s", session, ini)
		}
	}

	setRecord(true)
	for _, session := range []bool{false, true} {
		got := entries(session)
		db, ok := got["app_follower"]
		if !ok || db.DBName != "app" || db.Port != 5434 {
			t.Errorf("session=%v: the settled alias is not routed to the follower: %+v", session, db)
		}
		if ini := rendered(session); !strings.Contains(ini, "app_follower") {
			t.Errorf("session=%v: a settled alias is still withheld:\n%s", session, ini)
		}
	}
}

// AF-A1: the direct unfollow fence is a full-configuration entry point, so it
// applies the same routing lifecycle eligibility as general apply. An awaiting
// recovery database and an unrelated unfinished ordinary follow keep both their
// main and __auth aliases out of both poolers, while the ordinary source stays
// routable and the unsettled unfollow target is still withheld. A missing
// records file remains the legitimate empty initial state for a fresh boot.
func TestUnfollowFenceKeepsOtherUnfinishedAliasesExcluded(t *testing.T) {
	oldPath := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = oldPath })

	cfg := followerConfig()
	for i := range cfg.Clusters {
		if cfg.Clusters[i].StatementTimeout == "" {
			cfg.Clusters[i].StatementTimeout = "60s"
		}
	}
	generation := "0123456789abcdef0123456789abcdef"
	cfg.Clusters = append(cfg.Clusters,
		config.ClusterSpec{Name: "app_pending", Node: "db1", Port: 5435, Size: "small", StatementTimeout: "60s", Follows: "app", FollowGeneration: generation},
		config.ClusterSpec{Name: "app_restored", Node: "db1", Port: 5436, Size: "small", StatementTimeout: "60s", Follows: "app"},
	)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	oldWrite := writePoolerFile
	t.Cleanup(func() { writePoolerFile = oldWrite })
	written := map[string]string{}
	writePoolerFile = func(path, content string, _ os.FileMode, _, _ string) error {
		written[path] = content
		return nil
	}
	fence := func() {
		t.Helper()
		// The dry shell refuses the reload after the files are written; the
		// rendered content is this boundary's concern, as in the fail-closed
		// test above.
		_ = ApplyPoolerFence(context.Background(), cfg, engine.Runtime{Host: followerHost, Shell: engine.Shell{DryRun: true}}, nil)
	}
	assertAlias := func(db string, want bool) {
		t.Helper()
		for _, path := range []string{poolerFilePaths.ini, poolerFilePaths.sessionIni} {
			content, ok := written[path]
			if !ok {
				t.Fatalf("the direct fence did not render %s", path)
			}
			for _, alias := range []string{db, db + "__auth"} {
				if got := strings.Contains(content, alias+" ="); got != want {
					t.Errorf("%s: alias %s present = %v, want %v", filepath.Base(path), alias, got, want)
				}
			}
		}
	}

	// No records file: an ordinary follower is still rendered (fresh boot).
	fence()
	assertAlias("app", true)
	assertAlias("app_follower", true)

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Unfollowing["app_follower"] = backup.Unfollow{
			Source: "app", Stanza: "app_follower-abc123", StartedAt: "2026-11-10T00:00:00Z", Settled: false,
		}
		s.AwaitingRecovery["app_restored"] = backup.Awaiting{
			Stanza: "app_restored-abc123", Since: "2026-11-10T00:00:00Z",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	written = map[string]string{}
	fence()
	assertAlias("app", true)
	assertAlias("app_follower", false) // the unsettled unfollow target
	assertAlias("app_pending", false)  // an unrelated generated unfinished follow
	assertAlias("app_restored", false) // awaiting recovery
}

// C2-A1/B2-01: the pooler render reads the transition records once. A missing
// records file is the legitimate empty initial state; corrupt or unreadable
// records are unknown transition state and fail closed before either pooler's
// file is overwritten or a service is reloaded. The direct fence path used by
// db unfollow has the same boundary.
func TestPgbouncerRenderingFailsClosedOnUnreadUnfollowState(t *testing.T) {
	oldPath := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = oldPath })
	cfg := followerConfig()
	rt := engine.Runtime{Host: followerHost}
	oldPaths := poolerFilePaths
	dir := t.TempDir()
	poolerFilePaths = struct {
		ini, hba, sessionIni, sessionHBA, sessionUnit string
	}{
		ini: filepath.Join(dir, "pgbouncer.ini"), hba: filepath.Join(dir, "pgbouncer_hba.conf"),
		sessionIni: filepath.Join(dir, "session.ini"), sessionHBA: filepath.Join(dir, "session_hba.conf"),
		sessionUnit: filepath.Join(dir, "session.service"),
	}
	t.Cleanup(func() { poolerFilePaths = oldPaths })
	safe := map[string]string{
		poolerFilePaths.ini: "safe pgbouncer.ini\n", poolerFilePaths.hba: "safe hba\n",
		poolerFilePaths.sessionIni: "safe session.ini\n", poolerFilePaths.sessionHBA: "safe session hba\n",
		poolerFilePaths.sessionUnit: "safe unit\n",
	}
	for path, content := range safe {
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil { //nolint:gosec // G306: test-managed file.
			t.Fatal(err)
		}
	}

	// No records file: initial bootstrap, so a follower alias renders normally.
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		t.Fatalf("missing records are refused: %v", err)
	}
	assertPoolerAlias(t, files, "app_follower", true)
	assertPoolerAlias(t, files, "app", true)

	// A readable unsettled unfollow withholds the alias and its auth alias from
	// both poolers, on both the ordinary render and the direct fence path.
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Unfollowing["app_follower"] = backup.Unfollow{
			Source: "app", Stanza: "app_follower-abc123", StartedAt: "2026-11-10T00:00:00Z", Settled: false,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	files, err = pgbouncerFiles(cfg, rt)
	if err != nil {
		t.Fatal(err)
	}
	assertPoolerAlias(t, files, "app_follower", false)
	assertPoolerAlias(t, files, "app", true)

	oldWrite := writePoolerFile
	t.Cleanup(func() { writePoolerFile = oldWrite })
	var written map[string]string
	writePoolerFile = func(path, content string, _ os.FileMode, _, _ string) error {
		if written == nil {
			written = map[string]string{}
		}
		written[path] = content
		return nil
	}
	// The shell is dry, so the call stops after the writes; the rendered files
	// are what this boundary protects.
	_ = ApplyPoolerFence(context.Background(), cfg, engine.Runtime{Host: followerHost, Shell: engine.Shell{DryRun: true}}, nil)
	for _, path := range []string{poolerFilePaths.ini, poolerFilePaths.sessionIni} {
		content, ok := written[path]
		if !ok {
			t.Fatalf("the fence did not render %s", path)
		}
		if strings.Contains(content, "app_follower =") || strings.Contains(content, "app_follower__auth =") {
			t.Fatalf("the fence exposed the unsettled alias in %s:\n%s", path, content)
		}
	}

	// Corrupt records are unknown, never "no unfinished unfollow".
	if err := os.WriteFile(backup.RecordsPath, []byte("{"), 0o600); err != nil { //nolint:gosec // G306: test records file.
		t.Fatal(err)
	}
	if files := (pgbouncer{}).RenderedFiles(cfg, rt); files != nil {
		t.Fatalf("the plan render exposed files from unreadable records: %v", files)
	}
	for name, call := range map[string]func() error{
		"render": func() error { _, err := pgbouncerFiles(cfg, rt); return err },
		"inputs": func() error { _, err := (pgbouncer{}).Inputs(cfg, rt); return err },
		"check": func() error {
			_, err := (pgbouncer{}).Check(context.Background(), cfg, rt)
			return err
		},
		"apply": func() error {
			_, err := (pgbouncer{}).Apply(context.Background(), cfg, engine.Runtime{Host: followerHost, Shell: engine.Shell{DryRun: true}}, nil)
			return err
		},
		"direct fence": func() error {
			return ApplyPoolerFence(context.Background(), cfg, engine.Runtime{Host: followerHost, Shell: engine.Shell{DryRun: true}}, nil)
		},
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s accepted unreadable unfollow records", name)
		}
		var cliErr *output.CLIError
		if !errors.As(err, &cliErr) || !strings.Contains(fmt.Sprint(cliErr.Details), "unfollow_state_unknown") {
			t.Fatalf("%s error is not a fail-closed transition refusal: %v", name, err)
		}
	}

	// No file was touched by any failing path: the previous safe files stay.
	writePoolerFile = oldWrite
	if err := ApplyPoolerFence(context.Background(), cfg, engine.Runtime{Host: followerHost, Shell: engine.Shell{DryRun: true}}, nil); err == nil || refusalReason(err) != "unfollow_state_unknown" {
		t.Fatalf("the direct fence accepted unreadable unfollow records: %v", err)
	}
	for path, want := range safe {
		got, err := os.ReadFile(path) //nolint:gosec // G304: test-managed path.
		if err != nil {
			t.Fatalf("safe file %s is gone: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("unreadable records overwrote %s: %q", path, got)
		}
	}
}

// refusalReason is an output error's details.reason.
func refusalReason(err error) string {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return ""
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason
}

// assertPoolerAlias checks both poolers' rendered files for db's alias and its
// __auth alias.
func assertPoolerAlias(t *testing.T, files []pgbouncerFile, db string, want bool) {
	t.Helper()
	for _, f := range files {
		if f.path != poolerFilePaths.ini && f.path != poolerFilePaths.sessionIni {
			continue
		}
		for _, alias := range []string{db + " =", db + "__auth ="} {
			if got := strings.Contains(f.content, alias); got != want {
				t.Errorf("%s: alias %q present=%v, want %v:\n%s", f.path, alias, got, want, f.content)
			}
		}
	}
}

func TestFencePoolerDatabasePausesAndKillsBothPoolers(t *testing.T) {
	oldNames, oldAdmin := poolerDatabaseNames, poolerAdminAt
	t.Cleanup(func() { poolerDatabaseNames, poolerAdminAt = oldNames, oldAdmin })
	poolerDatabaseNames = func(context.Context, string, int) (map[string]bool, error) {
		return map[string]bool{"app_follower": true}, nil
	}
	var got []string
	poolerAdminAt = func(_ context.Context, _ string, port int, command string) error {
		got = append(got, fmt.Sprintf("%d %s", port, command))
		return nil
	}
	resume, err := FencePoolerDatabase(context.Background(), "app_follower")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{
		"6432 RESUME app_follower", "6432 KILL app_follower", "6432 PAUSE app_follower",
		"5432 RESUME app_follower", "5432 KILL app_follower", "5432 PAUSE app_follower",
	}; !slices.Equal(got, want) {
		t.Fatalf("fence commands = %v, want %v", got, want)
	}
	resume()
	if want := []string{"6432 RESUME app_follower", "5432 RESUME app_follower"}; !slices.Equal(got[6:], want) {
		t.Fatalf("resume commands = %v, want %v", got[6:], want)
	}
}

func TestFencePoolerDatabaseWithAnAliasAlreadyWithheld(t *testing.T) {
	oldNames, oldAdmin := poolerDatabaseNames, poolerAdminAt
	t.Cleanup(func() { poolerDatabaseNames, poolerAdminAt = oldNames, oldAdmin })
	poolerDatabaseNames = func(context.Context, string, int) (map[string]bool, error) {
		return map[string]bool{"app": true}, nil
	}
	var got []string
	poolerAdminAt = func(_ context.Context, _ string, port int, command string) error {
		got = append(got, fmt.Sprintf("%d %s", port, command))
		return nil
	}
	resume, err := FencePoolerDatabase(context.Background(), "app_follower")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a withheld alias was paused or killed: %v", got)
	}
	resume()
}

func TestFencePoolerDatabaseRefusesAFailedPause(t *testing.T) {
	oldNames, oldAdmin := poolerDatabaseNames, poolerAdminAt
	t.Cleanup(func() { poolerDatabaseNames, poolerAdminAt = oldNames, oldAdmin })
	poolerDatabaseNames = func(context.Context, string, int) (map[string]bool, error) {
		return map[string]bool{"app_follower": true}, nil
	}
	var resumed, pauseFailed bool
	poolerAdminAt = func(_ context.Context, _ string, _ int, command string) error {
		if command == "RESUME app_follower" && pauseFailed {
			resumed = true
			return nil
		}
		if command == "PAUSE app_follower" {
			pauseFailed = true
			return errors.New("the pooler did not answer")
		}
		return nil
	}
	if _, err := FencePoolerDatabase(context.Background(), "app_follower"); err == nil {
		t.Fatal("a failed pause did not refuse the fence")
	}
	if !resumed {
		t.Fatal("a failed fence left the pooler paused")
	}
}

func TestFollowerRouting(t *testing.T) {
	cfg := followerConfig()
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []bool{false, true} {
		var found bool
		for _, db := range poolerDatabases(cfg, recs, session) {
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
	ini := string(render.PgbouncerIni(cfg, followerHost, poolerDatabases(cfg, recs, false)))
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

// Admission reads the source's running floors from pg_settings: every floor
// must be present and a valid PostgreSQL integer, and an unknown row or value
// fails closed instead of becoming a default.
func TestRunningFloorsOf(t *testing.T) {
	rows := func(value string) []map[string]string {
		out := make([]map[string]string, 0, len(FloorSettings))
		for _, name := range FloorSettings {
			out = append(out, map[string]string{"name": name, "setting": value})
		}
		return out
	}
	got, err := runningFloorsOf("app", rows("100"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range FloorSettings {
		if got[name] != 100 {
			t.Errorf("%s = %d, want 100", name, got[name])
		}
	}
	if _, err := runningFloorsOf("app", rows("not a number")); err == nil {
		t.Error("a malformed running floor was accepted")
	}
	missing := rows("100")
	missing = missing[1:]
	if _, err := runningFloorsOf("app", missing); err == nil {
		t.Error("a missing running floor was accepted")
	}
}
