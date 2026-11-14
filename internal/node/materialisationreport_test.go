package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/health"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

func TestMaterialisationActualStatusSkipsApplicationProbes(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: o.Target, Size: "small", Follows: "source"}}
	oldHost, oldTLS, oldMaster, oldState := statusHostRead, statusTLSRead, statusMasterRead, statusStateRead
	oldPending, oldBudget, oldWarnings, oldPool := statusPendingRead, statusBudgetRead, statusBudgetWarnings, statusPoolRead
	oldInventory, oldSnapshot, oldFollower, oldCPU := materialisationReportInventory, snapshotPG, followerPG, statusCPURead
	oldRecords := backup.RecordsPath
	t.Cleanup(func() {
		statusHostRead, statusTLSRead, statusMasterRead, statusStateRead = oldHost, oldTLS, oldMaster, oldState
		statusPendingRead, statusBudgetRead, statusBudgetWarnings, statusPoolRead = oldPending, oldBudget, oldWarnings, oldPool
		materialisationReportInventory, snapshotPG, followerPG, statusCPURead = oldInventory, oldSnapshot, oldFollower, oldCPU
		backup.RecordsPath = oldRecords
	})
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	statusHostRead = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
	statusTLSRead = func(config.Cluster) *TLSState { return nil }
	statusMasterRead = func(config.Cluster) *statusMaster { return nil }
	statusStateRead = func() (engine.State, error) { return engine.State{}, nil }
	statusPendingRead = func() map[string]bool { return nil }
	statusBudgetRead = func(got config.Cluster, _ hostinfo.Info) map[string]any {
		if len(got.Clusters) != 1 || got.Clusters[0].Name != o.Target {
			t.Fatal("incomplete target omitted from budget")
		}
		return map[string]any{"clusters": []map[string]any{{"name": o.Target, "running": false, "memory_current_mib": 0}}}
	}
	statusBudgetWarnings = func(config.Cluster, hostinfo.Info) []output.Warning { return nil }
	statusPoolRead = func(context.Context) map[string]health.Pools { return nil }
	materialisationReportInventory = func(context.Context) (map[string]string, error) {
		return map[string]string{o.Target: "online"}, nil
	}
	snapshotPG = func(config.ClusterSpec) sqlRunner { t.Fatal("application SQL reached"); return nil }
	followerPG = func(config.ClusterSpec) sqlRunner { t.Fatal("follower SQL reached"); return nil }
	statusCPURead = func(context.Context, config.ClusterSpec) (hostinfo.CPUObservation, string, *int64) {
		t.Fatal("CPU policy read reached")
		return hostinfo.CPUObservation{}, "", nil
	}
	result, err := handleStatus(context.Background(), &Env{Cluster: cfg})
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(statusData)
	budget := data.Budget["clusters"].([]map[string]any)[0]
	if _, ok := budget["running"]; ok {
		t.Fatal("partial target reported as stopped in budget")
	}
	if _, ok := budget["memory_current_mib"]; ok {
		t.Fatal("partial target reported a known cgroup sample in budget")
	}
	if len(data.Databases) != 1 || data.Databases[0].State != stateMaterialisationIncomplete ||
		data.Databases[0].ApplicationReady == nil || *data.Databases[0].ApplicationReady {
		t.Fatalf("incorrect status: %+v", data.Databases)
	}
	raw, err := json.Marshal(data.Databases[0])
	if err != nil || strings.Contains(string(raw), `"connections"`) || strings.Contains(string(raw), `"cpu"`) {
		t.Fatalf("status fabricated application facts: %s %v", raw, err)
	}
}

func TestMaterialisationActualListingBeforeApplicationQueries(t *testing.T) {
	o, root := materialisationFixture(t)
	o.Job = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: o.Target, Size: "small", Follows: "source"}}
	oldInventory, oldPG, oldQuery := databaseListClusterNames, databaseListPG, materialisationReportQuery
	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() {
		databaseListClusterNames, databaseListPG, materialisationReportQuery = oldInventory, oldPG, oldQuery
		backup.RecordsPath = oldRecords
	})
	raw, _ := json.Marshal(backup.Records{AwaitingRecovery: map[string]backup.Awaiting{o.Target: {}}})
	if err := os.WriteFile(backup.RecordsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	databaseListClusterNames = func(context.Context) (map[string]string, error) {
		return map[string]string{o.Target: "online", "orphan": "online"}, nil
	}
	databaseListPG = func(config.ClusterSpec) sqlRunner {
		t.Fatal("incomplete target reached application SQL")
		return nil
	}
	materialisationReportQuery = func(context.Context, int) ([]map[string]string, error) {
		t.Fatal("prepared target reached physical SQL")
		return nil, nil
	}
	result, err := handleDBList(context.Background(), &Env{Cluster: cfg})
	if err != nil {
		t.Fatal(err)
	}
	rows := result.Data.(map[string]any)["databases"].([]databaseInfo)
	if len(rows) != 2 || rows[0].Name != "orphan" || rows[0].State != stateOrphaned ||
		rows[1].Name != o.Target || rows[1].State != stateMaterialisationIncomplete ||
		rows[1].ApplicationReady == nil || *rows[1].ApplicationReady {
		t.Fatalf("wrong listing: %+v", rows)
	}
	encoded, err := json.Marshal(rows[1])
	if err != nil || strings.Contains(string(encoded), `"exists"`) ||
		!strings.Contains(string(encoded), `"application_ready":false`) ||
		!strings.Contains(string(encoded), o.Job) {
		t.Fatalf("wrong JSON: %s %v", encoded, err)
	}
	databaseListClusterNames = func(context.Context) (map[string]string, error) {
		return nil, errors.New("inventory unavailable")
	}
	_, err = handleDBList(context.Background(), &Env{Cluster: cfg})
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Details.(map[string]any)["reason"] != "cluster_inventory_unavailable" {
		t.Fatalf("inventory failure: %v", err)
	}
}

func TestMaterialisationReportingPhysicalFacts(t *testing.T) {
	for _, tc := range []struct {
		name, phase, artifact, inventory, reason string
		queryResult                              []map[string]string
		queryErr                                 error
		port, calls                              int
		physical                                 string
		recovery, paused                         *bool
	}{
		{name: "prepared", phase: materialisationPrepared, physical: "unknown"},
		{name: "failed", phase: materialisationFailed, reason: "recovery_parameter_mismatch", physical: "unknown"},
		{name: "failed paused", phase: materialisationFailed, reason: "recovery_parameter_mismatch", artifact: materialisationRegistered, inventory: "online",
			queryResult: []map[string]string{{"in_recovery": "t", "replay_paused": "t"}}, port: config.TemporaryPortMin, calls: 1,
			physical: "online", recovery: boolPointer(true), paused: boolPointer(true)},
		{name: "recovering", phase: materialisationCopying, artifact: materialisationRegistered, inventory: "online",
			queryResult: []map[string]string{{"in_recovery": "t", "replay_paused": "t"}}, port: config.TemporaryPortMin, calls: 1,
			physical: "online", recovery: boolPointer(true), paused: boolPointer(true)},
		{name: "provisioning", phase: materialisationProvisioning, artifact: materialisationRegistered, inventory: "online",
			queryResult: []map[string]string{{"in_recovery": "f"}}, port: 5433, calls: 1,
			physical: "online", recovery: boolPointer(false)},
		{name: "down", phase: materialisationCopying, artifact: materialisationRegistered, inventory: "down", physical: "down"},
		{name: "unknown", phase: materialisationCopying, artifact: materialisationRegistered, physical: "unknown"},
		{name: "unreadable", phase: materialisationCopying, artifact: materialisationRegistered, inventory: "online",
			queryErr: errors.New("private raw log"), port: config.TemporaryPortMin, calls: 1, physical: "unknown"},
		{name: "bad boolean", phase: materialisationCopying, artifact: materialisationRegistered, inventory: "online",
			queryResult: []map[string]string{{"in_recovery": "maybe", "replay_paused": "f"}}, port: config.TemporaryPortMin, calls: 1, physical: "online"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, root := materialisationFixture(t)
			o.Job = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
			if _, err := createMaterialisation(o, root); err != nil {
				t.Fatal(err)
			}
			o.Phase, o.ArtifactStage, o.Reason = tc.phase, tc.artifact, tc.reason
			if tc.artifact != "" {
				o.TemporaryPort = config.TemporaryPortMin
				o.DataIdentity = materialisationFileID{Inode: 1, UID: materialisationRecordUID}
				o.ConfigIdentity = materialisationFileID{Inode: 2, UID: materialisationRecordUID}
			}
			raw, _ := json.Marshal(o)
			path, _ := materialisationPath(o.Target)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			oldInventory, oldQuery, oldVerify := materialisationReportInventory, materialisationReportQuery, materialisationReportArtifactsVerified
			t.Cleanup(func() {
				materialisationReportInventory, materialisationReportQuery, materialisationReportArtifactsVerified = oldInventory, oldQuery, oldVerify
			})
			materialisationReportInventory = func(context.Context) (map[string]string, error) {
				return map[string]string{o.Target: tc.inventory}, nil
			}
			materialisationReportArtifactsVerified = func(materialisationOwnership, string) bool { return true }
			calls := 0
			materialisationReportQuery = func(_ context.Context, port int) ([]map[string]string, error) {
				calls++
				if port != tc.port {
					t.Errorf("port %d want %d", port, tc.port)
				}
				if len(tc.queryResult) == 1 {
					tc.queryResult[0]["data_directory"] = o.DataPath
					tc.queryResult[0]["port"] = fmt.Sprint(port)
				}
				return tc.queryResult, tc.queryErr
			}
			cfg := config.DefaultCluster()
			cfg.Server.DataRoot = root
			cfg.Clusters = []config.ClusterSpec{{Name: o.Target, Size: "small"}}
			reports, snapshot, err := materialisationReports(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			r := reports[o.Target]
			if calls != tc.calls || r.Reason != tc.reason || r.PhysicalState != tc.physical || !equalReportBool(r.InRecovery, tc.recovery) ||
				!equalReportBool(r.ReplayPaused, tc.paused) || r.Job != o.Job {
				t.Fatalf("report %+v calls %d", r, calls)
			}
			if err := unchangedMaterialisationReports(cfg, snapshot); err != nil {
				t.Fatal(err)
			}
			row := incompleteStatusRow(cfg, o.Target, r)
			encoded, err := json.Marshal(row)
			if err != nil || row.State != "incomplete" || row.ApplicationReady == nil || *row.ApplicationReady ||
				strings.Contains(string(encoded), `"connections"`) || strings.Contains(string(encoded), "private raw log") {
				t.Fatalf("unsafe row %s %v", encoded, err)
			}
			list := incompleteDatabaseListRow(databaseInfo{Name: o.Target, State: "following", Exists: true}, r)
			encoded, err = json.Marshal(list)
			if err != nil || list.State != "incomplete" || strings.Contains(string(encoded), `"exists"`) {
				t.Fatalf("unsafe list row %s %v", encoded, err)
			}
		})
	}
}

func boolPointer(v bool) *bool { return &v }
func equalReportBool(a, b *bool) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func TestMaterialisationReportingRejectsUntrustedAndChanged(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	oldInventory := materialisationReportInventory
	materialisationReportInventory = func(context.Context) (map[string]string, error) { return nil, nil }
	t.Cleanup(func() { materialisationReportInventory = oldInventory })
	_, snapshot, err := materialisationReports(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := materialisationPath(o.Target)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unchangedMaterialisationReports(cfg, snapshot); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("changed: %v", err)
	}
	if _, _, err := materialisationReports(context.Background(), cfg); output.ExitCode(err) != output.ExitPrecondition ||
		strings.Contains(err.Error(), path) {
		t.Fatalf("corrupt marker: %v", err)
	}
}

func TestMaterialisationReportingForeignAndUnsafeJob(t *testing.T) {
	o, root := materialisationFixture(t)
	o.Job = "/not/a/safe/job"
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: o.Target}, {Name: "other"}}
	oldInventory := materialisationReportInventory
	materialisationReportInventory = func(context.Context) (map[string]string, error) { return nil, nil }
	t.Cleanup(func() { materialisationReportInventory = oldInventory })
	reports, _, err := materialisationReports(context.Background(), cfg)
	if err != nil || reports[o.Target].Job != "" {
		t.Fatalf("unsafe job label: %+v %v", reports, err)
	}
	view, _, err := incompleteDiagnosticsView(cfg)
	if err != nil || len(view.Clusters) != 1 || view.Clusters[0].Name != "other" {
		t.Fatalf("diagnostic view: %+v %v", view, err)
	}
	followHostInstance = func() (string, error) { return "foreign", nil }
	if _, _, err := materialisationReports(context.Background(), cfg); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("foreign record: %v", err)
	}
}

func TestMaterialisationReportingChangesDuringSample(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	o.Phase, o.ArtifactStage, o.TemporaryPort = materialisationCopying, materialisationRegistered, config.TemporaryPortMin
	o.DataIdentity = materialisationFileID{Inode: 1, UID: materialisationRecordUID}
	o.ConfigIdentity = materialisationFileID{Inode: 2, UID: materialisationRecordUID}
	path, _ := materialisationPath(o.Target)
	raw, _ := json.Marshal(o)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	oldInventory, oldQuery, oldVerify := materialisationReportInventory, materialisationReportQuery, materialisationReportArtifactsVerified
	t.Cleanup(func() {
		materialisationReportInventory, materialisationReportQuery, materialisationReportArtifactsVerified = oldInventory, oldQuery, oldVerify
	})
	materialisationReportInventory = func(context.Context) (map[string]string, error) {
		return map[string]string{o.Target: "online"}, nil
	}
	materialisationReportArtifactsVerified = func(materialisationOwnership, string) bool { return true }
	materialisationReportQuery = func(context.Context, int) ([]map[string]string, error) {
		o.Phase = materialisationProvisioning
		raw, _ := json.Marshal(o)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return []map[string]string{{"in_recovery": "t", "replay_paused": "t"}}, nil
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	reports, before, err := materialisationReports(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := reports[o.Target]
	if r.PhysicalState != "unknown" || r.InRecovery != nil || r.ReplayPaused != nil {
		t.Fatalf("accepted stale physical sample: %+v", r)
	}
	if err := unchangedMaterialisationReports(cfg, before); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("changed metadata: %v", err)
	}
}

func TestMaterialisationReportingUnsafeRecordMode(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	path, _ := materialisationPath(o.Target)
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // intentionally unsafe record
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	called := false
	oldInventory := materialisationReportInventory
	materialisationReportInventory = func(context.Context) (map[string]string, error) {
		called = true
		return nil, nil
	}
	t.Cleanup(func() { materialisationReportInventory = oldInventory })
	if _, _, err := materialisationReports(context.Background(), cfg); output.ExitCode(err) != output.ExitPrecondition || called {
		t.Fatalf("unsafe record passed preflight: %v, inventory called %v", err, called)
	}
	if _, err := handleDoctorRun(context.Background(), &Env{Cluster: cfg}); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("doctor did not refuse unverifiable metadata: %v", err)
	}
}
