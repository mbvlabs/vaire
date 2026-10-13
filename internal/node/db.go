package node

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/state"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/tune"
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
	// State is running, stopped (the cluster exists but is down), pending
	// (in cluster.toml, no cluster yet) or orphaned (a cluster cluster.toml
	// doesn't list).
	State string `json:"state"`
}

// Database states in db.list.
const (
	stateRunning  = "running"
	stateStopped  = "stopped"
	statePending  = "pending"
	stateOrphaned = "orphaned"
)

type dbCreateParams struct {
	Name             string   `json:"name"`
	StatementTimeout string   `json:"statement_timeout"`
	Extensions       []string `json:"extensions"`
	// Size, Memory and Profile are the cluster's; with neither size nor
	// memory, [postgres] default_size steps down to what fits.
	Size    string `json:"size,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Profile string `json:"profile,omitempty"`
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
				Message: "cluster 18/" + db.Name + " is on the server but not in cluster.toml; drop it with eru db drop " + db.Name})
		}
	}
	return Result{Data: map[string]any{"databases": databases}, Warnings: warnings}, nil
}

// listDatabases reads each [[clusters]] entry's database from its own
// cluster. A cluster that is not up lists as not existing; a cluster
// pg_lsclusters knows that cluster.toml doesn't is orphaned, unless a
// restore owns it (<db>__restoring, or a <db>__pre_restore_<ts> leftover).
func listDatabases(ctx context.Context, env *Env) ([]databaseInfo, error) {
	cfg := env.Cluster
	live := steps.ClusterNames(ctx)
	records, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	out := make([]databaseInfo, 0, len(cfg.Clusters))
	for _, spec := range cfg.Clusters {
		info := databaseInfo{Name: spec.Name, InConfig: true, Node: spec.Node, Host: cfg.DatabaseHostname(spec.Name),
			Port: spec.Port, Stanza: spec.Stanza, State: statePending}
		if res, err := cfg.Resources(spec); err == nil {
			info.Size, info.MemoryMiB, info.MemoryMaxMiB = res.Size, res.MemoryMiB, res.MemoryMaxMiB
			info.CPUWeight, info.CPUQuota, info.IOWeight, info.MaxConnections = res.CPUWeight, res.CPUQuota, res.IOWeight, res.MaxConnections
		}
		if status, ok := live[spec.Name]; ok {
			info.State = stateStopped
			if status == "online" {
				info.State = stateRunning
			}
			delete(live, spec.Name)
		}
		local := steps.ClusterPG(spec)
		rows, err := local.Query(ctx, "postgres",
			"SELECT pg_database_size(datname) AS size FROM pg_database WHERE datname = "+quoteLiteral(spec.Name), nil)
		if err != nil || len(rows) == 0 {
			out = append(out, info)
			continue
		}
		info.Exists = true
		if _, err := fmt.Sscan(rows[0]["size"], &info.SizeBytes); err != nil {
			info.SizeBytes = 0
		}
		roles, err := existingRolesIn(ctx, local, spec.Name)
		if err == nil {
			info.Roles = roles
		}
		exts, err := local.Query(ctx, spec.Name, "SELECT extname FROM pg_extension ORDER BY extname", nil)
		if err == nil {
			for _, row := range exts {
				info.Extensions = append(info.Extensions, row["extname"])
			}
		}
		out = append(out, info)
	}
	for name := range live {
		if config.DatabasePattern.MatchString(name) && !records.IsTemporary(name) {
			out = append(out, databaseInfo{Name: name, Exists: true, State: stateOrphaned})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
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
	timeout := p.StatementTimeout
	if timeout == "" {
		timeout = "60s"
	}
	extensions := p.Extensions
	if len(extensions) == 0 {
		extensions = []string{"pg_stat_statements"}
	}
	spec := config.ClusterSpec{Name: p.Name, Size: p.Size, Memory: p.Memory, Profile: p.Profile, StatementTimeout: timeout, Extensions: extensions}
	db, warnings, err := createDatabase(ctx, env, spec, p.Size == "" && p.Memory == "")
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"database": db}, Warnings: warnings}, nil
}

// createDatabase adds db to cluster.toml and creates its cluster: the
// kernel's huge pages, the cluster's own step instances with the role
// passwords from the request's secrets, then both poolers. With stepDown,
// db has no size of its own and the default steps down to what fits (RFC
// 7.4). A cluster that could not start is refused first (would_not_start).
// db create and db fork share it; the warnings are the step-down and the
// server's oversubscription.
func createDatabase(ctx context.Context, env *Env, db config.ClusterSpec, stepDown bool) (config.ClusterSpec, []output.Warning, error) {
	store := state.FileStore{}
	cfg, previous, err := store.Load()
	if err != nil {
		return db, nil, err
	}
	if _, ok := cfg.ClusterByName(db.Name); ok {
		return db, nil, existingDatabase(db.Name)
	}
	if steps.ClusterExists(ctx, db.Name) {
		return db, nil, existingDatabase(db.Name)
	}
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(db.Name, role)
		if env.Req.Secrets[key] == "" {
			return db, nil, output.NewError(output.CodeInputRequired, "the password for "+db.Name+"_"+role+" was not supplied", output.ExitInput,
				"Generate it on the operator and pass it as "+key+".").WithDetails(map[string]any{"secret": key})
		}
	}
	if err := placeCluster(cfg, &db); err != nil {
		return db, nil, err
	}
	rt := runtimeFor(env, false)
	var warnings []output.Warning
	if stepDown {
		if w := stepDownSize(cfg, &db, rt.Host); w != nil {
			warnings = append(warnings, *w)
		}
	}
	cfg.Clusters = append(cfg.Clusters, db)
	if err := cfg.Validate(); err != nil {
		return db, nil, config.ValidationError(err)
	}
	if err := checkStarts(cfg, db, rt.Host, 0); err != nil {
		return db, nil, err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return db, nil, err
	}
	if err := store.Save(raw); err != nil {
		return db, nil, err
	}
	env.Cluster = cfg
	// The kernel's huge-page pool grows first when the new cluster uses
	// huge pages, then the cluster's own step instances: create it,
	// configure it, then its roles and database (RFC 11.2).
	_, _, err = engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Report: env.Report})
	if err == nil {
		_, _, err = engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{DB: db.Name, Report: env.Report})
	}
	if err != nil {
		// Keep cluster.toml in step with the server: an entry whose cluster
		// was never created would make the retry refuse the name. Once the
		// cluster exists, eru apply --db finishes it.
		if !steps.ClusterExists(ctx, db.Name) {
			_ = store.Save(previous)
			return db, nil, err
		}
		return db, nil, withHint(err, "Finish it with eru apply --db "+db.Name+".")
	}
	if err := reloadPoolers(ctx, env, cfg); err != nil {
		return db, nil, withHint(err, "Finish it with eru apply.")
	}
	if w := firstBackup(ctx, env, cfg, db.Name); w != nil {
		warnings = append(warnings, *w)
	}
	created, _ := cfg.ClusterByName(db.Name)
	return created, append(warnings, oversubscriptionWarnings(cfg, rt.Host)...), nil
}

// stepDownSize gives db the largest size up to [postgres] default_size that
// fits in the server's free budget, and says so when that isn't the
// default. When even the smallest doesn't fit, db keeps the default and the
// memory_overcommitted warning tells the rest.
func stepDownSize(cfg config.Cluster, db *config.ClusterSpec, host hostinfo.Info) *output.Warning {
	_, sheet := budgetSheet(cfg, host)
	candidates := sizeCandidates(cfg, *db, host)
	picked, ok := tune.StepDown(candidates, db.Size, sheet.FreeMiB)
	if !ok || picked.Size == db.Size {
		return nil
	}
	over := 0
	for _, c := range candidates {
		if c.Size == db.Size {
			over = c.MemoryMiB - sheet.FreeMiB
		}
	}
	message := fmt.Sprintf("created as %s; %s would overcommit this server by %s", picked.Size, db.Size, tune.FormatMiB(over))
	db.Size = picked.Size
	return &output.Warning{Code: "size_stepped_down", Message: message}
}

// reloadPoolers runs pgbouncer-v1, which renders an entry per database and
// reloads the poolers (RFC 8.1). It skips itself once no database is left.
func reloadPoolers(ctx context.Context, env *Env, cfg config.Cluster) error {
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:   steps.PoolerStepIDs(),
		Report: env.Report,
	})
	return err
}

// firstBackup runs pgbackrest-v1 and timers-v1 when backups are on: the new
// cluster's stanza, its archive check and first full backup, then its backup
// timers. The database already works, so a failure is the warning
// first_backup_failed, not an error.
func firstBackup(ctx context.Context, env *Env, cfg config.Cluster, db string) *output.Warning {
	if !cfg.BackupConfigured() {
		return nil
	}
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:   append(steps.PgBackRestStepIDs(), steps.TimersStepIDs()...),
		Report: env.Report,
	})
	if err == nil {
		return nil
	}
	return &output.Warning{
		Code:    "first_backup_failed",
		Message: db + " was created, but its stanza, first full backup or backup timers failed: " + err.Error() + "; run eru apply to retry",
	}
}

func existingDatabase(name string) error {
	return output.NewError(output.CodePrecondition, "database "+name+" already exists", output.ExitPrecondition,
		"Pick another name, or drop it first with eru db drop "+name+".").WithDetails(map[string]any{"reason": "database_exists", "database": name})
}


// databaseIn reports whether database name exists in the cluster at local.
func databaseIn(ctx context.Context, local pg.Local, name string) (bool, error) {
	rows, err := local.Query(ctx, "postgres", "SELECT datname FROM pg_database WHERE datname = "+quoteLiteral(name), nil)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

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
	store := state.FileStore{}
	cfg, _, err := store.Load()
	if err != nil {
		return Result{}, err
	}
	spec, inConfig := cfg.ClusterByName(p.Name)
	clusterExists := steps.ClusterExists(ctx, p.Name)
	if !inConfig && !clusterExists {
		return Result{}, output.NewError(output.CodePrecondition, "database "+p.Name+" does not exist", output.ExitPrecondition,
			"Run eru db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": p.Name})
	}

	if err := restoreInProgress(p.Name, "db drop"); err != nil {
		return Result{}, err
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
	if clusterExists {
		if err := dropCluster(ctx, env, p.Name); err != nil {
			return Result{}, err
		}
	}
	if err := dropLeftovers(env, p.Name); err != nil {
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
	if inConfig && spec.Stanza != "" {
		if err := recordDroppedStanza(cfg, spec, final); err != nil {
			warnings = append(warnings, output.Warning{Code: "records_not_updated",
				Message: "the stanza " + spec.Stanza + " of " + p.Name + " is kept, but recording it failed: " + err.Error()})
		}
	}
	if err := dropRouting(ctx, env, cfg); err != nil {
		return Result{}, withHint(err, p.Name+" is dropped, but the poolers or timers still refer to it. Fix it with eru apply.")
	}
	data := map[string]any{"dropped": p.Name, "roles_dropped": roles}
	if inConfig && spec.Stanza != "" {
		data["stanza_kept"] = spec.Stanza
	}
	if final != nil {
		data["final_dump"] = final
	}
	return Result{Data: data, Warnings: warnings}, nil
}

// recordDroppedStanza notes spec's stanza in records.json as dropped: db
// drop keeps it, so its backups and WAL stay restorable for the database's
// restore window, after which the stanza-expiry job deletes it. The
// database's last backup and verify results go, so its metrics stop.
func recordDroppedStanza(cfg config.Cluster, spec config.ClusterSpec, final *backup.Dump) error {
	now := time.Now().UTC()
	entry := backup.DroppedStanza{
		Database:  spec.Name,
		Stanza:    spec.Stanza,
		DroppedAt: now.Format(time.RFC3339),
		ExpiresAt: now.AddDate(0, 0, 7*cfg.RetentionFull(spec)).Format(time.RFC3339),
	}
	if final != nil {
		entry.FinalDump = final.ID
	}
	err := backup.UpdateRecords(func(r *backup.Records) error {
		r.DroppedStanzas = append(r.DroppedStanzas, entry)
		delete(r.LastBackup, spec.Name)
		delete(r.LastVerify, spec.Name)
		return nil
	})
	steps.WriteMetrics(cfg)
	return err
}

// dropRouting takes a dropped database out of the poolers and the timers.
// pgbouncer-v1 renders an entry per database, but skips itself with none
// left, so dropping the last database stops and disables both poolers
// instead; the next db create starts them again.
func dropRouting(ctx context.Context, env *Env, cfg config.Cluster) error {
	if len(cfg.Clusters) == 0 {
		if _, err := os.Stat(steps.SessionUnitPath); err != nil {
			return nil
		}
		sh := engine.Shell{Secrets: secrets.Default, Log: env.Log}
		if err := sh.Exec(ctx, "db.drop", engine.Command{Name: "systemctl",
			Args: []string{"disable", "--now", "pgbouncer", steps.SessionPoolerService}}, env.Report); err != nil {
			return err
		}
	} else if err := reloadPoolers(ctx, env, cfg); err != nil {
		return err
	}
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:   steps.TimersStepIDs(),
		Report: env.Report,
	})
	return err
}

type dbResizeParams struct {
	Name   string `json:"name"`
	Size   string `json:"size,omitempty"`
	Memory string `json:"memory,omitempty"`
	// DeferRestart applies what is live (cgroup limits, reload-only
	// settings) and leaves the restart pending.
	DeferRestart bool `json:"defer_restart,omitempty"`
}

// handleDBResize changes a database's size or memory (RFC 7.4). The cgroup
// limits and reload-only settings change live; shared_buffers,
// max_connections and huge_pages wait for <db>/restart, or stay pending
// with defer_restart. A size that could not start is refused; shrinking
// below what the cluster uses now is a warning.
func handleDBResize(ctx context.Context, env *Env) (Result, error) {
	var p dbResizeParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if p.Size == "" && p.Memory == "" {
		return Result{}, output.NewError(output.CodeUsage, "db resize needs a size or memory", output.ExitUsage, "Pass --size or --memory.")
	}
	if err := restoreInProgress(p.Name, "db resize"); err != nil {
		return Result{}, err
	}
	store := state.FileStore{}
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
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == p.Name {
			cfg.Clusters[i] = resized
		}
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, config.ValidationError(err)
	}
	rt := runtimeFor(env, env.Req.DryRun)
	freed := 0
	if before := demandFor(env.Cluster, old, rt.Host); before.Running {
		freed = before.SharedMemoryMiB
	}
	if err := checkStarts(cfg, resized, rt.Host, freed); err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	res, _ := cfg.Resources(resized)
	if used, ok := clusterMemoryCurrentMiB(p.Name); ok && used > res.MemoryMiB {
		warnings = append(warnings, output.Warning{Code: "shrink_below_use", Message: fmt.Sprintf(
			"%s uses %s now, more than its new %s: it is throttled until it settles", p.Name, tune.FormatMiB(used), tune.FormatMiB(res.MemoryMiB))})
	}
	env.Cluster = cfg
	if env.Req.DryRun {
		data, err := engine.Plan(ctx, steps.Catalog(), cfg, rt, engine.Options{DB: p.Name})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"database": resized, "plan": data},
			Warnings: append(warnings, oversubscriptionWarnings(cfg, rt.Host)...)}, nil
	}
	raw, err := cfg.Encode()
	if err != nil {
		return Result{}, err
	}
	if err := store.Save(raw); err != nil {
		return Result{}, err
	}
	// The huge-page pool follows the new sum before the cluster restarts.
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Report: env.Report}); err != nil {
		return Result{}, withHint(err, "The new size is saved. Finish it with eru apply --db "+p.Name+".")
	}
	confirm := restartConfirmation(cfg, env.Req)
	if p.DeferRestart {
		confirm = engine.Confirmation{}
	}
	data, applyWarnings, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{
		DB:        p.Name,
		Confirm:   confirm,
		Restarter: steps.Restarter(cfg),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, withHint(err, "The new size is saved. Finish it with eru apply --db "+p.Name+".")
	}
	warnings = append(append(warnings, applyWarnings...), oversubscriptionWarnings(cfg, rt.Host)...)
	return Result{Data: map[string]any{"database": resized, "apply": data}, Warnings: warnings}, nil
}

// checkDatabaseName refuses names outside the pattern and the system databases.
func checkDatabaseName(name string) error {
	if !config.DatabasePattern.MatchString(name) {
		return output.NewError(output.CodeValidation, "database name "+name+" must match "+config.DatabasePattern.String(), output.ExitConfig, "")
	}
	switch name {
	case "postgres", "template0", "template1":
		return output.NewError(output.CodeValidation, "database "+name+" is a system database eru does not manage", output.ExitConfig, "")
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

// existingRolesIn returns the eru roles of database name that exist in the
// cluster at local.
func existingRolesIn(ctx context.Context, local pg.Local, name string) ([]string, error) {
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
			"Run eru db credentials "+p.Name+" on your machine, or pass --rotate to set a new password.",
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
// port, a new stanza and the default size.
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
	if db.Stanza, err = config.NewStanza(db.Name); err != nil {
		return err
	}
	if db.Size == "" {
		db.Size = cfg.Postgres.DefaultSize
	}
	return nil
}
