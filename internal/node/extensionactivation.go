package node

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

// Extension safety across a copy's life (Phase 7): the winning guard, the
// copied pg_cron jobs, the desired SQL a writable copy must have before it is
// routed, and the extra extensions restored data brought back.

// These are seams for lifecycle tests: production uses the real host effects.
var (
	deactivateCopyCron     = deactivateCopiedCronJobs
	deactivateCopyCronAt   = deactivateCopiedCronJobsAt
	cronIdentity           = cronMetadataIdentity
	cronCurrentName        = cronMetadataCurrentName
	retargetCopyCron       = retargetCopiedCronJobs
	reconcileCopySQL       = reconcileDesiredExtensionSQL
	checkCopyExtensions    = checkRestoredExtensions
	pruneCopyGuard         = pruneCopyGuardForSettlement
	checkCopyCronSetting   = checkCronMetadataSetting
	ensureCopyGuardAt      = ensureCopyGuard
	ensureAdoptedGuardAt   = ensureAdoptedCopyGuard
	verifyStartedGuardAt   = verifyStartedCopyGuard
	verifyGuardEffective   = verifyCopyGuardEffective
	copyGuardLocal         = func(spec config.ClusterSpec) sqlRunner { return steps.ClusterPG(spec) }
	resumeForkSettlementAt = resumeForkSettlement
)

// copyGuardEndpoint is the cluster a guard is installed in and verified
// against. For a restore before the swap it is the owned temporary copy; after
// the swap it is the live cluster.
type copyGuardEndpoint struct {
	Name    string
	Port    int
	ConfDir string
}

func liveCopyGuardEndpoint(spec config.ClusterSpec) copyGuardEndpoint {
	return copyGuardEndpoint{Name: spec.Name, Port: spec.Port, ConfDir: steps.PGConfDir(spec.Name)}
}

func (e copyGuardEndpoint) spec() config.ClusterSpec {
	return config.ClusterSpec{Name: e.Name, Port: e.Port}
}

// ensureAdoptedCopyGuard installs the plan on an already adopted follower copy
// before it is started again. A stopped copy cannot prove the guard effective
// yet; the start path verifies it before routing.
func ensureAdoptedCopyGuard(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec, plan *extensionCopyPlan) error {
	if plan == nil || plan.Plan == nil {
		return nil
	}
	dataDir := cfg.Server.DataDirectory(spec.Name)
	if _, err := os.Stat(dataDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	ep := liveCopyGuardEndpoint(spec)
	if _, _, err := backup.InstallCopyPlanGuard(ep.ConfDir, dataDir, plan.Plan); err != nil {
		return err
	}
	if !clusterRunning(spec) {
		return nil
	}
	_, err := verifyGuardEffective(ctx, ep, dataDir, func() error {
		return backup.RestartCluster(ctx, env.shell(), env.Job, env.Report, spec.Name, dataDir, spec.Port)
	})
	return err
}

// ensureCopyGuard installs the complete guard before a promotion or restart and
// proves it effective. A pending postmaster guard setting is not success: the
// owned copy is restarted and rechecked before the caller promotes it.
func ensureCopyGuard(ctx context.Context, dataDir string, plan *extensionCopyPlan, ep copyGuardEndpoint, restart func() error) ([]string, error) {
	if plan == nil || plan.Plan == nil {
		return nil, nil
	}
	if _, err := os.Stat(dataDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if _, _, err := backup.InstallCopyPlanGuard(ep.ConfDir, dataDir, plan.Plan); err != nil {
		return nil, err
	}
	if !clusterRunning(ep.spec()) {
		return nil, nil
	}
	return verifyGuardEffective(ctx, ep, dataDir, restart)
}

// verifyCopyGuardEffective reloads and verifies the installed guard. Any
// pending postmaster setting restarts the safe owned copy and rechecks; a
// second pending result is a refusal, never an ignored success.
func verifyCopyGuardEffective(ctx context.Context, ep copyGuardEndpoint, dataDir string, restart func() error) ([]string, error) {
	local := copyGuardLocal(ep.spec())
	if err := local.Exec(ctx, "postgres", "SELECT pg_reload_conf();\n", nil); err != nil {
		return nil, err
	}
	pending, err := verifyCopyGuard(ctx, local, dataDir)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	if restart == nil {
		return nil, output.NewError(output.CodePrecondition,
			"the copy's guard still needs a restart for "+strings.Join(pending, ", ")+" and no safe owned-copy restart is available",
			output.ExitPrecondition, "The copy was not activated; fix the guard or retry the owned operation.").
			WithDetails(map[string]any{"reason": "extension_copy_guard_pending", "settings": pending})
	}
	if err := restart(); err != nil {
		return nil, err
	}
	pending, err = verifyCopyGuard(ctx, local, dataDir)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, output.NewError(output.CodePrecondition,
			"the copy's guard is still pending after a restart for "+strings.Join(pending, ", "),
			output.ExitPrecondition, "The copy was not activated; fix the guard or the cluster before retrying.").
			WithDetails(map[string]any{"reason": "extension_copy_guard_pending", "settings": pending})
	}
	return nil, nil
}

// verifyCopyGuard reads the block actually installed (the preload union) and
// checks the effective settings. A missing value that is pending a restart is
// returned as pending; any other difference is an error.
func verifyCopyGuard(ctx context.Context, local sqlRunner, dataDir string) ([]string, error) {
	guard, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	var pending []string
	if len(guard.Preloads) > 0 {
		rows, err := local.Query(ctx, "postgres",
			"SELECT current_setting('shared_preload_libraries') AS v, coalesce((SELECT pending_restart::text FROM pg_settings WHERE name = 'shared_preload_libraries'), 'false') AS p", nil)
		if err != nil {
			return nil, err
		}
		loaded := map[string]bool{}
		pendingRestart := false
		if len(rows) > 0 {
			loaded = steps.ParsePreloads(rows[0]["v"])
			pendingRestart = rows[0]["p"] == "true"
		}
		for _, p := range guard.Preloads {
			if loaded[p] {
				continue
			}
			if pendingRestart {
				pending = append(pending, "shared_preload_libraries")
				break
			}
			return nil, output.NewError(output.CodePrecondition,
				"the copy's guard requires library "+p+", but it is not loaded and no restart is pending",
				output.ExitPrecondition,
				"Check the copy's postgresql.auto.conf and the package that provides "+p+"; the copy was not activated.").
				WithDetails(map[string]any{"reason": "extension_copy_guard_inactive", "library": p})
		}
	}
	for _, key := range guard.SettingKeys() {
		want := catalog.NormalizedSettingValue(guard.Settings[key])
		rows, err := local.Query(ctx, "postgres",
			"SELECT current_setting("+extensionLiteral(key)+") AS v, coalesce((SELECT vartype FROM pg_settings WHERE name = "+extensionLiteral(key)+"), '') AS vartype, coalesce((SELECT pending_restart::text FROM pg_settings WHERE name = "+extensionLiteral(key)+"), 'false') AS p", nil)
		if err != nil {
			return nil, output.NewError(output.CodePrecondition,
				"the copy's guard setting "+key+" is not a valid PostgreSQL setting: "+err.Error(),
				output.ExitPrecondition, "Fix the extension descriptor's copy_settings; the copy was not activated.").
				WithDetails(map[string]any{"reason": "extension_copy_guard_invalid", "setting": key})
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("reading the copy's %s returned no row", key)
		}
		if steps.ExtensionSettingMatches(want, steps.ParseExtensionSettingFact(rows[0])) {
			continue
		}
		if rows[0]["p"] == "true" {
			pending = append(pending, key)
			continue
		}
		return nil, output.NewError(output.CodePrecondition,
			"the copy's guard requires "+key+" = "+want+", but PostgreSQL reports "+rows[0]["v"],
			output.ExitPrecondition,
			"Check the copy's postgresql.auto.conf; the copy was not activated.").
			WithDetails(map[string]any{"reason": "extension_copy_guard_inactive", "setting": key})
	}
	sort.Strings(pending)
	return pending, nil
}

// verifyStartedCopyGuard proves an installed guard effective after a copy's
// start. It restarts the owned copy if a postmaster setting is pending and
// rechecks; a still-pending guard refuses routing.
func verifyStartedCopyGuard(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec) error {
	dataDir := cfg.Server.DataDirectory(spec.Name)
	if _, err := os.Stat(dataDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	ep := liveCopyGuardEndpoint(spec)
	if !clusterRunning(spec) {
		return nil
	}
	_, err := verifyGuardEffective(ctx, ep, dataDir, func() error {
		return backup.RestartCluster(ctx, env.shell(), env.Job, env.Report, spec.Name, dataDir, spec.Port)
	})
	return err
}

// cronSettlement is what a copy's metadata inspection proved.
type cronSettlement struct {
	MetadataDB  string
	MetadataOID string
	Deactivated int
}

// cronMetadataIdentity reads the actual effective cron.database_name and its
// catalog OID in the owned copy. The OID survives a rename; the name alone is
// not durable proof.
func cronMetadataIdentity(ctx context.Context, local sqlRunner) (cronSettlement, error) {
	rows, err := local.Query(ctx, "postgres",
		"SELECT current_setting('cron.database_name') AS db, coalesce((SELECT oid::text FROM pg_database WHERE datname = current_setting('cron.database_name')), '') AS oid", nil)
	if err != nil {
		return cronSettlement{}, cronUnknown("the copy's cron metadata database cannot be read: " + err.Error())
	}
	if len(rows) == 0 || strings.TrimSpace(rows[0]["db"]) == "" {
		return cronSettlement{}, cronUnknown("the copy's cron.database_name is empty")
	}
	if strings.TrimSpace(rows[0]["oid"]) == "" {
		return cronSettlement{}, cronUnknown("the copy's cron metadata database does not exist in its catalog")
	}
	return cronSettlement{MetadataDB: strings.TrimSpace(rows[0]["db"]), MetadataOID: strings.TrimSpace(rows[0]["oid"])}, nil
}

// cronMetadataCurrentName resolves a recorded metadata OID to its current
// name. A missing OID is unknown, not a guess from a stale setting.
func cronMetadataCurrentName(ctx context.Context, local sqlRunner, s cronSettlement) (string, error) {
	if s.MetadataOID == "" {
		if s.MetadataDB == "" {
			return "", cronUnknown("the copy's cron metadata database identity is unknown")
		}
		return s.MetadataDB, nil
	}
	rows, err := local.Query(ctx, "postgres",
		"SELECT datname AS db FROM pg_database WHERE oid = :'oid'", map[string]string{"oid": s.MetadataOID})
	if err != nil {
		return "", cronUnknown("the copy's cron metadata database identity cannot be read: " + err.Error())
	}
	if len(rows) == 0 || strings.TrimSpace(rows[0]["db"]) == "" {
		return "", cronUnknown("the copy's recorded cron metadata database no longer exists")
	}
	return strings.TrimSpace(rows[0]["db"]), nil
}

// cronUnknown is the fail-closed error for an unreadable or ambiguous cron
// metadata state.
func cronUnknown(message string) error {
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition,
		"Set cron.database_name to a database that exists in the copy, or remove pg_cron from the copy's requirement; the copy was not activated.").
		WithDetails(map[string]any{"reason": "cron_metadata_unknown"})
}

func cronJobTable(ctx context.Context, local sqlRunner, metadataDB string) (bool, error) {
	rows, err := local.Query(ctx, metadataDB, "SELECT to_regclass('cron.job') IS NOT NULL AS present", nil)
	if err != nil {
		return false, cronUnknown("the copy's cron metadata database " + metadataDB + " cannot be read: " + err.Error())
	}
	if len(rows) == 0 || rows[0]["present"] != "true" {
		return false, cronUnknown("the copy's cron metadata database " + metadataDB + " has no cron.job table")
	}
	return true, nil
}

func cronJobColumns(ctx context.Context, local sqlRunner, metadataDB string) error {
	rows, err := local.Query(ctx, metadataDB,
		"SELECT column_name AS name FROM information_schema.columns WHERE table_schema = 'cron' AND table_name = 'job' AND column_name IN ('jobid', 'active', 'database')", nil)
	if err != nil {
		return cronUnknown("the copy's cron.job columns cannot be read: " + err.Error())
	}
	have := map[string]bool{}
	for _, row := range rows {
		have[row["name"]] = true
	}
	if !have["active"] || !have["database"] {
		return cronUnknown("the copy's cron.job has no active/database columns; vaire will not guess a schema version")
	}
	return nil
}

// deactivateCopiedCronJobs deactivates every active job in the owned copy and
// verifies the result. It is idempotent.
func deactivateCopiedCronJobs(ctx context.Context, local sqlRunner) (cronSettlement, error) {
	return deactivateCopiedCronJobsAt(ctx, local, nil)
}

// deactivateCopiedCronJobsAt deactivates jobs in the metadata database a
// durable checkpoint recorded, resolving its OID to the current name first.
func deactivateCopiedCronJobsAt(ctx context.Context, local sqlRunner, recorded *backup.UnfollowCron) (cronSettlement, error) {
	var s cronSettlement
	if recorded != nil && (recorded.MetadataOID != "" || recorded.MetadataDB != "") {
		s = cronSettlement{MetadataDB: recorded.MetadataDB, MetadataOID: recorded.MetadataOID}
	} else {
		var err error
		s, err = cronMetadataIdentity(ctx, local)
		if err != nil {
			return cronSettlement{}, err
		}
	}
	metadataDB, err := cronMetadataCurrentName(ctx, local, s)
	if err != nil {
		return cronSettlement{}, err
	}
	s.MetadataDB = metadataDB
	if _, err := cronJobTable(ctx, local, metadataDB); err != nil {
		return cronSettlement{}, err
	}
	if err := cronJobColumns(ctx, local, metadataDB); err != nil {
		return cronSettlement{}, err
	}
	rows, err := local.Query(ctx, metadataDB, "UPDATE cron.job SET active = false WHERE active RETURNING jobid", nil)
	if err != nil {
		return cronSettlement{}, cronUnknown("deactivating the copy's cron jobs failed: " + err.Error())
	}
	check, err := local.Query(ctx, metadataDB, "SELECT count(*)::text AS n FROM cron.job WHERE active", nil)
	if err != nil || len(check) == 0 || check[0]["n"] != "0" {
		return cronSettlement{}, cronUnknown("the copy's cron jobs are still active after deactivation")
	}
	s.Deactivated = len(rows)
	return s, nil
}

// retargetCopiedCronJobs points only the copied jobs that named the old
// application database at the new name. Jobs in another metadata database are
// untouched. It is idempotent.
func retargetCopiedCronJobs(ctx context.Context, local sqlRunner, s cronSettlement, oldDB, newDB string) (int, error) {
	if oldDB == newDB {
		return 0, nil
	}
	metadataDB := s.MetadataDB
	if metadataDB == oldDB {
		metadataDB = newDB
	}
	if _, err := cronJobTable(ctx, local, metadataDB); err != nil {
		return 0, err
	}
	if err := cronJobColumns(ctx, local, metadataDB); err != nil {
		return 0, err
	}
	rows, err := local.Query(ctx, metadataDB,
		"UPDATE cron.job SET database = :'new' WHERE database = :'old' RETURNING jobid",
		map[string]string{"old": oldDB, "new": newDB})
	if err != nil {
		return 0, cronUnknown("retargeting the copy's cron jobs failed: " + err.Error())
	}
	check, err := local.Query(ctx, metadataDB,
		"SELECT count(*)::text AS n FROM cron.job WHERE database = :'old'", map[string]string{"old": oldDB})
	if err != nil || len(check) == 0 || check[0]["n"] != "0" {
		return 0, cronUnknown("the copy's cron jobs still name the old database " + oldDB)
	}
	return len(rows), nil
}

// checkCronMetadataSetting verifies the permanent configuration points at the
// metadata database that was actually renamed. A fixed old name is refused; a
// {db} substitution renders the new name.
func checkCronMetadataSetting(ctx context.Context, local sqlRunner, oldDB, newDB string) error {
	if oldDB == newDB {
		return nil
	}
	rows, err := local.Query(ctx, "postgres", "SELECT current_setting('cron.database_name') AS db", nil)
	if err != nil {
		return cronUnknown("the promoted copy's cron.database_name cannot be read: " + err.Error())
	}
	if len(rows) == 0 {
		return cronUnknown("the promoted copy's cron.database_name is empty")
	}
	got := strings.TrimSpace(rows[0]["db"])
	if got == oldDB {
		return output.NewError(output.CodePrecondition,
			"cron.database_name still names "+oldDB+", which was renamed to "+newDB,
			output.ExitPrecondition,
			"Change the descriptor's cron.database_name to {db} or "+newDB+", then run vaire apply; routing stays blocked.").
			WithDetails(map[string]any{"reason": "cron_database_name_stale", "database": newDB, "metadata_database": got})
	}
	return nil
}

// reconcileDesiredExtensionSQL creates the desired SQL extensions a writable
// promoted copy lacks, using the same SQL the roles step uses. It never runs
// on a standby, never updates a version, never drops an extra and never
// creates a historical-only extension.
func reconcileDesiredExtensionSQL(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, local sqlRunner, db string) ([]string, error) {
	rows, err := local.Query(ctx, db, "SELECT pg_is_in_recovery()::text AS r", nil)
	if err != nil {
		return nil, output.NewError(output.CodePrecondition,
			"reading "+spec.Name+"'s recovery state failed: "+err.Error(), output.ExitPrecondition,
			"The copy stays suppressed and unrouted; retry after fixing the cluster.").
			WithDetails(map[string]any{"reason": "extension_reconcile_unavailable", "database": spec.Name})
	}
	if len(rows) == 0 || rows[0]["r"] != "false" {
		return nil, output.NewError(output.CodePrecondition,
			spec.Name+" is still a standby, so vaire will not create extensions on it",
			output.ExitPrecondition, "The copy stays suppressed and unrouted; promote it before reconciling desired SQL.").
			WithDetails(map[string]any{"reason": "extension_reconcile_standby", "database": spec.Name})
	}
	installedRows, err := local.Query(ctx, db, "SELECT extname AS name FROM pg_extension ORDER BY 1", nil)
	if err != nil {
		return nil, output.NewError(output.CodePrecondition,
			"reading "+spec.Name+"'s installed extensions failed: "+err.Error(), output.ExitPrecondition,
			"The copy stays suppressed and unrouted; retry after fixing the cluster.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name})
	}
	installed := map[string]bool{}
	for _, row := range installedRows {
		installed[row["name"]] = true
	}
	sql, created, err := steps.DesiredExtensionSQL(cfg, spec, installed, db)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sql) != "" {
		if err := local.Exec(ctx, db, sql, nil); err != nil {
			return nil, output.WrapError(output.CodeStepFailed,
				fmt.Errorf("creating the desired extensions in %s: %w", spec.Name, err), output.ExitStep,
				"The copy stays suppressed and unrouted; read vaire logs postgres --db "+spec.Name+" and retry.").
				WithDetails(map[string]any{"reason": "extension_sql_failed", "database": spec.Name, "created": created})
		}
	}
	if len(created) > 0 {
		after, err := local.Query(ctx, db, "SELECT extname AS name FROM pg_extension ORDER BY 1", nil)
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		for _, row := range after {
			have[row["name"]] = true
		}
		for _, name := range created {
			if !have[extensionSQLName(cfg, spec, name)] {
				return nil, output.NewError(output.CodeStepFailed,
					"the desired extension "+name+" is still absent from "+spec.Name+" after CREATE EXTENSION",
					output.ExitStep, "The copy stays suppressed and unrouted; read vaire logs postgres --db "+spec.Name+".").
					WithDetails(map[string]any{"reason": "extension_not_created", "database": spec.Name, "extension": name})
			}
		}
	}
	return created, nil
}

func extensionSQLName(cfg config.Cluster, spec config.ClusterSpec, canonical string) string {
	if ext, err := cfg.ExtensionResolver().Resolve(canonical); err == nil {
		return ext.ExtName
	}
	return canonical
}

// checkRestoredExtensions classifies the extensions the restored data holds.
// Unknown or undeclared native code blocks activation; known extras and
// retained history are reported, never dropped or upgraded.
func checkRestoredExtensions(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, plan *backup.CopyPlan, local sqlRunner, db string) ([]output.Warning, error) {
	rows, err := local.Query(ctx, db, "SELECT extname AS name FROM pg_extension ORDER BY 1", nil)
	if err != nil {
		return nil, output.NewError(output.CodePrecondition,
			"reading the restored extensions of "+spec.Name+" failed: "+err.Error(), output.ExitPrecondition,
			"The copy stays suppressed and unrouted; retry after fixing the cluster.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name})
	}
	desired := map[string]bool{}
	entries, err := steps.DesiredExtensionNames(cfg, spec)
	if err != nil {
		return nil, err
	}
	for _, ext := range entries {
		desired[ext.ExtName] = true
	}
	known := map[string]catalog.Extension{}
	if plan != nil {
		for _, ext := range plan.Requirements {
			if ext.ExtName != "" {
				known[ext.ExtName] = ext
			}
		}
	}
	defs := map[string]catalog.Extension{}
	if list, err := cfg.ExtensionResolver().List(); err == nil {
		for _, ext := range list {
			if ext.ExtName != "" {
				defs[ext.ExtName] = ext
			}
		}
	}
	var warnings []output.Warning
	var unknown []string
	for _, row := range rows {
		name := row["name"]
		if name == "plpgsql" || desired[name] {
			continue
		}
		if ext, ok := known[name]; ok {
			warnings = append(warnings, output.Warning{Code: "extension_retained",
				Message: spec.Name + "'s restored data has " + name + " from its retained history; vaire keeps its library and copy settings, and does not create or drop it. Add [extensions." + ext.Name + "] to the database's intent to make it desired."})
			continue
		}
		if ext, ok := defs[name]; ok {
			if ext.CopySafety == catalog.CopySafetyUnknown {
				unknown = append(unknown, name)
				continue
			}
			warnings = append(warnings, output.Warning{Code: "extension_not_in_config",
				Message: spec.Name + "'s restored data has " + name + ", which cluster.toml does not enable; vaire keeps it and does not drop it. Add it to extensions to manage it."})
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) > 0 {
		return warnings, output.NewError(output.CodePrecondition,
			"the restored data of "+spec.Name+" has extensions with no safe declaration: "+strings.Join(unknown, ", "),
			output.ExitPrecondition,
			"Declare each in [extensions.<name>] with copy_safety and standby_safe, or remove it from the data; the copy stays suppressed and unrouted.").
			WithDetails(map[string]any{"reason": "extension_unknown", "database": spec.Name, "extensions": unknown})
	}
	return warnings, nil
}

// pruneCopyGuardForSettlement rewrites the guard after settlement. It keeps
// only what the permanent configuration still does not supply: retained
// libraries and historical-only settings, plus a mandatory suppression that
// has not been handed off to verified equivalent configuration. An independent
// copy persists its disabling settings first, so those keys are released and
// recorded as handed off; a same-database restore keeps a historical-only
// suppression until the operator supplies the equivalent permanent value.
func pruneCopyGuardForSettlement(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, dataDir string, plan *backup.CopyPlan, independent bool, db string, restart func() error) ([]output.Warning, error) {
	_ = independent // the permanent settings decide the per-key handoff, not a blanket flag
	if plan == nil {
		return nil, nil
	}
	desired := map[string]bool{}
	entries, err := steps.DesiredExtensionNames(cfg, spec)
	if err != nil {
		return nil, err
	}
	for _, ext := range entries {
		desired[ext.Name] = true
	}
	var retained []catalog.Extension
	for _, ext := range plan.Requirements {
		if desired[ext.Name] {
			continue
		}
		retained = append(retained, ext)
	}
	_, base, err := backup.MergeCopyRequirements(retained, nil, false, db)
	if err != nil {
		return nil, err
	}
	resolver := cfg.ExtensionResolver()
	currentNames, err := resolver.EffectiveNames(spec.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	current, err := resolveExtensions(resolver, currentNames)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	known := append(append([]catalog.Extension(nil), current...), retained...)
	permanent := permanentExtensionSettings(cfg, spec, current)
	actualPermanent, err := backup.EffectivePermanentSettings(steps.PGConfDir(spec.Name), dataDir)
	if err != nil {
		return nil, output.NewError(output.CodePrecondition,
			"the permanent configuration's effective precedence for "+spec.Name+" cannot be proven: "+err.Error(),
			output.ExitPrecondition,
			"Fix the cluster's configuration files; vaire will not release a copy suppression without proof.").
			WithDetails(map[string]any{"reason": "extension_permanent_unproven", "database": spec.Name})
	}
	instance, err := currentFollowInstance()
	if err != nil {
		return nil, err
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	decision, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: known, Current: current, Permanent: permanent,
		Explicit: spec.Settings, CopyDB: db, Pending: !independent,
		Handoff: recs.GuardHandoffFor(db, instance, spec.Stanza), HandoffPermanent: actualPermanent,
	})
	if err != nil {
		return nil, err
	}
	base.Settings = decision.Settings
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.SetGuardHandoff(db, instance, spec.Stanza, decision.Handoff)
		return nil
	}); err != nil {
		return nil, err
	}
	desiredPreloads, err := resolver.Preloads(spec.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	ep := liveCopyGuardEndpoint(spec)
	guardPlan := &backup.CopyPlan{
		Guard: base, DesiredPreloads: desiredPreloads,
		ValidateGuard: finalPreloadValidator(cfg, spec, db, known, permanent, false, decision.Handoff),
	}
	if _, _, err := backup.InstallCopyPlanGuard(ep.ConfDir, dataDir, guardPlan); err != nil {
		return nil, err
	}
	if !clusterRunning(spec) {
		return nil, nil
	}
	if _, err := verifyGuardEffective(ctx, ep, dataDir, restart); err != nil {
		return nil, err
	}
	return nil, nil
}

// persistCopySettings writes the mandatory disabling settings of an
// independent copy into its explicit [clusters.settings]. A conflicting
// inherited ordinary value is overridden with a visible warning; a conflict
// between two descriptors' mandatory settings was already refused.
func persistCopySettings(name string, plan *backup.CopyPlan, db string) (config.Cluster, []output.Warning, error) {
	if plan == nil {
		return config.Cluster{}, nil, nil
	}
	cfg, _, err := followerState().Load()
	if err != nil {
		return config.Cluster{}, nil, err
	}
	spec, ok := cfg.ClusterByName(name)
	if !ok {
		return cfg, nil, databaseNotFound(name)
	}
	var warnings []output.Warning
	settings := spec.Clone().Settings
	if settings == nil {
		settings = map[string]string{}
	}
	changed := false
	for _, ext := range plan.Requirements {
		if ext.CopySafety != catalog.CopySafetyDisabled {
			continue
		}
		for k, v := range ext.ResolvedCopySettings(db) {
			if config.IsReservedSetting(k) {
				return cfg, nil, output.NewError(output.CodePrecondition,
					"extension "+ext.Name+"'s copy setting "+k+" is managed by vaire and cannot be persisted",
					output.ExitPrecondition, "Choose a setting the extension owns; the copy was not made independent.").
					WithDetails(map[string]any{"reason": "extension_copy_setting_reserved", "database": name, "setting": k})
			}
			raw := catalog.NormalizedSettingValue(v)
			if prev, ok := settings[k]; ok && catalog.NormalizedSettingValue(prev) != raw {
				warnings = append(warnings, output.Warning{Code: "extension_copy_setting_override",
					Message: name + "'s inherited setting " + k + " = " + prev + " was overridden with " + raw + " to keep the copy's " + ext.Name + " background work disabled; change it deliberately after settlement"})
			}
			if settings[k] != raw {
				settings[k], changed = raw, true
			}
		}
	}
	if !changed {
		return cfg, warnings, nil
	}
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == name {
			cfg.Clusters[i].Settings = settings
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, nil, config.ValidationError(err)
	}
	raw, err := cfg.Encode()
	if err != nil {
		return cfg, nil, err
	}
	if err := followerState().Save(raw); err != nil {
		return cfg, nil, err
	}
	return cfg, warnings, nil
}

// settleRestoredCopy is the same-database restore's activation sequence:
// deactivate copied jobs, create missing desired SQL, block unknown
// extensions, prune the guard to retained history, and only then let the
// caller resume routing.
func settleRestoredCopy(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, local sqlRunner, dataDir string, plan *backup.CopyPlan, db string, restart func() error) ([]output.Warning, error) {
	var warnings []output.Warning
	if _, err := reconcileCopySQL(ctx, cfg, spec, local, db); err != nil {
		return nil, err
	}
	if plan != nil && plan.Cron {
		if _, err := deactivateCopyCron(ctx, local); err != nil {
			return nil, err
		}
	}
	more, err := checkCopyExtensions(ctx, cfg, spec, plan, local, db)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, more...)
	more, err = pruneCopyGuard(ctx, cfg, spec, dataDir, plan, false, db, restart)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, more...)
	return warnings, nil
}
