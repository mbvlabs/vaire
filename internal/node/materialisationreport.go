package node

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

const stateMaterialisationIncomplete = "incomplete"

// materialisationReport contains only safe, known facts. Nil observations
// mean unknown, not false. It does not describe application availability.
type materialisationReport struct {
	Job           string `json:"owning_job,omitempty"`
	Phase         string `json:"phase"`
	Reason        string `json:"reason,omitempty"`
	PhysicalState string `json:"physical_state"`
	InRecovery    *bool  `json:"in_recovery,omitempty"`
	ReplayPaused  *bool  `json:"replay_paused,omitempty"`
}

var materialisationReportInventory = steps.ClusterNamesStrict
var materialisationReportArtifactsVerified = func(o materialisationOwnership, dataRoot string) bool {
	uid, err := materialisationPostgresUID()
	if err != nil {
		return false
	}
	artifacts, err := openMaterialisationArtifacts(o.Target, dataRoot, uid)
	if err != nil {
		return false
	}
	defer artifacts.close()
	return artifacts.owner == o && artifacts.validate() == nil
}
var materialisationReportQuery = func(ctx context.Context, port int) ([]map[string]string, error) {
	local := steps.ClusterPG(config.ClusterSpec{Port: port})
	return local.Query(ctx, "postgres",
		"SELECT current_setting('data_directory') AS data_directory, current_setting('port') AS port, pg_is_in_recovery() AS in_recovery, CASE WHEN pg_is_in_recovery() THEN pg_is_wal_replay_paused() END AS replay_paused", nil)
}

func reportBool(v string) *bool {
	var b bool
	switch v {
	case "t", "true":
		b = true
	case "f", "false":
		b = false
	default:
		return nil
	}
	return &b
}

// materialisationReports reads the entire trusted marker set before querying
// anything. Inventory and SQL failures mean unknown physical facts.
func materialisationReports(ctx context.Context, cfg config.Cluster) (map[string]materialisationReport, []materialisationOwnership, error) {
	records, err := guardedMaterialisations(cfg.Server.DataRoot)
	if err != nil {
		return nil, nil, materialisationIncomplete("", nil)
	}
	var inventory map[string]string
	if len(records) != 0 {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		inventory, _ = materialisationReportInventory(qctx)
		cancel()
	}
	return materialisationReportsFrom(ctx, cfg, records, inventory), records, nil
}

func materialisationReportsFrom(ctx context.Context, cfg config.Cluster, records []materialisationOwnership, inventory map[string]string) map[string]materialisationReport {
	reports := make(map[string]materialisationReport, len(records))
	for _, o := range records {
		report := materialisationReport{Phase: o.Phase, Reason: o.Reason, PhysicalState: "unknown"}
		if validJobID(o.Job) {
			report.Job = o.Job
		}
		registered := o.ArtifactStage == materialisationRegistered || o.ArtifactStage == materialisationProvisioned
		if registered && materialisationReportArtifactsVerified(o, cfg.Server.DataRoot) {
			switch inventory[o.Target] {
			case "down":
				report.PhysicalState = "down"
			case "online":
				if o.Phase != materialisationCopying && o.Phase != materialisationProvisioning && o.Phase != materialisationFailed {
					break
				}
				port := o.TemporaryPort
				if o.Phase == materialisationProvisioning {
					port = o.IntendedPort
				}
				qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				rows, queryErr := materialisationReportQuery(qctx, port)
				cancel()
				if queryErr == nil && len(rows) == 1 && rows[0]["data_directory"] == o.DataPath &&
					rows[0]["port"] == strconv.Itoa(port) {
					report.PhysicalState = "online"
					report.InRecovery = reportBool(rows[0]["in_recovery"])
					if report.InRecovery != nil && *report.InRecovery {
						report.ReplayPaused = reportBool(rows[0]["replay_paused"])
					}
				}
			}
		}
		// A phase, port or identity transition during observation invalidates
		// the physical sample. Never publish raw metadata errors.
		current, found, err := readMaterialisation(o.Target, cfg.Server.DataRoot)
		if err != nil || !found || current != o ||
			(registered && !materialisationReportArtifactsVerified(o, cfg.Server.DataRoot)) {
			report.PhysicalState, report.InRecovery, report.ReplayPaused = "unknown", nil, nil
		}
		reports[o.Target] = report
	}
	return reports
}

func unchangedMaterialisationReports(cfg config.Cluster, before []materialisationOwnership) error {
	after, err := guardedMaterialisations(cfg.Server.DataRoot)
	if err != nil || !reflect.DeepEqual(before, after) {
		return materialisationIncomplete("", nil)
	}
	return nil
}

func incompleteDiagnosticsView(cfg config.Cluster) (config.Cluster, []materialisationOwnership, error) {
	records, err := guardedMaterialisations(cfg.Server.DataRoot)
	if err != nil {
		return config.Cluster{}, nil, materialisationIncomplete("", nil)
	}
	excluded := make(map[string]bool, len(records))
	for _, record := range records {
		excluded[record.Target] = true
	}
	out := cfg
	out.Clusters = nil
	for _, spec := range cfg.Clusters {
		if !excluded[spec.Name] {
			out.Clusters = append(out.Clusters, spec)
		}
	}
	return out, records, nil
}

// omitIncompleteMeasurements keeps unknown application values absent rather
// than emitting fabricated zero measurements.
func omitIncompleteMeasurements(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"uptime_s", "connections", "db_size", "wal_bytes", "longest_xact_s", "backup", "restart_pending", "restore_in_progress"} {
		delete(fields, key)
	}
	return json.Marshal(fields)
}

// incompleteDatabaseListRow is applied before follower or application SQL.
func incompleteDatabaseListRow(info databaseInfo, report materialisationReport) databaseInfo {
	info.State, info.Follower = stateMaterialisationIncomplete, nil
	ready := false
	info.ApplicationReady, info.Materialisation = &ready, &report
	return info
}

func incompleteStatusRow(cfg config.Cluster, name string, report materialisationReport) statusDatabase {
	spec, _ := cfg.ClusterByName(name)
	res, _ := cfg.Resources(spec)
	ready := false
	return statusDatabase{Name: name, State: stateMaterialisationIncomplete, Size: res.Size,
		MemoryMiB: res.MemoryMiB, MemoryHighMiB: res.MemoryMiB + res.MemoryMiB/4,
		Protected: spec.Protected, Follows: spec.Follows,
		ApplicationReady: &ready, Materialisation: &report}
}

func (row databaseInfo) MarshalJSON() ([]byte, error) {
	type plain databaseInfo
	raw, err := json.Marshal(plain(row))
	if err != nil || row.State != stateMaterialisationIncomplete {
		return raw, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"exists", "size_bytes", "roles", "extensions"} {
		delete(fields, key)
	}
	return json.Marshal(fields)
}
