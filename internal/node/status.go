package node

import (
	"context"
	"sync"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/health"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/steps"
)

// clusterSnapshot is one cluster read for status and doctor.
type clusterSnapshot struct {
	Spec     config.ClusterSpec
	State    string // running, stopped, unreachable
	Error    string
	Sample   health.Sample
	TPS      float64
	HasTPS   bool
	Cgroup   hostinfo.Cgroup
	CgroupOK bool
}

// snapshotTimeout bounds one cluster's reading, so a hung cluster shows as
// unreachable instead of holding the whole status.
const snapshotTimeout = 10 * time.Second

// readClusters reads every cluster of cfg (only db when set) concurrently
// over its socket. With tps it samples the transaction counter twice, a
// second apart.
func readClusters(ctx context.Context, cfg config.Cluster, db string, tps bool) []clusterSnapshot {
	var specs []config.ClusterSpec
	for _, spec := range cfg.Clusters {
		if db == "" || spec.Name == db {
			specs = append(specs, spec)
		}
	}
	out := make([]clusterSnapshot, len(specs))
	started := time.Now()
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := clusterSnapshot{Spec: spec}
			s.Cgroup, s.CgroupOK = hostinfo.ReadCgroup(hostinfo.ServiceCgroup(steps.PGServiceName(spec.Name)))
			if !clusterRunning(spec) {
				s.State = "stopped"
				out[i] = s
				return
			}
			qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
			defer cancel()
			rows, err := steps.ClusterPG(spec).Query(qctx, spec.Name, health.SampleSQL, nil)
			if err != nil || len(rows) == 0 {
				s.State = "unreachable"
				if err != nil {
					s.Error = err.Error()
				}
				out[i] = s
				return
			}
			s.State = "running"
			s.Sample = health.ParseSample(rows[0])
			out[i] = s
		}()
	}
	wg.Wait()
	if !tps {
		return out
	}
	if wait := time.Second - time.Since(started); wait > 0 {
		select {
		case <-ctx.Done():
			return out
		case <-time.After(wait):
		}
	}
	elapsed := time.Since(started)
	for i := range out {
		if out[i].State != "running" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
			defer cancel()
			rows, err := steps.ClusterPG(out[i].Spec).Query(qctx, out[i].Spec.Name, health.XactsSQL, nil)
			if err != nil || len(rows) == 0 {
				return
			}
			after := health.ParseSample(rows[0]).Xacts
			out[i].TPS, out[i].HasTPS = health.TPS(out[i].Sample.Xacts, after, elapsed), true
		}()
	}
	wg.Wait()
	return out
}

// readPools sums both poolers' SHOW POOLS; a pooler that doesn't answer is
// left out.
func readPools(ctx context.Context) map[string]health.Pools {
	out := map[string]health.Pools{}
	for name, l := range map[string]struct {
		dir  string
		port int
	}{"transaction": {steps.PGSocketDir, steps.PgbouncerPort}, "session": {steps.SessionSocketDir, steps.SessionPoolerPort}} {
		qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		rows, err := steps.PoolerAdminPG(l.dir, l.port).Query(qctx, "pgbouncer", "SHOW POOLS;", nil)
		cancel()
		if err == nil {
			out[name] = health.SumPools(rows)
		}
	}
	return out
}

type statusParams struct {
	DB string `json:"db,omitempty"`
}

// statusConnections is a database's connections by state.
type statusConnections struct {
	Used     int64 `json:"used"`
	Max      int64 `json:"max"`
	Active   int64 `json:"active"`
	Idle     int64 `json:"idle"`
	IdleInTx int64 `json:"idle_in_tx"`
}

// statusBackup is a database's backup state.
type statusBackup struct {
	LastFull         string `json:"last_full,omitempty"`
	LastDiff         string `json:"last_diff,omitempty"`
	LastIncr         string `json:"last_incr,omitempty"`
	WindowStart      string `json:"pitr_window_start,omitempty"`
	ArchiveFailed    int64  `json:"archive_failed_count"`
	LastArchivedAgeS int64  `json:"last_archived_age_s"`
	Sets             int    `json:"sets,omitempty"`
	RepoSize         int64  `json:"repo_size,omitempty"`
	LastFailure      string `json:"last_failure,omitempty"`
}

// statusDatabase is one database's row of vaire status.
type statusDatabase struct {
	Name              string            `json:"name"`
	State             string            `json:"state"`
	Error             string            `json:"error,omitempty"`
	Size              string            `json:"size"`
	MemoryMiB         int               `json:"memory_mib"`
	MemoryHighMiB     int               `json:"memory_high_mib"`
	MemoryCurrentMiB  *int64            `json:"memory_current_mib,omitempty"`
	MemoryPressure10  *float64          `json:"memory_pressure_avg10,omitempty"`
	UptimeS           int64             `json:"uptime_s"`
	Connections       statusConnections `json:"connections"`
	CacheHitRatio     *float64          `json:"cache_hit_ratio,omitempty"`
	TPS               *float64          `json:"tps,omitempty"`
	DBSize            int64             `json:"db_size"`
	WALBytes          int64             `json:"wal_bytes"`
	LongestXactS      int64             `json:"longest_xact_s"`
	Backup            statusBackup      `json:"backup"`
	Drill             map[string]string `json:"drill,omitempty"`
	RestartPending    bool              `json:"restart_pending"`
	DeferredRestart   string            `json:"deferred_restart,omitempty"`
	RestoreInProgress bool              `json:"restore_in_progress"`
	Protected         bool              `json:"protected,omitempty"`
}

// statusData is vaire status's result (Phase 4).
type statusData struct {
	Server       string                  `json:"server"`
	Node         string                  `json:"node"`
	Role         string                  `json:"role"`
	VaireVersion string                  `json:"vaire_version"`
	PGVersion    string                  `json:"pg_version,omitempty"`
	Budget       map[string]any          `json:"budget"`
	Poolers      map[string]health.Pools `json:"poolers"`
	Disk         map[string]any          `json:"disk,omitempty"`
	TLS          *TLSState               `json:"tls,omitempty"`
	Backups      bool                    `json:"backups_configured"`
	Databases    []statusDatabase        `json:"databases"`
	// Master is the master backup's last bundle (Phase 5).
	Master *statusMaster `json:"master_backup,omitempty"`
}

// statusMaster is the master backup's row on the server card.
type statusMaster struct {
	Last       string `json:"last,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	Uploaded   bool   `json:"uploaded"`
	Bucket     bool   `json:"bucket"`
	Epoch      int64  `json:"epoch"`
	Pending    bool   `json:"pending"`
	Fenced     bool   `json:"fenced"`
	LastFailed string `json:"last_failure,omitempty"`
}

func masterStatusRow(cfg config.Cluster) *statusMaster {
	f := masterFacts(cfg)
	if !f.Known {
		return nil
	}
	last := loadMasterLast()
	e, _ := masterbackup.ReadLocalEpoch("")
	return &statusMaster{Last: last.Name, CreatedAt: last.CreatedAt, Uploaded: last.Name != "" && last.UploadedName == last.Name,
		Bucket: f.Bucket, Epoch: e.Epoch, Pending: f.PendingSince != "" || f.Dirty, Fenced: f.Fenced, LastFailed: last.LastFailure}
}

// handleStatus is vaire status: the server's summary and a row per database
// (Phase 4). It takes no lock, so it answers while an apply runs; a cluster
// that can't be read is shown as unreachable, never a failed RPC.
func handleStatus(ctx context.Context, env *Env) (Result, error) {
	var p statusParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	if p.DB != "" {
		if _, err := clusterOf(cfg, p.DB); err != nil {
			return Result{}, err
		}
	}
	host, _ := hostinfo.Detect(cfg.Server.DataRoot)
	data := statusData{
		Server:       cfg.Name(),
		Role:         config.RoleMaster,
		VaireVersion: buildinfo.Version,
		Budget:       serverBudget(cfg, host),
		Poolers:      map[string]health.Pools{},
		TLS:          inspectTLS(cfg),
		Backups:      cfg.BackupConfigured(),
		Databases:    []statusDatabase{},
	}
	if n, ok := cfg.Master(); ok {
		data.Node = n.Name
	}
	if total, free, ok := hostinfo.DiskUsage(cfg.Server.DataRoot); ok {
		data.Disk = map[string]any{"path": cfg.Server.DataRoot, "total_bytes": total, "free_bytes": free, "free_pct": freePct(total, free)}
	}
	data.Master = masterStatusRow(cfg)
	snaps := readClusters(ctx, cfg, p.DB, true)
	if len(cfg.Clusters) > 0 {
		data.Poolers = readPools(ctx)
	}
	rs, _ := backup.LoadRecords()
	st, _ := engine.LoadState(engine.StatePath)
	pending := pendingRestarts()
	for _, s := range snaps {
		row := statusRow(cfg, s, rs, st, pending)
		if rs.IsAwaiting(s.Spec.Name) {
			row.State, row.Error = stateAwaiting, "vaire db recover "+s.Spec.Name
		}
		if data.PGVersion == "" {
			data.PGVersion = s.Sample.ServerVersion
		}
		data.Databases = append(data.Databases, row)
	}
	warnings := append(oversubscriptionWarnings(cfg, host), NoOffsiteWarning(cfg)...)
	warnings = append(warnings, statusWarnings(data)...)
	return Result{Data: data, Warnings: warnings}, nil
}

func freePct(total, free uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(free) * 100 / float64(total)
}

// statusRow turns a snapshot and the records into a database's row.
func statusRow(cfg config.Cluster, s clusterSnapshot, rs backup.Records, st engine.State, pending map[string]bool) statusDatabase {
	spec := s.Spec
	res, _ := cfg.Resources(spec)
	row := statusDatabase{
		Name: spec.Name, State: s.State, Error: s.Error, Size: res.Size,
		MemoryMiB: res.MemoryMiB, MemoryHighMiB: res.MemoryMiB + res.MemoryMiB/4,
		UptimeS: s.Sample.UptimeS, DBSize: s.Sample.DBSize, WALBytes: s.Sample.WALBytes, LongestXactS: s.Sample.LongestXactS,
		Connections: statusConnections{
			Used: s.Sample.Connections, Max: s.Sample.MaxConnections,
			Active: s.Sample.Active, Idle: s.Sample.Idle, IdleInTx: s.Sample.IdleInTx,
		},
		RestartPending:  pendingFor(pending, spec.Name) || s.Sample.PendingRestart > 0,
		DeferredRestart: st.DeferredRestarts[spec.Name],
		Protected:       spec.Protected,
	}
	if row.Connections.Max == 0 {
		row.Connections.Max = int64(res.MaxConnections)
	}
	if s.CgroupOK {
		mib := s.Cgroup.MemoryCurrent >> 20
		row.MemoryCurrentMiB = &mib
		if p, ok := s.Cgroup.Pressure["memory"]; ok {
			avg := p.Some.Avg10
			row.MemoryPressure10 = &avg
		}
	}
	if ratio, ok := health.CacheHit(s.Sample.BlksHit, s.Sample.BlksRead); ok && s.State == "running" {
		row.CacheHitRatio = &ratio
	}
	if s.HasTPS {
		tps := s.TPS
		row.TPS = &tps
	}
	last := rs.LastBackup[spec.Name]
	row.Backup = statusBackup{
		LastFull: last[backup.TypeFull].FinishedAt, LastDiff: last[backup.TypeDiff].FinishedAt, LastIncr: last[backup.TypeIncr].FinishedAt,
		ArchiveFailed: s.Sample.ArchiveFailed, LastArchivedAgeS: s.Sample.LastArchivedAgeS,
	}
	if f, ok := rs.Repo[spec.Name]; ok && f.Stanza == spec.Stanza {
		row.Backup.WindowStart, row.Backup.Sets, row.Backup.RepoSize = f.WindowStart, f.Sets, f.RepoSize
	}
	for _, at := range rs.LastFailure[spec.Name] {
		if at > row.Backup.LastFailure {
			row.Backup.LastFailure = at
		}
	}
	if d, ok := rs.LastDrill(spec.Name); ok {
		row.Drill = map[string]string{"last_result": d.Result, "at": d.FinishedAt}
	}
	_, row.RestoreInProgress = rs.OpenRestore(spec.Name)
	return row
}

// statusWarnings are the per-database warnings status adds: a pending
// restart, an open restore, a cluster that isn't running.
func statusWarnings(d statusData) []output.Warning {
	var out []output.Warning
	for _, db := range d.Databases {
		switch db.State {
		case "stopped":
			out = append(out, output.Warning{Code: "database_stopped", Message: db.Name + "'s cluster is not running"})
		case "unreachable":
			out = append(out, output.Warning{Code: "database_unreachable", Message: db.Name + "'s cluster does not answer: " + db.Error})
		}
		if db.RestartPending {
			msg := db.Name + " waits for a restart to apply settings"
			if db.DeferredRestart != "" {
				msg += "; it restarts at the start of the maintenance window"
			}
			out = append(out, output.Warning{Code: "restart_pending", Message: msg})
		}
		if db.RestoreInProgress {
			out = append(out, output.Warning{Code: "restore_in_progress", Message: "a restore of " + db.Name + " is open (vaire restore status " + db.Name + ")"})
		}
	}
	return out
}
