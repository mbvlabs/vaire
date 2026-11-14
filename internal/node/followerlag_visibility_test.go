package node

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/doctor"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/health"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/promfile"
	"github.com/mbvlabs/vaire/internal/steps"
)

func int64p(v int64) *int64       { return &v }
func float64p(v float64) *float64 { return &v }

// followerQueryPG serves the source and self queries separately, so a test
// can fail one side only.
type followerQueryPG struct {
	sourceRows []map[string]string
	sourceErr  error
	selfRows   []map[string]string
	selfErr    error
}

func (f *followerQueryPG) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	if strings.Contains(sql, "pg_stat_replication") || strings.Contains(sql, "pg_replication_slots") {
		return f.sourceRows, f.sourceErr
	}
	return f.selfRows, f.selfErr
}

func (f *followerQueryPG) Exec(context.Context, string, string, map[string]string) error { return nil }

// R4-1: a failed side keeps its own error and stays unknown; it does not
// borrow the other side's answer.
func TestReadFollowerTracksSidesIndependently(t *testing.T) {
	oldRunning, oldPG := clusterRunning, followerPG
	t.Cleanup(func() { clusterRunning, followerPG = oldRunning, oldPG })
	clusterRunning = func(config.ClusterSpec) bool { return true }

	cfg := followerCfg()
	src, follower := cfg.Clusters[0], cfg.Clusters[1]
	srcRow := map[string]string{"sender_state": "streaming", "lag_bytes": "0", "replay_lag": "",
		"slot_active": "true", "slot_retained": "0", "slot_safe": ""}
	selfRow := map[string]string{"in_recovery": "true", "pause_state": "not paused"}

	followerPG = func(config.ClusterSpec) sqlRunner {
		return &followerQueryPG{sourceRows: []map[string]string{srcRow}, selfErr: errors.New("connection reset")}
	}
	st := readFollower(context.Background(), follower, src)
	if !st.SourceKnown || st.SelfKnown || !st.Streaming || st.SelfError == "" || st.SourceError != "" {
		t.Fatalf("source read, self failed: %+v", st)
	}

	followerPG = func(config.ClusterSpec) sqlRunner {
		return &followerQueryPG{sourceErr: errors.New("statement timeout"), selfRows: []map[string]string{selfRow}}
	}
	st = readFollower(context.Background(), follower, src)
	if st.SourceKnown || !st.SelfKnown || !st.InRecovery || st.SourceError == "" || st.SelfError != "" {
		t.Fatalf("source failed, self read: %+v", st)
	}
}

// R4-5: a failed slot enumeration is returned, not silently read as no
// orphan slots.
func TestOrphanSlotsReportsQueryFailure(t *testing.T) {
	oldRunning, oldPG := clusterRunning, followerPG
	t.Cleanup(func() { clusterRunning, followerPG = oldRunning, oldPG })
	clusterRunning = func(config.ClusterSpec) bool { return true }
	followerPG = func(config.ClusterSpec) sqlRunner {
		return &followerQueryPG{sourceErr: errors.New("canceling statement due to statement timeout")}
	}

	slots, failures := orphanSlots(context.Background(), followerCfg())
	if len(slots) != 0 || len(failures) != 1 || failures[0].Source != "app" ||
		failures[0].Error != "canceling statement due to statement timeout" {
		t.Fatalf("slots %v, failures %v", slots, failures)
	}
}

// diagnosticsFollowerPG serves one configured follower's source and self
// queries from fixed rows, so the production status/doctor/metrics gathering
// can run without a PostgreSQL server.
type diagnosticsFollowerPG struct {
	name    string
	sources map[string]map[string]string
	self    map[string]map[string]string
}

func (d *diagnosticsFollowerPG) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	switch {
	case strings.Contains(sql, "FROM pg_stat_replication"):
		for follower, row := range d.sources {
			if strings.Contains(sql, "'"+follower+"'") {
				return []map[string]string{row}, nil
			}
		}
		return nil, errors.New("no replication row")
	case strings.Contains(sql, "pg_is_in_recovery"):
		if row, ok := d.self[d.name]; ok {
			return []map[string]string{row}, nil
		}
		return nil, errors.New("connection refused")
	case strings.Contains(sql, "FROM pg_replication_slots"):
		return nil, nil
	}
	return nil, errors.New("unexpected query")
}

func (d *diagnosticsFollowerPG) Exec(context.Context, string, string, map[string]string) error {
	return nil
}

// diagnosticsClusterPG makes status and doctor see a running cluster without
// a PostgreSQL server; the sample's fields are irrelevant to follower facts.
type diagnosticsClusterPG struct{}

func (diagnosticsClusterPG) Query(context.Context, string, string, map[string]string) ([]map[string]string, error) {
	return []map[string]string{{}}, nil
}

func (diagnosticsClusterPG) Exec(context.Context, string, string, map[string]string) error {
	return nil
}

// B3-01: a configured follower whose ordinary follow has not completed but
// whose cluster materialised is not a convergence target, yet its diagnostics
// stay measurable. The production status gather, doctor gather and metrics
// write all accept it through the read-only view: a stopped follower keeps its
// source slot pressure, a running one keeps its paused/lost facts, an unknown
// self side is omitted, and an awaiting-recovery database stays out. General
// plan/apply still excludes the unfinished followers from every step.
func TestUnfinishedMaterialisedFollowerRetainsDiagnostics(t *testing.T) {
	oldRecords, oldRunning, oldPG, oldSnapshotPG, oldProm := backup.RecordsPath, clusterRunning, followerPG, snapshotPG, writeFollowerProm
	oldCPU := statusCPURead
	oldPools := statusPoolRead
	oldHost := statusHostRead
	oldServices, oldProbe, oldDrift, oldOverrides, oldHBA := doctorServiceRead, doctorProbeRead, doctorDriftRead, doctorOverridesRead, doctorWeakHBARead
	oldActivity := activityStoreFactory
	t.Cleanup(func() {
		backup.RecordsPath, clusterRunning, followerPG, snapshotPG, writeFollowerProm = oldRecords, oldRunning, oldPG, oldSnapshotPG, oldProm
		statusCPURead = oldCPU
		statusPoolRead = oldPools
		statusHostRead = oldHost
		doctorServiceRead, doctorProbeRead, doctorDriftRead, doctorOverridesRead, doctorWeakHBARead = oldServices, oldProbe, oldDrift, oldOverrides, oldHBA
		activityStoreFactory = oldActivity
	})
	poolReads := 0
	statusPoolRead = func(context.Context) map[string]health.Pools {
		poolReads++
		return map[string]health.Pools{"transaction": {}}
	}
	statusHostRead = func(string) (hostinfo.Info, error) {
		return hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}, nil
	}
	statusCPURead = func(context.Context, config.ClusterSpec) (hostinfo.CPUObservation, string, *int64) {
		return hostinfo.CPUObservation{Reason: "test_unavailable"}, "", nil
	}
	doctorServiceRead = func(context.Context, string) bool { return false }
	doctorProbeRead = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("test probe unavailable")
	}
	doctorDriftRead = func(context.Context, config.Cluster, engine.Runtime, string) string { return "" }
	doctorOverridesRead = func(string) []string { return nil }
	var hbaReads []string
	doctorWeakHBARead = func(_ context.Context, spec config.ClusterSpec) []string {
		hbaReads = append(hbaReads, spec.Name)
		return nil
	}
	activityStoreFactory = func() (activityStore, error) { return &fakeActivityStore{}, nil }
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")

	cfg := followerLifecycleCfg(t, "app_stopped", "app_paused")
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{
		Name: "app_awaiting", Node: "db1", Port: 5436, Size: "small", StatementTimeout: "60s", Follows: "app",
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// The domain is only needed for validation; clearing it keeps gatherFacts
	// from resolving a test hostname. Metrics are off so its exporter probes
	// are not part of this diagnostics test.
	cfg.Domain, cfg.Metrics.Enabled = "", false
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery["app_awaiting"] = backup.Awaiting{
			Stanza: "app_awaiting-abc123", Since: "2026-11-10T00:00:00Z",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sourceRows := map[string]map[string]string{
		"app_stopped": {
			"sender_state": "", "lag_bytes": "", "replay_lag": "",
			"slot_active": "false", "slot_wal_status": "extended", "slot_retained": "104857600",
			"slot_safe": "20971520", "slot_uncapped": "false",
		},
		"app_paused": {
			"sender_state": "streaming", "lag_bytes": "4096", "replay_lag": "120",
			"slot_active": "false", "slot_wal_status": "lost", "slot_retained": "0",
			"slot_safe": "0", "slot_uncapped": "false",
		},
	}
	selfRows := map[string]map[string]string{
		"app_paused": {"in_recovery": "true", "pause_state": "paused"},
	}
	followerPG = func(spec config.ClusterSpec) sqlRunner {
		return &diagnosticsFollowerPG{name: spec.Name, sources: sourceRows, self: selfRows}
	}
	snapshotPG = func(config.ClusterSpec) sqlRunner { return diagnosticsClusterPG{} }
	clusterRunning = func(spec config.ClusterSpec) bool {
		return spec.Name == "app" || spec.Name == "app_paused"
	}

	res, err := handleStatus(context.Background(), &Env{Req: request("status", nil), Cluster: cfg})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	data := res.Data.(statusData)
	if poolReads != 1 || len(data.Poolers) != 1 {
		t.Fatalf("status did not use the fake pooler snapshot: reads=%d pools=%v", poolReads, data.Poolers)
	}
	rows := map[string]statusDatabase{}
	for _, row := range data.Databases {
		rows[row.Name] = row
	}
	stopped := rows["app_stopped"]
	if stopped.Follower == nil {
		t.Fatal("status dropped the stopped unfinished follower's diagnostics")
	}
	if !stopped.Follower.SourceKnown || stopped.Follower.SelfKnown {
		t.Fatalf("stopped follower sides: %+v", stopped.Follower)
	}
	if stopped.Follower.SlotWALStatus != "extended" || stopped.Follower.SlotRetainedBytes == nil ||
		*stopped.Follower.SlotRetainedBytes != 104857600 || stopped.Follower.SlotSafeBytes == nil ||
		*stopped.Follower.SlotSafeBytes != 20971520 {
		t.Fatalf("stopped follower lost its source slot pressure: %+v", stopped.Follower)
	}
	raw, err := json.Marshal(stopped.Follower)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{`"in_recovery"`, `"replay_paused"`} {
		if strings.Contains(string(raw), omitted) {
			t.Errorf("unknown self side published %s: %s", omitted, raw)
		}
	}
	paused := rows["app_paused"]
	if paused.Follower == nil || !paused.Follower.SelfKnown || !paused.Follower.ReplayPaused ||
		paused.Follower.SlotWALStatus != "lost" || !paused.Follower.Streaming {
		t.Fatalf("running paused/lost follower diagnostics: %+v", paused.Follower)
	}
	awaiting := rows["app_awaiting"]
	if awaiting.State != stateAwaiting || awaiting.Follower != nil {
		t.Fatalf("awaiting recovery row %+v", awaiting)
	}

	facts := gatherFacts(context.Background(), cfg, engine.Runtime{Host: hostinfo.Info{
		VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100},
	}}, false)
	if !slices.Equal(hbaReads, []string{"app", "app_paused"}) {
		t.Fatalf("doctor did not use the fake HBA reader for running clusters: %v", hbaReads)
	}
	clusters := map[string]doctor.ClusterFacts{}
	for _, c := range facts.Clusters {
		clusters[c.Name] = c
	}
	if clusters["app_stopped"].Follower == nil {
		t.Fatal("doctor dropped the stopped unfinished follower's diagnostics")
	}
	if clusters["app_paused"].Follower == nil || !clusters["app_paused"].Follower.ReplayPaused ||
		clusters["app_paused"].Follower.SlotWALStatus != "lost" {
		t.Fatalf("doctor's running paused/lost facts: %+v", clusters["app_paused"].Follower)
	}
	if !clusters["app_awaiting"].AwaitingRecovery {
		t.Fatal("doctor lost the awaiting recovery marker")
	}
	results := doctor.Run(doctor.FollowerChecks(), facts, "")
	find := func(id, db string) doctor.Result {
		t.Helper()
		for _, r := range results {
			if r.ID == id && r.DB == db {
				return r
			}
		}
		t.Fatalf("doctor result %s/%s is missing", id, db)
		return doctor.Result{}
	}
	if got := find("follower.slot", "app_stopped"); got.Status != doctor.Warn || strings.Contains(got.Detail, "cannot read its replication") ||
		!strings.Contains(got.Detail, "more than half") {
		t.Errorf("stopped follower slot check: %+v", got)
	}
	if got := find("follower.slot", "app_paused"); got.Status != doctor.Fail || !strings.Contains(got.Detail, "lost") {
		t.Errorf("running follower slot check: %+v", got)
	}
	if got := find("follower.streaming", "app_paused"); got.Status != doctor.Fail || !strings.Contains(got.Detail, "paused") {
		t.Errorf("running follower streaming check: %+v", got)
	}

	var families []*promfile.Family
	writeFollowerProm = func(name string, got []*promfile.Family) error {
		if name != FollowersProm {
			t.Errorf("metrics written to %q", name)
		}
		families = got
		return nil
	}
	if err := writeFollowerMetrics(context.Background(), cfg); err != nil {
		t.Fatalf("metrics: %v", err)
	}
	byName := map[string]*promfile.Family{}
	for _, f := range families {
		byName[f.Name] = f
	}
	sample := func(family, db string) (float64, bool) {
		f := byName[family]
		if f == nil {
			return 0, false
		}
		for _, s := range f.Samples {
			for _, l := range s.Labels {
				if l.Name == "db" && l.Value == db {
					return s.Value, true
				}
			}
		}
		return 0, false
	}
	if v, ok := sample("vaire_follower_streaming", "app_stopped"); !ok || v != 0 {
		t.Errorf("stopped follower streaming sample = %v, %v", v, ok)
	}
	if v, ok := sample("vaire_follower_streaming", "app_paused"); !ok || v != 1 {
		t.Errorf("paused follower streaming sample = %v, %v", v, ok)
	}
	if v, ok := sample("vaire_follower_slot_lost", "app_paused"); !ok || v != 1 {
		t.Errorf("paused follower lost sample = %v, %v", v, ok)
	}
	if v, ok := sample("vaire_follower_slot_retained_bytes", "app_stopped"); !ok || v != 104857600 {
		t.Errorf("stopped follower retained sample = %v, %v", v, ok)
	}
	if v, ok := sample("vaire_follower_lag_seconds", "app_paused"); !ok || v != 120 {
		t.Errorf("paused follower lag seconds = %v, %v", v, ok)
	}
	for _, family := range families {
		if _, ok := sample(family.Name, "app_awaiting"); ok {
			t.Errorf("%s published the awaiting database", family.Name)
		}
	}

	// Convergence still excludes every unfinished entry from creation, start,
	// routing and timers, including the poolers' rendered configuration.
	only := []string{"postgres-cluster-v1", "postgres-config-v1", "pgbouncer-v1", "timers-v1"}
	rt := engine.Runtime{Host: hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}}
	journal := filepath.Join(t.TempDir(), "journal.json")
	plan, err := engine.Plan(context.Background(), steps.Catalog(), cfg, rt, engine.Options{Only: only, JournalPath: journal})
	if err != nil {
		t.Fatalf("general plan: %v", err)
	}
	unfinished := map[string]bool{"app_stopped": true, "app_paused": true, "app_awaiting": true}
	for _, s := range plan.Steps {
		if unfinished[s.Cluster] {
			t.Errorf("general plan selected %s for unfinished %s", s.ID, s.Cluster)
		}
		if s.ID == "pgbouncer-v1" {
			for _, d := range s.Diffs {
				for name := range unfinished {
					if strings.Contains(d.Diff, name+" =") || strings.Contains(d.Diff, name+"__auth =") {
						t.Errorf("general plan's pooler diff routes unfinished %s:\n%s", name, d.Diff)
					}
				}
			}
		}
	}
	viewed := engineView(cfg, "")
	expanded := map[string]engine.Step{}
	for _, s := range engine.Expand(steps.Catalog(), viewed) {
		expanded[s.ID()] = s
	}
	j := &engine.Journal{Schema: engine.JournalSchema, Steps: map[string]engine.JournalEntry{}}
	for _, s := range plan.Steps {
		step, ok := expanded[s.ID]
		if !ok {
			t.Fatalf("plan step %s is not in the viewed catalogue", s.ID)
		}
		inputs, err := step.Inputs(viewed, rt)
		if err != nil {
			t.Fatalf("inputs for %s: %v", s.ID, err)
		}
		j.Steps[s.ID] = engine.JournalEntry{Status: engine.StatusCompleted, InputsHash: engine.InputsHash(s.ID, inputs)}
	}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}
	// Keep the real cluster step in the plan, but do not run its host drift
	// recheck in a fixture without a mocked systemd directory.
	applied, _, err := engine.Apply(context.Background(), steps.Catalog(), cfg, rt, engine.Options{Only: []string{"postgres-config-v1", "pgbouncer-v1", "timers-v1"}, JournalPath: journal})
	if err != nil {
		t.Fatalf("general apply: %v", err)
	}
	for _, s := range applied.Steps {
		if unfinished[s.Cluster] {
			t.Errorf("general apply ran %s for unfinished %s", s.ID, s.Cluster)
		}
	}
}

// R4-6: a follower-only textfile write failure fails refresh instead of
// leaving stale healthy samples behind.
func TestFollowerMetricWriteFailureFailsRefresh(t *testing.T) {
	old := writeFollowerProm
	oldBackup, oldHost, oldWrite := refreshBackupMetrics, refreshHostRead, refreshHostWrite
	t.Cleanup(func() {
		writeFollowerProm = old
		refreshBackupMetrics, refreshHostRead, refreshHostWrite = oldBackup, oldHost, oldWrite
	})
	refreshBackupMetrics = func(config.Cluster) {}
	refreshHostRead = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
	refreshHostWrite = func(string, []*promfile.Family) error { return nil }
	want := errors.New("textfile directory is not writable")
	writeFollowerProm = func(string, []*promfile.Family) error { return want }

	if err := writeFollowerMetrics(context.Background(), config.Cluster{}); !errors.Is(err, want) {
		t.Fatalf("writeFollowerMetrics: %v", err)
	}
	if err := refreshMetrics(context.Background(), config.Cluster{}); !errors.Is(err, want) {
		t.Fatalf("refreshMetrics lost the follower write error: %v", err)
	}
}
