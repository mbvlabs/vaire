package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/tune"
)

// databaseInfo is one entry in db.list: the database, where its cluster
// runs and under which hostname, its size and limits, its stanza, and its
// state.
type databaseInfo struct {
	Name       string   `json:"name"`
	InConfig   bool     `json:"in_config"`
	Exists     bool     `json:"exists"`
	SizeBytes  int64    `json:"size_bytes"`
	Extensions []string `json:"extensions"`
	Roles      []string `json:"roles"`
	Node       string   `json:"node,omitempty"`
	Host       string   `json:"host,omitempty"`
	Port       int      `json:"port,omitempty"`
	Stanza     string   `json:"stanza,omitempty"`
	Size       string   `json:"size,omitempty"`
	MemoryMiB  int      `json:"memory_mib,omitempty"`
	// MemoryMaxMiB and CPUQuota are the opt-in hard limits; zero and empty
	// are unset.
	MemoryMaxMiB   int    `json:"memory_max_mib,omitempty"`
	CPUWeight      int    `json:"cpu_weight,omitempty"`
	CPUQuota       string `json:"cpu_quota,omitempty"`
	IOWeight       int    `json:"io_weight,omitempty"`
	MaxConnections int    `json:"max_connections,omitempty"`
	// Protected is clusters.<db>.protected.
	Protected bool `json:"protected,omitempty"`
	// State is running, following (a running follower), stopped (the
	// cluster exists but is down), pending (in cluster.toml, no cluster
	// yet) or orphaned (a cluster cluster.toml doesn't list).
	State string `json:"state"`
	// Follows is the database a follower streams from, and Follower its
	// replication: lag, slot, streaming (Phase 6).
	Follows          string                 `json:"follows,omitempty"`
	Follower         *followerReading       `json:"follower,omitempty"`
	ApplicationReady *bool                  `json:"application_ready,omitempty"`
	Materialisation  *materialisationReport `json:"materialisation,omitempty"`
}

// Database states in db.list.
const (
	stateRunning = "running"
	// stateFollowing is a running follower (Phase 6).
	stateFollowing = "following"
	stateStopped   = "stopped"
	statePending   = "pending"
	stateOrphaned  = "orphaned"
	// stateAwaiting is a database a rebuilt master hasn't recovered yet
	// (Phase 5): in cluster.toml, no cluster until db recover.
	stateAwaiting = "awaiting_recovery"
)

type dbCreateParams struct {
	Name             string   `json:"name"`
	StatementTimeout string   `json:"statement_timeout"`
	Extensions       []string `json:"extensions"`
	// Size, Memory and Profile are the cluster's; with neither size nor
	// memory, it gets [postgres] default_size.
	Size    string `json:"size,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Profile string `json:"profile,omitempty"`
	// Protected marks the new database protected (clusters.<db>.protected).
	Protected bool `json:"protected,omitempty"`
	// AllowOvercommit creates it even when its size pushes the databases'
	// sizes past the budget, with the memory_overcommitted warning.
	AllowOvercommit bool `json:"allow_overcommit,omitempty"`
}

type dbDropParams struct {
	Name string `json:"name"`
	// NoFinalDump skips the final long-retention dump db drop takes when
	// backups are configured.
	NoFinalDump bool `json:"no_final_dump"`
}

type dbCredentialsParams struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Rotate bool   `json:"rotate"`
}

func handleDBList(ctx context.Context, env *Env) (Result, error) {
	databases, err := listDatabases(ctx, env)
	if err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	for _, db := range databases {
		if db.State == stateOrphaned {
			warnings = append(warnings, output.Warning{Code: "orphaned_database",
				Message: "cluster 18/" + db.Name + " is on the server but not in cluster.toml; drop it with vaire db drop " + db.Name})
		}
	}
	return Result{Data: map[string]any{"databases": databases}, Warnings: warnings}, nil
}

// databaseListClusterNames and databaseListPG are db.list's host facts: the
// cluster inventory pg_lsclusters reports and each cluster's local SQL. They
// are variables so tests model a stopped follower without the node's commands.
var (
	databaseListClusterNames = steps.ClusterNamesStrict
	databaseListPG           = func(spec config.ClusterSpec) sqlRunner { return steps.ClusterPG(spec) }
)

// listDatabases reads each [[clusters]] entry's database from its own
// cluster. A cluster that is not up lists as not existing; a cluster
// pg_lsclusters knows that cluster.toml doesn't is orphaned, unless a
// restore owns it (<db>__restoring, or a <db>__pre_restore_<ts> leftover)
// or a drill does (<db>__drill).
func listDatabases(ctx context.Context, env *Env) ([]databaseInfo, error) {
	cfg := env.Cluster
	live, err := databaseListClusterNames(ctx)
	if err != nil {
		return nil, output.NewError(output.CodePrecondition, "PostgreSQL cluster inventory is unavailable", output.ExitPrecondition,
			"Check pg_lsclusters on the node and retry.").WithDetails(map[string]any{"reason": "cluster_inventory_unavailable"})
	}
	records, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	markers, err := guardedMaterialisations(cfg.Server.DataRoot)
	if err != nil {
		return nil, materialisationIncomplete("", nil)
	}
	reports := materialisationReportsFrom(ctx, cfg, markers, live)
	out := make([]databaseInfo, 0, len(cfg.Clusters))
	for _, spec := range cfg.Clusters {
		info := databaseInfo{Name: spec.Name, InConfig: true, Node: spec.Node, Host: cfg.DatabaseHostname(spec.Name),
			Port: spec.Port, Stanza: spec.Stanza, Protected: spec.Protected, State: statePending, Follows: spec.Follows}
		if res, err := cfg.Resources(spec); err == nil {
			info.Size, info.MemoryMiB, info.MemoryMaxMiB = res.Size, res.MemoryMiB, res.MemoryMaxMiB
			info.CPUWeight, info.CPUQuota, info.IOWeight, info.MaxConnections = res.CPUWeight, res.CPUQuota, res.IOWeight, res.MaxConnections
		}
		if report, ok := reports[spec.Name]; ok {
			out = append(out, incompleteDatabaseListRow(info, report))
			delete(live, spec.Name)
			delete(reports, spec.Name)
			continue
		}
		if status, ok := live[spec.Name]; ok {
			info.State = stateStopped
			if status == "online" {
				info.State = stateRunning
			}
			delete(live, spec.Name)
		}
		if spec.IsFollower() && info.State == stateRunning && !records.IsAwaiting(spec.Name) {
			info.State = stateFollowing
			if src, ok := cfg.ClusterByName(spec.Follows); ok {
				st := readFollower(ctx, spec, src)
				info.Follower = &st
			}
		}
		if records.IsAwaiting(spec.Name) {
			info.State = stateAwaiting
			// No cluster to measure: the stanza's size orders db recover --all.
			info.SizeBytes = records.Repo[spec.Name].RepoSize
			out = append(out, info)
			continue
		}
		local := databaseListPG(spec)
		rows, err := local.Query(ctx, "postgres",
			"SELECT pg_database_size(datname) AS size FROM pg_database WHERE datname = "+quoteLiteral(spec.DatabaseName()), nil)
		if err != nil || len(rows) == 0 {
			out = append(out, info)
			continue
		}
		info.Exists = true
		if _, err := fmt.Sscan(rows[0]["size"], &info.SizeBytes); err != nil {
			info.SizeBytes = 0
		}
		roles, err := existingRolesIn(ctx, local, spec.DatabaseName())
		if err == nil {
			info.Roles = roles
		}
		exts, err := local.Query(ctx, spec.DatabaseName(), "SELECT extname FROM pg_extension ORDER BY extname", nil)
		if err == nil {
			for _, row := range exts {
				info.Extensions = append(info.Extensions, row["extname"])
			}
		}
		out = append(out, info)
	}
	for name, report := range reports {
		out = append(out, incompleteDatabaseListRow(databaseInfo{Name: name}, report))
		delete(live, name)
	}
	for name := range live {
		if config.DatabasePattern.MatchString(name) && !records.IsTemporary(name) {
			out = append(out, databaseInfo{Name: name, Exists: true, State: stateOrphaned})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	if err := unchangedMaterialisationReports(cfg, markers); err != nil {
		return nil, err
	}
	return out, nil
}

func handleDBCreate(ctx context.Context, env *Env) (Result, error) {
	var p dbCreateParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := checkDatabaseName(p.Name); err != nil {
		return Result{}, err
	}
	if p.Protected {
		if err := defaultProtectGuard.checkProtect([]string{p.Name}); err != nil {
			return Result{}, err
		}
	}
	timeout := p.StatementTimeout
	if timeout == "" {
		timeout = "60s"
	}
	extensions := p.Extensions
	if len(extensions) == 0 {
		extensions = []string{"pg_stat_statements"}
	}
	spec := config.ClusterSpec{Name: p.Name, Size: p.Size, Memory: p.Memory, Profile: p.Profile, StatementTimeout: timeout, Extensions: extensions, Protected: p.Protected}
	db, warnings, err := createDatabase(ctx, env, spec, p.AllowOvercommit, createCommand(p))
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"database": db}, Warnings: warnings}, nil
}

// createCommand is the db create command of p at another size: what the
// memory_overcommitted refusal suggests.
func createCommand(p dbCreateParams) func(size string) string {
	return func(size string) string {
		cmd := "vaire db create " + p.Name + " --size " + size
		if p.Profile != "" {
			cmd += " --profile " + p.Profile
		}
		if p.StatementTimeout != "" && p.StatementTimeout != "60s" {
			cmd += " --statement-timeout " + p.StatementTimeout
		}
		if len(p.Extensions) > 1 || (len(p.Extensions) == 1 && p.Extensions[0] != "pg_stat_statements") {
			cmd += " --extensions " + strings.Join(p.Extensions, ",")
		}
		if p.Protected {
			cmd += " --protected"
		}
		return cmd
	}
}

// createOptions are the narrowly scoped differences between an ordinary
// database creation and a copy's provisioning.
type createOptions struct {
	// deferInitialBackup leaves the new cluster's stanza, first full backup
	// and timers to the caller. vaire db copy runs them after the import so
	// the backup reflects the copied data, not an empty database.
	deferInitialBackup bool
}

// createDatabase adds db to cluster.toml and creates its cluster: the
// kernel's huge pages, the cluster's own step instances with the role
// passwords from the request's secrets, both poolers, and the other
// databases' configuration whose share of the server changed. A cluster
// that could not start (would_not_start) or whose size overcommits the
// server (memory_overcommitted, unless allowOvercommit) is refused first.
// The warnings are the server's oversubscription and any restart the other
// databases' configuration left pending.
func createDatabase(ctx context.Context, env *Env, db config.ClusterSpec, allowOvercommit bool, suggest func(size string) string) (config.ClusterSpec, []output.Warning, error) {
	return createDatabaseWithOptions(ctx, env, db, allowOvercommit, suggest, createOptions{})
}

// createDatabaseWithOptions is createDatabase with explicit copy-only
// provisioning options.
func createDatabaseWithOptions(ctx context.Context, env *Env, db config.ClusterSpec, allowOvercommit bool, suggest func(size string) string, opts createOptions) (config.ClusterSpec, []output.Warning, error) {
	cfg, previous, db, err := addDatabase(ctx, env, db, allowOvercommit, suggest)
	if err != nil {
		return db, nil, err
	}
	if env.Req.DryRun {
		return db, oversubscriptionWarnings(cfg, runtimeFor(env, true).Host), nil
	}
	return provisionDatabase(ctx, env, cfg, previous, db, opts)
}

// provisionDatabase creates the cluster of a database whose entry is already
// saved in cfg. previous is the file before the entry was added, for the
// rollback of a cluster that was never created.
func provisionDatabase(ctx context.Context, env *Env, cfg config.Cluster, previous []byte, db config.ClusterSpec, opts createOptions) (config.ClusterSpec, []output.Warning, error) {
	var warnings []output.Warning
	env.Cluster = cfg
	rt := runtimeFor(env, false)
	others := otherConfigs(ctx, steps.Catalog(), cfg, rt, db.Name, "")
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, createRuns(cfg, db.Name, others)...))
	// The kernel's huge-page pool grows first when the new cluster uses
	// huge pages, then the cluster's own step instances: create it,
	// configure it, then its roles and database (RFC 11.2).
	_, _, err := createApplyKernel(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Restarter: steps.Restarter(cfg), Report: env.Report})
	if err == nil {
		err = createApplyNewCluster(ctx, cfg, rt, db.Name, "", env.Report)
	}
	if err != nil {
		// Keep cluster.toml in step with the server: an entry whose cluster
		// was never created would make the retry refuse the name. Once the
		// cluster exists, vaire apply --db finishes it.
		if !createClusterExists(ctx, db.Name) {
			_ = followerState().Save(previous)
			return db, nil, errors.Join(err, forgetNewCluster(cfg, db.Name))
		}
		return db, nil, withHint(err, finishNewCluster(db.Name))
	}
	if err := createReloadPoolers(ctx, env, cfg); err != nil {
		return db, nil, withHint(err, "Finish it with vaire apply.")
	}
	if w := createApplyMetrics(ctx, cfg, rt, env.Report, db.Name, ""); w != nil {
		warnings = append(warnings, *w)
	}
	warnings = append(warnings, createApplyOtherConfigs(ctx, steps.Catalog(), cfg, rt, others,
		engine.Options{Restarter: steps.Restarter(cfg), Report: env.Report})...)
	if !opts.deferInitialBackup {
		if w := createFirstBackup(ctx, cfg, rt, env.Report, db.Name); w != nil {
			warnings = append(warnings, *w)
		}
	}
	created, _ := cfg.ClusterByName(db.Name)
	return created, append(warnings, oversubscriptionWarnings(cfg, rt.Host)...), nil
}

// The provisioning seams keep ordinary create behavior unchanged while
// letting a copy's tests stop at each boundary without a live node.
var (
	createClusterExists     = steps.ClusterExists
	createConfDir           = steps.PGConfDir
	createApplyKernel       = engine.Apply
	createApplyNewCluster   = provisionOwnedTarget
	createReloadPoolers     = reloadPoolers
	createApplyMetrics      = applyMetrics
	createApplyOtherConfigs = applyOtherConfigs
	createFirstBackup       = firstBackup
)

// addDatabase checks db's name and role passwords, places it (node, port,
// stanza, the default size), refuses a cluster that could not start or
// whose size overcommits the server (unless allowOvercommit; suggest is the
// command at a size that fits), and saves it in cluster.toml without
// creating anything. It returns the saved configuration and the previous
// file for a rollback. db create and db fork share it.
func addDatabase(ctx context.Context, env *Env, db config.ClusterSpec, allowOvercommit bool, suggest func(size string) string) (config.Cluster, []byte, config.ClusterSpec, error) {
	return addDatabaseFrom(ctx, env, func(config.Cluster) (config.ClusterSpec, error) { return db, nil }, allowOvercommit, suggest, nil)
}

// addDatabaseCheckStarts refuses a proposal PostgreSQL would not start. It is
// a variable so handler tests can use deterministic host facts.
var addDatabaseCheckStarts = checkStarts

// addDatabaseFrom is addDatabase with the entry built from the freshly loaded
// configuration and an optional check of the exact placed proposal before it
// is saved. A fresh follow uses both: its entry derives from the source's
// current values, and its floors are compared with the target present exactly
// once under the preparation lock.
func addDatabaseFrom(ctx context.Context, env *Env, build func(config.Cluster) (config.ClusterSpec, error), allowOvercommit bool, suggest func(size string) string, beforeSave func(config.Cluster, config.ClusterSpec) error) (config.Cluster, []byte, config.ClusterSpec, error) {
	store := followerState()
	cfg, previous, err := store.Load()
	if err != nil {
		return cfg, nil, config.ClusterSpec{}, err
	}
	db, err := build(cfg)
	if err != nil {
		return cfg, nil, db, err
	}
	if _, ok := cfg.ClusterByName(db.Name); ok {
		return cfg, nil, db, existingDatabase(db.Name)
	}
	if createClusterExists(ctx, db.Name) {
		return cfg, nil, db, existingDatabase(db.Name)
	}
	if !env.Req.DryRun {
		for _, role := range steps.DatabaseRoles {
			key := steps.SecretDatabaseRole(db.Name, role)
			// A follower's roles are its source's, replicated (Phase 6).
			if env.Req.Secrets[key] == "" && !db.IsFollower() {
				return cfg, nil, db, output.NewError(output.CodeInputRequired, "the password for "+db.Name+"_"+role+" was not supplied", output.ExitInput,
					"Generate it on the operator and pass it as "+key+".").WithDetails(map[string]any{"secret": key})
			}
		}
	}
	if err := placeCluster(cfg, &db); err != nil {
		return cfg, nil, db, err
	}
	host := runtimeFor(env, false).Host
	if err := guardRecoveryFloorAddition(cfg, db, host); err != nil {
		return cfg, nil, db, err
	}
	cfg.Clusters = append(cfg.Clusters, db)
	if err := cfg.Validate(); err != nil {
		return cfg, nil, db, config.ValidationError(err)
	}
	if err := checkSpareCPU(ctx, db.Name, db.CPUMode); err != nil {
		return cfg, nil, db, err
	}
	if err := addDatabaseCheckStarts(cfg, db, host, 0); err != nil {
		return cfg, nil, db, err
	}
	if err := checkBudget(cfg, db.Name, 0, host, allowOvercommit, suggest); err != nil {
		return cfg, nil, db, err
	}
	if env.Req.DryRun {
		return cfg, previous, db, nil
	}
	if beforeSave != nil {
		if err := beforeSave(cfg, db); err != nil {
			return cfg, nil, db, err
		}
	}
	raw, err := cfg.Encode()
	if err != nil {
		return cfg, nil, db, err
	}
	if err := store.Save(raw); err != nil {
		return cfg, nil, db, err
	}
	return cfg, previous, db, nil
}

// forgetNewCluster undoes what a failed db create left of a cluster that
// was never created: its configuration directory, which a configure step
// may have half written and which makes pg_createcluster refuse the retry,
// and its journal entries, so the retry runs every instance fresh. A data
// directory means something of a cluster is there, so it keeps the
// configuration then.
func forgetNewCluster(cfg config.Cluster, name string) error {
	if _, err := os.Stat(cfg.Server.DataDirectory(name)); errors.Is(err, fs.ErrNotExist) {
		if err := os.RemoveAll(createConfDir(name)); err != nil {
			return err
		}
	}
	return engine.ForgetCluster(engine.JournalPath, name)
}

// forgetClusterJournal drops a cluster's journal entries. It is a variable so
// a rollback test can run the desired-state transaction without the node's
// journal path.
var forgetClusterJournal = engine.ForgetCluster

// forgetDatabase removes name's entry from cluster.toml and its journal
// entries: db fork's rollback when its cluster was never made.
func forgetDatabase(name string) error {
	if err := forgetClusterJournal(engine.JournalPath, name); err != nil {
		return err
	}
	store := followerState()
	cfg, _, err := store.Load()
	if err != nil {
		return err
	}
	kept := cfg.Clusters[:0]
	for _, c := range cfg.Clusters {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	cfg.Clusters = kept
	raw, err := cfg.Encode()
	if err != nil {
		return err
	}
	return store.Save(raw)
}

// createRuns are the applies db create and db fork run for the new
// database name, in order, for the request's step numbering: the kernel's
// huge pages, its own steps, the poolers, the other databases' changed
// configuration (others) and, with backups on, its stanza and timers.
func createRuns(cfg config.Cluster, name string, others []string) []engine.Options {
	runs := []engine.Options{{Only: steps.KernelStepIDs()}, {DB: name}, {Only: steps.PoolerStepIDs()}, {Only: steps.MetricsStepIDs()}}
	if len(others) > 0 {
		runs = append(runs, engine.Options{Only: others})
	}
	if cfg.BackupConfigured() {
		runs = append(runs, engine.Options{Only: append(steps.PgBackRestStepIDs(), steps.TimersStepIDs()...)})
	}
	return runs
}

// otherConfigs are the postgres-config-v1 instances of cfg's databases
// other than except whose rendered configuration changed since it was last
// applied (reason inputs_changed). Each cluster's tuning depends on the
// others: its share of the data disk sizes min_wal_size and max_wal_size,
// and the number of clusters its CPU share. So adding, forking or dropping
// a database changes them, and db create, db fork and db drop apply them in
// the same run instead of leaving them to the next vaire apply. A database
// never configured (a cluster still pending) is left to vaire apply.
// journal is the journal path, "" for the node's.
func otherConfigs(ctx context.Context, catalog []engine.Step, cfg config.Cluster, rt engine.Runtime, except, journal string) []string {
	var ids []string
	for _, c := range cfg.Clusters {
		if c.Name != except {
			ids = append(ids, steps.ConfigStepID(c.Name))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	plan, err := engine.Plan(ctx, catalog, cfg, rt, engine.Options{Only: ids, JournalPath: journal})
	if err != nil {
		return nil
	}
	var changed []string
	for _, s := range plan.Steps {
		if s.Action == "apply" && s.Reason == engine.ReasonInputsChanged {
			changed = append(changed, s.ID)
		}
	}
	return changed
}

// applyOtherConfigs applies ids, the other databases' changed
// configuration, as a reload: opts carries no confirm token, so a change
// that needs a restart restarts nothing and stays pending, with the
// restart_pending warning naming the token. The database being created or
// dropped is done by then, so a failure is the warning
// other_config_failed, which vaire apply retries.
func applyOtherConfigs(ctx context.Context, catalog []engine.Step, cfg config.Cluster, rt engine.Runtime, ids []string, opts engine.Options) []output.Warning {
	if len(ids) == 0 {
		return nil
	}
	opts.Only, opts.DB, opts.Confirm = ids, "", engine.Confirmation{}
	_, warnings, err := engine.Apply(ctx, catalog, cfg, rt, opts)
	if err != nil {
		return []output.Warning{{Code: "other_config_failed",
			Message: "the configuration of " + strings.Join(clustersOf(ids), ", ") + " was not updated for the new split of the server: " + err.Error() + "; run vaire apply"}}
	}
	return warnings
}

// clustersOf is the cluster of each instance ID.
func clustersOf(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		_, cluster := engine.SplitID(id)
		out = append(out, cluster)
	}
	return out
}

// finishNewCluster is the hint for a new cluster whose steps failed after it
// was created: its own steps with the restart it may still need, then the
// poolers, its stanza and first full, and its timers.
func finishNewCluster(name string) string {
	return "Finish it with vaire apply --db " + name + " --confirm " + name + "/restart, then vaire apply."
}

// reloadPoolers runs pgbouncer-v1, which renders an entry per database and
// reloads the poolers (RFC 8.1). It skips itself once no database is left.
func reloadPoolers(ctx context.Context, env *Env, cfg config.Cluster) error {
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:      steps.PoolerStepIDs(),
		Restarter: steps.Restarter(cfg),
		Report:    env.Report,
	})
	return err
}

// applyMetrics runs metrics-v1 after a database is created, forked or
// dropped, so its /metrics path and targets.json entry follow (Phase 4). The
// database already works, so a failure is the warning metrics_reload_failed.
// recovering keeps a database that awaits recovery in the steps' view.
func applyMetrics(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter, db, recovering string) *output.Warning {
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{
		Only:       steps.MetricsStepIDs(),
		Recovering: recovering,
		Restarter:  steps.Restarter(cfg),
		Report:     report,
	})
	if err == nil {
		return nil
	}
	return &output.Warning{
		Code:    "metrics_reload_failed",
		Message: "the /metrics endpoint was not updated for " + db + ": " + err.Error() + "; run vaire apply to retry",
	}
}

// firstBackup runs pgbackrest-v1 and timers-v1 when backups are on: the new
// cluster's stanza, its archive check and first full backup, then its backup
// timers. The database already works, so a failure is the warning
// first_backup_failed, not an error.
func firstBackup(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter, db string) *output.Warning {
	if !cfg.BackupConfigured() {
		return nil
	}
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{
		Only:      append(steps.PgBackRestStepIDs(), steps.TimersStepIDs()...),
		Restarter: steps.Restarter(cfg),
		Report:    report,
	})
	if err == nil {
		return nil
	}
	w := firstBackupWarning(db, err)
	return &w
}

// firstBackupWarning is first_backup_failed for err. A step that refused for
// a reason the operator has to act on (input_required, such as bucket keys
// that are neither on the node nor in the operator's recovery kit, or
// precondition_failed) carries its own hint: a bare vaire apply would refuse
// the same way. Any other failure is retried by vaire apply, which re-runs the
// unfinished steps with the recovery kit.
func firstBackupWarning(db string, err error) output.Warning {
	msg := db + " was created, but its stanza, first full backup or backup timers failed: " + err.Error()
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) && (cliErr.Code == output.CodeInputRequired || cliErr.Code == output.CodePrecondition) && cliErr.Hint != "" {
		return output.Warning{Code: "first_backup_failed", Message: msg + ". " + cliErr.Hint}
	}
	return output.Warning{Code: "first_backup_failed", Message: msg + "; run vaire apply to retry"}
}

func existingDatabase(name string) error {
	return output.NewError(output.CodePrecondition, "database "+name+" already exists", output.ExitPrecondition,
		"Pick another name, or drop it first with vaire db drop "+name+".").WithDetails(map[string]any{"reason": "database_exists", "database": name})
}

// databaseIn reports whether database name exists in the cluster at local.
func databaseIn(ctx context.Context, local pg.Local, name string) (bool, error) {
	rows, err := local.Query(ctx, "postgres", "SELECT datname FROM pg_database WHERE datname = "+quoteLiteral(name), nil)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

var (
	dbDropStore             = func() state.Store { return state.FileStore{} }
	dbDropClusterExists     = steps.ClusterExists
	dbDropSlot              = dropFollowerSlot
	dbDropLeftovers         = dropLeftovers
	dbDropForgetJournal     = engine.ForgetCluster
	dbDropOtherConfigs      = otherConfigs
	dbDropApplyOtherConfigs = applyOtherConfigs
	dbDropRouting           = dropRouting
	dbDropMetrics           = applyMetrics
	dbDropClearDeferred     = clearDeferred
)

func handleDBDrop(ctx context.Context, env *Env) (Result, error) {
	var p dbDropParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := checkDatabaseName(p.Name); err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("dropping "+p.Name+" deletes all of its data", p.Name)
	}
	// A recorded temporary drill uses its own exact-birth cleanup, never the
	// ordinary cluster/configuration drop path.
	records, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	drillSource := ""
	for db, name := range records.Drilling {
		if name == p.Name || backup.DrillName(db) == p.Name {
			if drillSource != "" && drillSource != db {
				return Result{}, incompleteMaterialisation(p.Name)
			}
			drillSource = db
		}
	}
	for db, attempt := range records.DrillAttempts {
		if attempt.Target == p.Name {
			if drillSource != "" && drillSource != db {
				return Result{}, incompleteMaterialisation(p.Name)
			}
			drillSource = db
		}
	}
	store := dbDropStore()
	cfg, _, err := store.Load()
	if err != nil {
		return Result{}, err
	}
	if drillSource != "" {
		if env.Req.Confirm != p.Name {
			return Result{}, confirmRequired("dropping "+p.Name+" deletes all of its data", p.Name)
		}
		attempt, ok := records.DrillAttempts[drillSource]
		if !ok || records.Drilling[drillSource] != p.Name || attempt.Source != drillSource || attempt.Target != p.Name {
			return Result{}, incompleteMaterialisation(p.Name)
		}
		cleanup, err := openDrillCleanup(ctx, cfg, attempt)
		if err != nil {
			return Result{}, err
		}
		defer cleanup.close()
		if err := cleanup.remove(ctx, env); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"dropped": p.Name}}, nil
	}
	spec, inConfig := cfg.ClusterByName(p.Name)
	_, retained, err := readMaterialisation(p.Name, cfg.Server.DataRoot)
	if err != nil {
		return Result{}, materialisationIncomplete(p.Name, nil)
	}
	var forkCleanup *materialisationArtifacts
	if retained {
		if env.Req.Confirm != p.Name {
			return Result{}, confirmRequired("dropping "+p.Name+" deletes all of its data", p.Name)
		}
		forkCleanup, err = openFailedForkCleanup(cfg, p.Name)
		if err != nil {
			return Result{}, err
		}
		defer forkCleanup.close()
	}
	var followCleanup *followCleanup
	if forkCleanup != nil {
		// Owned incomplete forks are never inspected as live databases.
	} else if inConfig && spec.IsFollower() {
		if env.Req.Confirm != p.Name {
			return Result{}, confirmRequired("dropping "+p.Name+" deletes all of its data", p.Name)
		}
		followCleanup, err = openFollowCleanup(cfg, spec)
		if err != nil {
			return Result{}, err
		}
		if followCleanup != nil {
			defer followCleanup.close()
		}
	} else if err := refuseUnverifiedFollowerDrop(p.Name, spec); err != nil {
		return Result{}, err
	}
	clusterExists := false
	if followCleanup == nil && forkCleanup == nil {
		clusterExists = dbDropClusterExists(ctx, p.Name)
	}
	if !inConfig && !clusterExists {
		return Result{}, output.NewError(output.CodePrecondition, "database "+p.Name+" does not exist", output.ExitPrecondition,
			"Run vaire db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": p.Name})
	}

	if err := restoreInProgress(p.Name, "db drop"); err != nil {
		return Result{}, err
	}
	if err := preflightDropLeftovers(cfg, p.Name); err != nil {
		return Result{}, err
	}
	if err := refuseSourceDrop(cfg, p.Name); err != nil {
		return Result{}, err
	}
	if forkCleanup != nil {
		if err := removeFailedFork(ctx, env, forkCleanup); err != nil {
			return Result{}, err
		}
	}
	if followCleanup != nil {
		if err := followCleanup.remove(ctx, env, cfg); err != nil {
			return Result{}, err
		}
	}
	local := steps.ClusterPG(spec)
	exists := false
	if inConfig && clusterExists {
		exists, _ = databaseIn(ctx, local, p.Name)
	}
	var warnings []output.Warning
	var final *backup.Dump
	switch {
	case !exists:
	case !cfg.BackupConfigured():
		warnings = append(warnings, output.Warning{Code: "no_backup_before_drop",
			Message: "backups are not configured, so no final dump of " + p.Name + " was taken"})
	case p.NoFinalDump:
		warnings = append(warnings, output.Warning{Code: "no_backup_before_drop",
			Message: "--no-final-dump: no final dump of " + p.Name + " was taken"})
	default:
		dump, err := dumpToBucket(ctx, cfg, engine.Shell{Secrets: secrets.Default, Log: env.Log}, "db.drop", env.Report, p.Name, true)
		if err != nil {
			return Result{}, output.WrapError(output.CodePrecondition, fmt.Errorf("the final dump of %s failed, so it was not dropped: %w", p.Name, err),
				output.ExitPrecondition, "Fix the bucket, or drop without it with --no-final-dump.").WithDetails(map[string]any{"reason": "final_dump_failed"})
		}
		final = &dump
	}

	// The database's roles live in its cluster, so they go with it.
	var roles []string
	if exists {
		roles, _ = existingRolesIn(ctx, local, p.Name)
	}
	// The kept stanza's window ends at the last WAL the cluster archived.
	lastArchived := ""
	if inConfig && clusterExists && spec.Stanza != "" && cfg.BackupConfigured() {
		lastArchived = lastArchivedTime(ctx, spec)
	}
	if clusterExists {
		if err := dropCluster(ctx, env, p.Name); err != nil {
			return Result{}, err
		}
	}
	// A follower's slot on its source goes with it (Phase 6).
	if inConfig && spec.IsFollower() {
		if w := dbDropSlot(ctx, cfg, spec); w != nil {
			warnings = append(warnings, *w)
		}
	}
	dropEnv := *env
	dropEnv.Cluster = cfg
	if err := dbDropLeftovers(&dropEnv, p.Name); err != nil {
		return Result{}, err
	}
	// A database created again under this name and size would otherwise
	// skip its step instances on the old journal entries and never get a
	// cluster. It is still in cluster.toml, so a retry finishes the drop.
	if err := dbDropForgetJournal(engine.JournalPath, p.Name); err != nil {
		return Result{}, err
	}

	kept := make([]config.ClusterSpec, 0, len(cfg.Clusters))
	for _, db := range cfg.Clusters {
		if db.Name != p.Name {
			kept = append(kept, db)
		}
	}
	cfg.Clusters = kept
	raw, err := cfg.Encode()
	if err != nil {
		return Result{}, err
	}
	if err := store.Save(raw); err != nil {
		return Result{}, err
	}
	env.Cluster = cfg
	if forkCleanup != nil {
		if err := forkCleanup.retireMaterialisation(func(o materialisationOwnership) (func(), error) {
			return forkReservationLock(ctx, o.Target, o.TemporaryPort, false)
		}, forkCleanup.validateCleanupCompleted); err != nil {
			return Result{}, err
		}
	}
	if followCleanup != nil {
		if err := followCleanup.retire(ctx); err != nil {
			return Result{}, err
		}
	}
	// Without a bucket the stanza never held a backup, so nothing is kept.
	stanzaKept := forkCleanup == nil && inConfig && spec.Stanza != "" && cfg.BackupConfigured()
	if stanzaKept {
		if err := recordDroppedStanza(cfg, spec, final, lastArchived); err != nil {
			warnings = append(warnings, output.Warning{Code: "records_not_updated",
				Message: "the stanza " + spec.Stanza + " of " + p.Name + " is kept, but recording it failed: " + err.Error()})
		}
	}
	rt := runtimeFor(env, false)
	others := dbDropOtherConfigs(ctx, steps.Catalog(), cfg, rt, "", "")
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, dropRuns(cfg, others)...))
	if err := dbDropRouting(ctx, env, cfg); err != nil {
		return Result{}, withHint(err, p.Name+" is dropped, but the poolers or timers still refer to it. Fix it with vaire apply.")
	}
	if w := dbDropMetrics(ctx, cfg, rt, env.Report, p.Name, ""); w != nil {
		warnings = append(warnings, *w)
	}
	dbDropClearDeferred(p.Name)
	warnings = append(warnings, dbDropApplyOtherConfigs(ctx, steps.Catalog(), cfg, rt, others,
		engine.Options{Restarter: steps.Restarter(cfg), Report: env.Report})...)
	if inConfig && spec.IsFollower() {
		if err := retireFollowOwnershipAfterDrop(p.Name, spec.Follows, spec.FollowGeneration); err != nil {
			warnings = append(warnings, output.Warning{Code: "follow_record_not_retired",
				Message: "the node-local follow record of " + p.Name + " was left after the drop: " + err.Error()})
		}
	}
	data := map[string]any{"dropped": p.Name, "roles_dropped": roles}
	if stanzaKept {
		data["stanza_kept"] = spec.Stanza
	}
	if final != nil {
		data["final_dump"] = final
	}
	return Result{Data: data, Warnings: warnings}, nil
}

func refuseUnverifiedFollowerDrop(name string, spec config.ClusterSpec) error {
	own, _, err := readFollowOwnership(followOwner{Name: name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec})
	if err != nil {
		return err
	}
	if own.Attempt != nil {
		return materialisationIncomplete(name, nil)
	}
	return nil
}

// recordDroppedStanza notes spec's stanza in records.json as dropped: db
// drop keeps it, so its backups and WAL stay restorable for the database's
// restore window, after which the stanza-expiry job deletes it. The
// database's last backup and verify results go, so its metrics stop.
func recordDroppedStanza(cfg config.Cluster, spec config.ClusterSpec, final *backup.Dump, lastArchived string) error {
	entry := droppedStanza(cfg, spec, lastArchived)
	if final != nil {
		entry.FinalDump = final.ID
	}
	err := backup.UpdateRecords(func(r *backup.Records) error {
		r.DroppedStanzas = append(r.DroppedStanzas, entry)
		delete(r.LastBackup, spec.Name)
		delete(r.LastVerify, spec.Name)
		delete(r.LastFailure, spec.Name)
		delete(r.Repo, spec.Name)
		return nil
	})
	steps.WriteMetrics(cfg)
	return err
}

// dropRuns are the applies db drop runs after the cluster is gone, in
// order, for the request's step numbering: the poolers (unless it was the
// last database), the timers, and the other databases' changed
// configuration (others).
func dropRuns(cfg config.Cluster, others []string) []engine.Options {
	var runs []engine.Options
	if len(cfg.Clusters) > 0 {
		runs = append(runs, engine.Options{Only: steps.PoolerStepIDs()})
	}
	runs = append(runs, engine.Options{Only: steps.TimersStepIDs()}, engine.Options{Only: steps.MetricsStepIDs()})
	if len(others) > 0 {
		runs = append(runs, engine.Options{Only: others})
	}
	return runs
}

// dropRouting takes a dropped database out of the poolers and the timers.
// pgbouncer-v1 renders an entry per database, but skips itself with none
// left, so dropping the last database stops and disables both poolers
// instead; the next db create starts them again. Its journal entry goes
// too: a database created with the same pooler inputs would skip it on the
// journal and leave both poolers stopped.
func dropRouting(ctx context.Context, env *Env, cfg config.Cluster) error {
	if len(cfg.Clusters) == 0 {
		if _, err := os.Stat(steps.SessionUnitPath); err != nil {
			return engine.ForgetSteps(engine.JournalPath, steps.PoolerStepIDs()...)
		}
		sh := engine.Shell{Secrets: secrets.Default, Log: env.Log}
		if err := sh.Exec(ctx, "db.drop", engine.Command{Name: "systemctl",
			Args: []string{"disable", "--now", "pgbouncer", steps.SessionPoolerService}}, env.Report); err != nil {
			return err
		}
		if err := engine.ForgetSteps(engine.JournalPath, steps.PoolerStepIDs()...); err != nil {
			return err
		}
	} else if err := reloadPoolers(ctx, env, cfg); err != nil {
		return err
	}
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:      steps.TimersStepIDs(),
		Restarter: steps.Restarter(cfg),
		Report:    env.Report,
	})
	return err
}

type dbResizeParams struct {
	Name     string  `json:"name"`
	Size     string  `json:"size,omitempty"`
	Memory   string  `json:"memory,omitempty"`
	CPUMode  *string `json:"cpu_mode,omitempty"`
	CPUQuota *string `json:"cpu_quota,omitempty"`
	// DeferRestart applies what is live (cgroup limits, reload-only
	// settings) and leaves the restart pending.
	DeferRestart bool `json:"defer_restart,omitempty"`
	// AllowOvercommit grows it even past the budget, with the
	// memory_overcommitted warning.
	AllowOvercommit bool `json:"allow_overcommit,omitempty"`
}

// applyResizeStep is engine.Apply for db resize's kernel and source runs; a
// variable so tests record their order beside the followers' runs.
var applyResizeStep = engine.Apply
var planCPUResize = engine.Plan
var resizeCatalog = steps.Catalog

// handleDBResize changes a database's size or memory (RFC 7.4). The cgroup
// limits and reload-only settings change live; shared_buffers,
// max_connections and huge_pages wait for <db>/restart, or stay pending
// with defer_restart. A size that could not start is refused, and so is a
// larger size that overcommits the server unless allow_overcommit; shrinking
// below what the cluster uses now is a warning. A resize that would lower a
// follower's hot-standby floors is refused (Q1a).
func handleDBResize(ctx context.Context, env *Env) (Result, error) {
	var p dbResizeParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := rejectNullCPUParams(env.Req.Params); err != nil {
		return Result{}, err
	}
	if err := validateCPUParams(p.CPUMode, p.CPUQuota); err != nil {
		return Result{}, err
	}
	if p.Size == "" && p.Memory == "" && p.CPUMode == nil && p.CPUQuota == nil {
		return Result{}, output.NewError(output.CodeUsage, "db resize needs a resource change", output.ExitUsage, "Pass --size, --memory, --cpu-mode or --cpu-quota.")
	}
	if err := restoreInProgress(p.Name, "db resize"); err != nil {
		return Result{}, err
	}
	store := followerState()
	cfg, _, err := store.Load()
	if err != nil {
		return Result{}, err
	}
	old, err := clusterOf(cfg, p.Name)
	if err != nil {
		return Result{}, err
	}
	resized := old
	if p.Size != "" {
		// A new size replaces the old memory override unless one is given.
		resized.Size, resized.Memory = p.Size, ""
	}
	if p.Memory != "" {
		resized.Memory = p.Memory
	}
	if p.CPUMode != nil {
		if *p.CPUMode != "normal" && *p.CPUMode != "spare" {
			return Result{}, output.NewError(output.CodeValidation, "CPU mode must be normal or spare", output.ExitConfig, "")
		}
		resized.CPUMode = *p.CPUMode
	}
	if p.CPUQuota != nil {
		resized.CPUQuota = *p.CPUQuota
		switch resized.CPUQuota {
		case "none":
			resized.CPUQuota = ""
		case "":
			return Result{}, output.NewError(output.CodeValidation, "CPU quota must be a percentage or none", output.ExitConfig, "")
		}
	}
	if old.Protected && ResizeShrinks(cfg, old, resized) {
		return Result{}, protectedError(p.Name, "shrinking it")
	}
	rt := runtimeFor(env, env.Req.DryRun)
	next := cfg
	next.Clusters = append([]config.ClusterSpec(nil), cfg.Clusters...)
	for i := range next.Clusters {
		if next.Clusters[i].Name == p.Name {
			next.Clusters[i] = resized
		}
	}
	if err := guardRecoveryFloorChanges(cfg, next, rt.Host); err != nil {
		return Result{}, err
	}
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == p.Name {
			cfg.Clusters[i] = resized
		}
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, config.ValidationError(err)
	}
	cpuOnly := p.Size == "" && p.Memory == ""
	if !cpuOnly && resized.CPUMode == "spare" && (p.CPUMode != nil || p.CPUQuota != nil) {
		if err := checkSpareCPU(ctx, p.Name, resized.CPUMode); err != nil {
			return Result{}, err
		}
	} else if err := checkCPUProposal(ctx, nextBeforeResize(cfg, old), cfg); err != nil {
		return Result{}, err
	}
	if cpuOnly {
		if err := checkCPUOnlyResources(ctx, cfg, p.Name); err != nil {
			return Result{}, err
		}
		catalog, err := cpuOnlyCatalog(steps.Catalog())
		if err != nil {
			return Result{}, err
		}
		env.Cluster = cfg
		opts := steps.CPUOnlyOptions(p.Name)
		opts.Report, opts.JournalPath = env.Report, cpuJournalPath()
		if env.Req.DryRun {
			plan, err := planCPUResize(ctx, catalog, cfg, rt, opts)
			return Result{Data: map[string]any{"database": resized, "plan": plan}}, err
		}
		raw, err := cfg.Encode()
		if err != nil {
			return Result{}, err
		}
		if err := invalidateCPUProofs([]string{p.Name}); err != nil {
			return Result{}, err
		}
		if err := store.Save(raw); err != nil {
			return Result{}, err
		}
		data, warnings, err := applyResizeStep(ctx, catalog, cfg, rt, opts)
		if err != nil {
			return Result{}, withHint(cpuPolicySaved(err), "The CPU policy is saved. Finish it with vaire apply --db "+p.Name+".")
		}
		return Result{Data: map[string]any{"database": resized, "apply": data}, Warnings: warnings}, nil
	}
	freed := 0
	if before := demandFor(env.Cluster, old, rt.Host); before.Running {
		freed = before.SharedMemoryMiB
	}
	if err := checkStarts(cfg, resized, rt.Host, freed); err != nil {
		return Result{}, err
	}
	before, _ := env.Cluster.Resources(old)
	if err := checkBudget(cfg, p.Name, before.MemoryMiB, rt.Host, p.AllowOvercommit, resizeCommand(p, restartConfirmation(cfg, env.Req).Covers(p.Name))); err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	res, _ := cfg.Resources(resized)
	if used, ok := clusterMemoryCurrentMiB(p.Name); ok && used > res.MemoryMiB {
		warnings = append(warnings, output.Warning{Code: "shrink_below_use", Message: fmt.Sprintf(
			"%s uses %s now, more than its new %s: it is throttled until it settles", p.Name, tune.FormatMiB(used), tune.FormatMiB(res.MemoryMiB))})
	}
	env.Cluster = cfg
	targets := cpuProofTargets(nextBeforeResize(cfg, old), cfg, nil)
	if p.CPUMode != nil || p.CPUQuota != nil {
		targets = append(targets, p.Name)
	}
	if env.Req.DryRun {
		ids := make([]string, 0, len(targets))
		for _, name := range targets {
			ids = append(ids, engine.InstanceID("postgres-cluster-v1", name))
		}
		data, err := engine.PlanAfterInvalidatingCompletions(ctx, resizeCatalog(), cfg, rt, engine.Options{DB: p.Name, JournalPath: cpuJournalPath()}, ids)
		if err != nil {
			return Result{}, err
		}
		followers, err := followerPlans(ctx, cfg, rt, p.Name, old, resized, engine.Options{})
		if err != nil {
			return Result{}, err
		}
		out := map[string]any{"database": resized, "plan": data}
		if len(followers) > 0 {
			out["followers"] = followers
		}
		return Result{Data: out, Warnings: append(warnings, oversubscriptionWarnings(cfg, rt.Host)...)}, nil
	}
	raw, err := cfg.Encode()
	if err != nil {
		return Result{}, err
	}
	if err := invalidateCPUProofs(targets); err != nil {
		return Result{}, err
	}
	if err := store.Save(raw); err != nil {
		return Result{}, err
	}
	// The huge-page pool follows the new sum before the cluster restarts.
	if _, _, err := applyResizeStep(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Restarter: steps.Restarter(cfg), Report: env.Report}); err != nil {
		return Result{}, withHint(err, "The new size is saved. Finish it with vaire apply --db "+p.Name+".")
	}
	confirm := restartConfirmation(cfg, env.Req)
	if p.DeferRestart {
		confirm = engine.Confirmation{}
	}
	// Resize never lowers a follower's floors (Q1a), so the followers always
	// take the source's new values first, even on a retry after a follower
	// update failed (Phase 6, Q1, D6).
	base := engine.Options{Confirm: confirm, Restarter: steps.Restarter(cfg), Report: env.Report}
	followers, err := applyFollowerConfigs(ctx, cfg, rt, p.Name, base)
	if err != nil {
		return Result{}, withHint(err, "The new size is saved and "+p.Name+" is not updated yet. Fix "+followers.Failed+", then run the same db resize again.")
	}
	data, applyWarnings, err := applyResizeStep(ctx, steps.Catalog(), cfg, rt, engine.Options{
		DB:        p.Name,
		Confirm:   confirm,
		Restarter: steps.Restarter(cfg),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, withHint(cpuPolicySaved(err), "The new size is saved. Finish it with vaire apply --db "+p.Name+".")
	}
	sourceRestarts := len(data.Restarts)
	// The restarts are listed in the order they ran: the followers' first.
	data.Restarts = append(followers.Restarts, data.Restarts...)
	data.RestartsSettled = append(followers.Settled, data.RestartsSettled...)
	warnings = append(append(append(warnings, followers.Warnings...), applyWarnings...), oversubscriptionWarnings(cfg, rt.Host)...)
	// A deferred restart runs at the start of the maintenance window
	// (maint-daily, Phase 4 D11); one done now clears an older deferral.
	out := map[string]any{"database": resized, "apply": data}
	switch {
	case p.DeferRestart && len(data.RestartPending) > 0:
		recordDeferred(p.Name)
		out["deferred_to_window"] = env.Cluster.Maintenance.Window
	case sourceRestarts > 0:
		clearDeferred(p.Name)
	}
	return Result{Data: out, Warnings: warnings}, nil
}

func nextBeforeResize(cfg config.Cluster, previous config.ClusterSpec) config.Cluster {
	cfg.Clusters = append([]config.ClusterSpec(nil), cfg.Clusters...)
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == previous.Name {
			cfg.Clusters[i] = previous
		}
	}
	return cfg
}

// resizeCommand is the db resize command of p at another size, with the
// request's restart choice: what the memory_overcommitted refusal suggests.
func resizeCommand(p dbResizeParams, confirmed bool) func(size string) string {
	return func(size string) string {
		cmd := "vaire db resize " + p.Name + " --size " + size
		if p.CPUMode != nil {
			cmd += " --cpu-mode " + *p.CPUMode
		}
		if p.CPUQuota != nil {
			cmd += " --cpu-quota " + *p.CPUQuota
		}
		switch {
		case p.DeferRestart:
			cmd += " --defer-restart"
		case confirmed:
			cmd += " --confirm " + p.Name + "/restart"
		}
		return cmd
	}
}

// checkDatabaseName refuses names outside the pattern and the system databases.
func checkDatabaseName(name string) error {
	if !config.DatabasePattern.MatchString(name) {
		return output.NewError(output.CodeValidation, "database name "+name+" must match "+config.DatabasePattern.String(), output.ExitConfig, "")
	}
	switch name {
	case "postgres", "template0", "template1":
		return output.NewError(output.CodeValidation, "database "+name+" is a system database vaire does not manage", output.ExitConfig, "")
	}
	return nil
}

// confirmRequired refuses a destructive request that carries no confirm token.
// The dispatcher has already checked any token that was sent.
func confirmRequired(message, token string) error {
	return output.NewError(output.CodeUnsafeAction, message, output.ExitUnsafe,
		"Re-run with the token in details.confirm_token after a human approves it.").
		WithDetails(map[string]any{"confirm_token": token})
}

// dropCluster stops and deletes 18/<name>, its data directory and its
// cgroup drop-in. db drop then removes its pooler entries and timers and
// keeps its stanza.
func dropCluster(ctx context.Context, env *Env, name string) error {
	sh := engine.Shell{Secrets: secrets.Default, Log: env.Log}
	if err := sh.Exec(ctx, "db.drop", engine.Command{Name: "pg_dropcluster", Args: []string{"18", name, "--stop"}}, env.Report); err != nil {
		return err
	}
	if err := os.RemoveAll(steps.PGServiceDropInDir(name)); err != nil {
		return err
	}
	return sh.Exec(ctx, "db.drop", engine.Command{Name: "systemctl", Args: []string{"daemon-reload"}}, env.Report)
}

// existingRolesIn returns the vaire roles of database name that exist in the
// cluster at local.
func existingRolesIn(ctx context.Context, local sqlRunner, name string) ([]string, error) {
	wanted := append([]string{name + "_owner"}, prefixedRoles(name)...)
	quoted := make([]string, len(wanted))
	for i, role := range wanted {
		quoted[i] = quoteLiteral(role)
	}
	rows, err := local.Query(ctx, "postgres",
		"SELECT rolname FROM pg_roles WHERE rolname IN ("+strings.Join(quoted, ", ")+") ORDER BY rolname", nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row["rolname"])
	}
	return out, nil
}

func prefixedRoles(name string) []string {
	out := make([]string, len(steps.DatabaseRoles))
	for i, role := range steps.DatabaseRoles {
		out[i] = name + "_" + role
	}
	return out
}

func handleDBCredentials(ctx context.Context, env *Env) (Result, error) {
	var p dbCredentialsParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if !p.Rotate {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"credentials live on the operator, not the node",
			output.ExitPrecondition,
			"Run vaire db credentials "+p.Name+" on your machine, or pass --rotate to set a new password.",
		).WithDetails(map[string]any{"reason": "operator_only", "database": p.Name})
	}
	if err := checkDatabaseName(p.Name); err != nil {
		return Result{}, err
	}
	local, err := clusterPG(env.Cluster, p.Name)
	if err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("rotating a password disconnects clients that use the old one", p.Name)
	}
	if err := restoreInProgress(p.Name, "rotating a password"); err != nil {
		return Result{}, err
	}
	role := p.Role
	if role == "" {
		role = steps.RoleApp
	}
	switch role {
	case steps.RoleMigrate, steps.RoleApp, steps.RoleRO:
	default:
		return Result{}, output.NewError(output.CodeValidation, "role must be migrate, app or ro", output.ExitConfig, "")
	}
	secretKey := steps.SecretDatabaseRole(p.Name, role)
	password := env.Req.Secrets[secretKey]
	if password == "" {
		return Result{}, output.NewError(output.CodeInputRequired, "the new password was not supplied", output.ExitInput,
			"Generate it on the operator and pass it as "+secretKey+".").WithDetails(map[string]any{"secret": secretKey})
	}
	verifier, err := pg.SCRAMVerifier(password)
	if err != nil {
		return Result{}, err
	}
	sql := fmt.Sprintf("ALTER ROLE %s PASSWORD '%s';\n", pg.Quote(p.Name+"_"+role), verifier)
	if err := local.Exec(ctx, "postgres", sql, nil); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"rotated": true, "role": role, "user": p.Name + "_" + role}}, nil
}

// quoteLiteral single-quotes a validated identifier used as a string literal.
func quoteLiteral(value string) string {
	return "'" + value + "'"
}

// placeCluster fills what db create assigns: the node (the master), a free
// port, a new stanza (none for a follower) and the default size.
func placeCluster(cfg config.Cluster, db *config.ClusterSpec) error {
	master, ok := cfg.Master()
	if !ok {
		return output.NewError(output.CodeConfig, "cluster.toml has no master node", output.ExitConfig, "")
	}
	db.Node = master.Name
	port, err := cfg.FreeClusterPort()
	if err != nil {
		return output.NewError(output.CodePrecondition, err.Error(), output.ExitPrecondition,
			"Drop a database you no longer need, or put the next one on its own node (Phase 10).").
			WithDetails(map[string]any{"reason": "no_cluster_port"})
	}
	db.Port = port
	// A follower has no stanza while it follows (Phase 6).
	if !db.IsFollower() {
		if db.Stanza, err = config.NewStanza(db.Name); err != nil {
			return err
		}
	}
	if db.Size == "" {
		db.Size = cfg.Postgres.DefaultSize
	}
	return nil
}
