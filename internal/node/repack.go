package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/maint"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
)

// pg_repack (Phase 7): manual and scheduled repack. The client runs as
// postgres over the cluster's socket on an unprotected database. Protected
// databases are refused before any query or mutation; no TOTP or grant path
// makes an unrestricted postgres repack acceptable there.

// repackExecResult carries pg_repack's combined redacted output and process
// error. Completion is classified from process status, typed skip lines and
// diagnostic storage evidence; storage alone never proves a rewrite.
type repackExecResult struct {
	Output string
	Err    error
}

// Seams keep host and shell effects out of unit tests without production
// fault hooks. Every one has a production default.
var (
	// maintRepackPG is the unprivileged read-only catalog connection. It can
	// see pg_extension, pg_proc, pg_class, pg_index, pg_namespace and
	// pg_tablespace, but not call pg_repack's version functions when the
	// extension schema has no PUBLIC USAGE.
	maintRepackPG = maintRepackAdminPG
	// maintRepackPrivilegedPG is the existing postgres peer connection. It is
	// used for one fixed version/default-tablespace query only after the current
	// target has been revalidated as unprotected under the job's locks.
	maintRepackPrivilegedPG  = maintRepackPostgresPG
	maintRepackClientVersion = func(ctx context.Context) (string, error) {
		return steps.Probe(ctx, maint.RepackBin, "--version")
	}
	maintRepackSpace = backup.CheckSpace
	autoRepackLocks  = acquireAutoRepackLocks
	autoRepackNow    = time.Now
	repackNow        = time.Now
	reloadJobCluster = func() (config.Cluster, error) {
		return config.LoadCluster(config.ClusterPath)
	}
	maintRepackExec = func(ctx context.Context, env *jobEnv, spec config.ClusterSpec, table string) repackExecResult {
		args, err := maint.RepackArgs(steps.PGSocketDir, spec.Port, spec.DatabaseName(), table)
		if err != nil {
			return repackExecResult{Err: err}
		}
		logLine(env.Report, env.Job, "pg_repack "+strings.Join(args, " "))
		var lines []string
		execErr := env.shell().Exec(ctx, env.Job, engine.Command{
			Name: "runuser", Args: args,
			OnLine: func(line string) { lines = append(lines, line) },
		}, env.Report)
		return repackExecResult{Output: secrets.Redact(strings.Join(lines, "\n")), Err: execErr}
	}
)

func maintRepackAdminPG(spec config.ClusterSpec) sqlRunner { return steps.AdminPG(spec) }

func maintRepackPostgresPG(spec config.ClusterSpec) sqlRunner { return steps.ClusterPG(spec) }

// maintRunToken is maint.run's confirm token: <db> for a repack, none for the
// other runs.
func maintRunToken(_ config.Cluster, params json.RawMessage) string {
	var p maint.Options
	if json.Unmarshal(params, &p) != nil || p.Kind != maint.Repack {
		return ""
	}
	return p.DB
}

// ProtectedRepackError is the bounded fallback for a protected database:
// repack needs table-owner or superuser access that vaire does not grant
// there, and a code from the authenticator app must not turn into one.
func ProtectedRepackError(db string) error {
	return output.NewError(output.CodePrecondition,
		"repacking "+db+" is not supported while it is protected", output.ExitPrecondition,
		"Repack needs table-owner or superuser access that vaire does not grant on a protected database. Use vaire maint vacuum, analyze or reindex, which keep their limited role path.").
		WithDetails(map[string]any{"reason": maint.RepackReasonProtected, "database": db})
}

func targetIdentityChanged(expected, current config.ClusterSpec) bool {
	return expected.Name != current.Name ||
		expected.Node != current.Node ||
		expected.Port != current.Port ||
		expected.Stanza != current.Stanza ||
		expected.Follows != current.Follows ||
		expected.FollowGeneration != current.FollowGeneration
}

// reloadRepackTarget reloads cluster.toml under the job's acquired locks and
// returns the current target. A changed name/node/port/stanza/follow identity
// invalidates the locks the job already holds, so it refuses before effects.
func reloadRepackTarget(expected config.ClusterSpec) (config.Cluster, config.ClusterSpec, error) {
	cfg, err := reloadJobCluster()
	if err != nil {
		return config.Cluster{}, config.ClusterSpec{}, err
	}
	current, ok := cfg.ClusterByName(expected.Name)
	if !ok {
		return config.Cluster{}, config.ClusterSpec{}, output.NewError(output.CodePrecondition,
			"the target "+expected.Name+" disappeared while the repack waited for its locks", output.ExitPrecondition,
			"Retry after the configuration settles; vaire will not act on a database it can no longer identify.").
			WithDetails(map[string]any{"reason": "config_changed", "database": expected.Name})
	}
	if targetIdentityChanged(expected, current) {
		return config.Cluster{}, config.ClusterSpec{}, output.NewError(output.CodePrecondition,
			"the target identity of "+expected.Name+" changed while the repack waited for its locks", output.ExitPrecondition,
			"Retry the repack; the port, stanza, node or follower identity the locks cover is no longer current.").
			WithDetails(map[string]any{
				"reason": "config_changed", "database": expected.Name,
				"expected_port": expected.Port, "current_port": current.Port,
				"expected_stanza": expected.Stanza, "current_stanza": current.Stanza,
				"expected_follows": expected.Follows, "current_follows": current.Follows,
			})
	}
	return cfg, current, nil
}

// repackTarget is the admitted relation identity. Name is the canonical
// schema.table resolved from the catalog; OID is the same relation's OID. All
// later probes and pg_repack's argv use Name, never the caller's possibly
// unqualified text, so a different session search_path cannot select a
// same-named relation in another schema.
type repackTarget struct {
	Name string
	OID  int64
}

// repackPlan is a validated manual repack request. Skipped holds no-table
// candidates that failed relation/key/space validation; a named table is
// refused instead of skipped.
type repackPlan struct {
	Tables       []string             `json:"tables"`
	Targets      []repackTarget       `json:"-"`
	Skipped      []maint.RepackSkip   `json:"skipped,omitempty"`
	Versions     maint.RepackVersions `json:"versions"`
	VersionCheck string               `json:"version_check,omitempty"`
	Space        []backup.SpaceNeed   `json:"space,omitempty"`
	Deadline     time.Time            `json:"-"`
}

func repackExtensionMissing(db, extension, why string) error {
	return output.NewError(output.CodePrecondition,
		extension+" is not installed in "+db, output.ExitPrecondition,
		why+": vaire ext add "+extension+" --db "+db+".").
		WithDetails(map[string]any{"reason": maint.RepackReasonExtensionMissing, "extension": extension, "database": db})
}

func repackClientMissing(err error) error {
	return output.NewError(output.CodePrecondition,
		"the PostgreSQL 18 pg_repack client is not available", output.ExitPrecondition,
		"Install the pg_repack package for PostgreSQL 18 (vaire ext add pg_repack --db <db>) so "+maint.RepackBin+" exists.").
		WithDetails(map[string]any{"reason": maint.RepackReasonClientMissing, "binary": maint.RepackBin, "error": err.Error()})
}

func repackVersionUnknown(db, installed, why string) error {
	return output.NewError(output.CodePrecondition,
		"pg_repack is installed in "+db+" at "+installed+", but its version facts are unavailable", output.ExitPrecondition,
		why+"; recreate or upgrade the extension from a matching PostgreSQL 18 package, then retry.").
		WithDetails(map[string]any{"reason": maint.RepackReasonVersionMismatch, "database": db, "extension_version": installed})
}

func repackVersionMismatch(db string, v maint.RepackVersions) error {
	return output.NewError(output.CodePrecondition,
		"pg_repack's client, extension, library and SQL versions do not match in "+db, output.ExitPrecondition,
		"Install the pg_repack package that matches the extension, or recreate the extension from the matching package; vaire does not upgrade or downgrade it automatically.").
		WithDetails(map[string]any{
			"reason": maint.RepackReasonVersionMismatch, "database": db,
			"versions": v,
		})
}

func repackOutsideWindow(cfg config.Cluster, db string) error {
	return output.NewError(output.CodePrecondition,
		"it is outside the maintenance window ("+cfg.Maintenance.Window+")", output.ExitPrecondition,
		"Run the repack in the window, or pass --now: a repack rewrites the table and its indexes.").
		WithDetails(map[string]any{"reason": maint.RepackReasonOutsideWindow, "database": db, "window": cfg.Maintenance.Window})
}

func repackWindowEnded(db string) error {
	return output.NewError(output.CodePrecondition,
		"the maintenance window ended before pg_repack could start", output.ExitPrecondition,
		"Retry in the next window, or pass --now for an explicit manual run.").
		WithDetails(map[string]any{"reason": maint.RepackReasonWindowEnded, "database": db})
}

func repackNotRunning(db string) error {
	return output.NewError(output.CodePrecondition,
		"the cluster of "+db+" is not running", output.ExitPrecondition,
		"Start it with vaire apply --db "+db+", then retry.").
		WithDetails(map[string]any{"reason": maint.RepackReasonClusterNotRunning, "database": db})
}

func repackProblemError(db, table string, problem maint.RepackSkip) error {
	hint := repackProblemHint(problem.Reason)
	return output.NewError(output.CodePrecondition, problem.Message, output.ExitPrecondition, hint).
		WithDetails(map[string]any{"reason": problem.Reason, "database": db, "table": table})
}

func repackProblemHint(reason string) string {
	switch reason {
	case maint.RepackReasonPartitionParent:
		return "Name an ordinary partition leaf; vaire does not repack a partitioned parent as one table."
	case maint.RepackReasonForeignTable:
		return "Foreign tables have no local table storage; maintain the foreign server instead."
	case maint.RepackReasonNoKey:
		return "Add a primary key or a unique index on NOT NULL key columns, or use vaire maint vacuum --db <db> --table <table>."
	case maint.RepackReasonExtensionStorage:
		return "This table, an ancestor or its schema belongs to an extension; vaire does not write extension-managed storage with pg_repack."
	case maint.RepackReasonTablespace:
		return "Move the table, its TOAST and all its indexes into pg_default (PGDATA), or use vaire maint vacuum; vaire does not repack across an unproven external tablespace."
	case maint.RepackReasonDefaultTablespace:
		return "Move the database default tablespace and the postgres session default_tablespace to pg_default; pg_repack creates auxiliary objects without an explicit tablespace."
	case maint.RepackReasonInsufficientSpace:
		return "Free space for about twice the table, index and TOAST footprint plus the host reserve, then retry."
	case maint.RepackReasonSpaceUnknown:
		return "The free-space probe could not read the actual PGDATA filesystem; fix the mount/permissions and retry. Repack does not proceed on unknown space."
	default:
		return "Choose an ordinary permanent heap table with a primary key or a suitable unique index."
	}
}

func repackReason(err error) string {
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) {
		if details, ok := cliErr.Details.(map[string]any); ok {
			if reason, ok := details["reason"].(string); ok && reason != "" {
				return reason
			}
		}
	}
	return maint.RepackReasonFailed
}

func repackFailed(run maint.RepackRun) error {
	return output.NewError(output.CodeExternalCommand,
		fmt.Sprintf("pg_repack failed on %d of %d table(s) in %s", len(run.Failed), len(run.Tables), run.DB),
		output.ExitExternal,
		"Read the job log (vaire job follow). A lock wait timeout or invalid index is reported as skipped; completion that cannot be attributed is reported as unknown, never as success or a rewrite claim.").
		WithDetails(map[string]any{"failed": run.Failed, "unknown": run.Unknown, "repacked": run.Repacked, "counts": run.Counts})
}

// repackFacts are the unprivileged catalog/client facts read before a
// privileged version probe.
type repackFacts struct {
	Installed           string
	Schema              string
	Client              string
	DBDefaultTablespace string
}

func defaultTablespaceSupported(dbDefault, sessionDefault string) bool {
	return dbDefault == "pg_default" && (sessionDefault == "" || sessionDefault == "pg_default")
}

func repackDefaultTablespaceError(db, dbDefault, sessionDefault string) error {
	return output.NewError(output.CodePrecondition,
		"the database or postgres session default tablespace of "+db+" is not verified as pg_default", output.ExitPrecondition,
		"Repack creates auxiliary objects without an explicit tablespace; move the database/session default to pg_default or use vaire maint vacuum. vaire does not override an unsafe default.").
		WithDetails(map[string]any{
			"reason": maint.RepackReasonDefaultTablespace, "database": db,
			"db_default_tablespace": dbDefault, "session_default_tablespace": sessionDefault,
		})
}

// checkRepackFacts reads the extension, version-function schema and client
// version through the unprivileged read-only connection. It never calls
// pg_repack's version functions.
func checkRepackFacts(ctx context.Context, spec config.ClusterSpec) (repackFacts, error) {
	local := maintRepackPG(spec)
	rows, err := local.Query(ctx, spec.DatabaseName(), maint.RepackInstalledSQL, nil)
	if err != nil {
		return repackFacts{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read pg_repack's catalog facts; the database is reachable.")
	}
	if len(rows) != 1 {
		return repackFacts{}, repackExtensionMissing(spec.Name, "pg_repack",
			"maint repack runs pg_repack, which needs its extension in the database")
	}
	facts := repackFacts{Installed: rows[0]["extversion"]}
	rows, err = local.Query(ctx, spec.DatabaseName(), maint.RepackDBDefaultTablespaceSQL, nil)
	if err != nil {
		return repackFacts{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the database default tablespace.")
	}
	if len(rows) != 1 {
		return repackFacts{}, repackDefaultTablespaceError(spec.Name, "", "")
	}
	facts.DBDefaultTablespace = rows[0]["tablespace"]
	if facts.DBDefaultTablespace != "pg_default" {
		return repackFacts{}, repackDefaultTablespaceError(spec.Name, facts.DBDefaultTablespace, "")
	}
	rows, err = local.Query(ctx, spec.DatabaseName(), maint.RepackExtensionSchemaSQL, nil)
	if err != nil {
		return repackFacts{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read pg_repack's version function schema.")
	}
	if len(rows) != 1 || rows[0]["schema"] == "" {
		return repackFacts{}, repackVersionUnknown(spec.Name, facts.Installed,
			"the installed extension has no readable version_sql function")
	}
	facts.Schema = rows[0]["schema"]
	client, err := maintRepackClientVersion(ctx)
	if err != nil {
		return repackFacts{}, repackClientMissing(err)
	}
	facts.Client = client
	return facts, nil
}

// checkRepackVersions calls pg_repack's version functions through the
// existing postgres peer connection. It must only run after the current
// target has been revalidated as unprotected and the job's locks are held.
func checkRepackVersions(ctx context.Context, spec config.ClusterSpec) (maint.RepackVersions, error) {
	if spec.Protected {
		return maint.RepackVersions{}, ProtectedRepackError(spec.Name)
	}
	if spec.IsFollower() {
		return maint.RepackVersions{}, followerError(spec, "maintenance")
	}
	facts, err := checkRepackFacts(ctx, spec)
	if err != nil {
		return maint.RepackVersions{}, err
	}
	rows, err := maintRepackPrivilegedPG(spec).Query(ctx, spec.DatabaseName(), maint.RepackVersionsSQL(facts.Schema), nil)
	if err != nil {
		return maint.RepackVersions{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read pg_repack's extension, library and SQL versions through the postgres peer connection.")
	}
	if len(rows) != 1 {
		return maint.RepackVersions{}, repackVersionUnknown(spec.Name, facts.Installed,
			"the installed extension has no readable version functions")
	}
	versions, err := maint.NormalizeRepackVersions(facts.Client, rows[0]["extension_version"], rows[0]["library_version"], rows[0]["sql_version"])
	if err != nil || !versions.Match() {
		return versions, repackVersionMismatch(spec.Name, versions)
	}
	versions.DBDefaultTablespace = rows[0]["db_default_tablespace"]
	versions.SessionDefaultTablespace = rows[0]["session_default_tablespace"]
	if !defaultTablespaceSupported(versions.DBDefaultTablespace, versions.SessionDefaultTablespace) {
		return versions, repackDefaultTablespaceError(spec.Name, versions.DBDefaultTablespace, versions.SessionDefaultTablespace)
	}
	return versions, nil
}

// inspectRepackTable resolves the admitted relation from the catalog and
// returns its canonical identity and total size, or a precise skip problem.
// The first lookup may be unqualified (the caller's table argument), but every
// later probe uses the resolved quoted schema.table. expectedOID is zero for a
// fresh admission; a non-zero expectedOID makes a changed relation a refusal.
func inspectRepackTable(ctx context.Context, spec config.ClusterSpec, table string, expectedOID int64) (int64, repackTarget, *maint.RepackSkip, error) {
	q, err := maint.QuoteRelation(table)
	if err != nil {
		return 0, repackTarget{}, &maint.RepackSkip{Table: table, Reason: maint.RepackReasonUnsupportedRelation, Message: err.Error()}, nil
	}
	local := maintRepackPG(spec)
	rows, err := local.Query(ctx, spec.DatabaseName(), maint.RelationFactsSQL(q), nil)
	if err != nil {
		return 0, repackTarget{}, nil, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the relation's kind, persistence and access method.")
	}
	if len(rows) == 0 {
		return 0, repackTarget{}, &maint.RepackSkip{Table: table, Reason: maint.RepackReasonRelationNotFound,
			Message: table + " does not exist in " + spec.Name}, nil
	}
	facts := maint.ParseRelationFacts(rows[0])
	if reason, message, unsupported := facts.UnsupportedOrdinaryTable(); unsupported {
		return 0, repackTarget{}, &maint.RepackSkip{Table: table, Reason: reason, Message: message}, nil
	}
	if expectedOID != 0 && facts.OID != expectedOID {
		return 0, repackTarget{}, &maint.RepackSkip{Table: table, Reason: maint.RepackReasonRelationChanged,
			Message: table + " is now OID " + strconv.FormatInt(facts.OID, 10) + ", not the admitted OID " + strconv.FormatInt(expectedOID, 10)}, nil
	}
	canonical := facts.Schema + "." + facts.Table
	cq, err := maint.QuoteRelation(canonical)
	if err != nil {
		return 0, repackTarget{}, &maint.RepackSkip{Table: table, Reason: maint.RepackReasonUnsupportedRelation,
			Message: "the resolved relation " + canonical + " is not a supported ordinary identifier: " + err.Error()}, nil
	}
	target := repackTarget{Name: canonical, OID: facts.OID}
	extRows, err := local.Query(ctx, spec.DatabaseName(), maint.ExtensionChainSQL(cq), nil)
	if err != nil {
		return 0, repackTarget{}, nil, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the relation's extension dependency chain.")
	}
	if deps := maint.ParseExtensionChain(extRows); len(deps) > 0 {
		d := deps[0]
		return 0, repackTarget{}, &maint.RepackSkip{Table: canonical, Reason: maint.RepackReasonExtensionStorage,
			Message: canonical + " is extension-managed (" + d.Extension + ", " + d.Scope + " at depth " + strconv.Itoa(d.Depth) + ")"}, nil
	}
	tsRows, err := local.Query(ctx, spec.DatabaseName(), maint.RepackTablespaceSQL(cq), nil)
	if err != nil {
		return 0, repackTarget{}, nil, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the table, TOAST and index tablespaces.")
	}
	if len(tsRows) == 0 {
		return 0, repackTarget{}, &maint.RepackSkip{Table: canonical, Reason: maint.RepackReasonRelationNotFound,
			Message: canonical + " has no readable tablespace facts"}, nil
	}
	for _, fact := range maint.ParseTablespaceFacts(tsRows) {
		if !fact.SupportedPGDATA() {
			return 0, repackTarget{}, &maint.RepackSkip{Table: canonical, Reason: maint.RepackReasonTablespace,
				Message: fact.Kind + " " + fact.Name + " is in tablespace " + fact.Tablespace + "; repack supports only pg_default (PGDATA)"}, nil
		}
	}
	keyRows, err := local.Query(ctx, spec.DatabaseName(), maint.RepackKeySQL(cq), nil)
	if err != nil {
		return 0, repackTarget{}, nil, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the relation's primary key or unique key facts.")
	}
	if len(keyRows) != 1 || !boolValue(keyRows[0]["ok"]) {
		return 0, repackTarget{}, &maint.RepackSkip{Table: canonical, Reason: maint.RepackReasonNoKey,
			Message: canonical + " has no primary key or unique index on NOT NULL key columns"}, nil
	}
	sizeRows, err := local.Query(ctx, spec.DatabaseName(), maint.RelationSizeSQL(cq), nil)
	if err != nil {
		return 0, repackTarget{}, nil, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the relation's total size.")
	}
	if len(sizeRows) != 1 {
		return 0, repackTarget{}, &maint.RepackSkip{Table: canonical, Reason: maint.RepackReasonRelationNotFound,
			Message: canonical + " has no readable size"}, nil
	}
	size, err := strconv.ParseInt(sizeRows[0]["n"], 10, 64)
	if err != nil {
		return 0, repackTarget{}, nil, output.WrapError(output.CodeExternalCommand, fmt.Errorf("relation size: %w", err), output.ExitExternal, "")
	}
	return size, target, nil, nil
}

func repackStorage(ctx context.Context, spec config.ClusterSpec, target repackTarget) (maint.RepackStorage, error) {
	q, err := maint.QuoteRelation(target.Name)
	if err != nil {
		return maint.RepackStorage{}, err
	}
	rows, err := maintRepackPG(spec).Query(ctx, spec.DatabaseName(), maint.RepackStorageSQL(q), nil)
	if err != nil {
		return maint.RepackStorage{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the relation's storage identity before or after the rewrite.")
	}
	if len(rows) != 1 {
		return maint.RepackStorage{}, output.NewError(output.CodePrecondition,
			target.Name+" has no readable storage identity", output.ExitPrecondition,
			"Retry after the relation is stable; without its OID and relfilenode vaire reports the attempt's completion as unknown.").
			WithDetails(map[string]any{"reason": maint.RepackReasonCompletionUnknown, "table": target.Name, "database": spec.Name})
	}
	return maint.ParseRepackStorage(rows[0]), nil
}

// verifyRepackTargetIdentity re-reads the admitted canonical relation
// immediately before execution. The check is deliberately not described as a
// DDL lock: no such lock is taken, but a same-named relation in another schema
// or a replaced OID is refused before pg_repack writes.
func verifyRepackTargetIdentity(ctx context.Context, spec config.ClusterSpec, target repackTarget) error {
	q, err := maint.QuoteRelation(target.Name)
	if err != nil {
		return err
	}
	rows, err := maintRepackPG(spec).Query(ctx, spec.DatabaseName(), maint.RelationFactsSQL(q), nil)
	if err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Read the admitted relation identity immediately before pg_repack.")
	}
	if len(rows) == 0 {
		return output.NewError(output.CodePrecondition, target.Name+" disappeared before pg_repack", output.ExitPrecondition,
			"Retry the repack after the relation is stable.").
			WithDetails(map[string]any{"reason": maint.RepackReasonRelationChanged, "table": target.Name, "database": spec.Name})
	}
	facts := maint.ParseRelationFacts(rows[0])
	if facts.OID != target.OID || facts.Schema+"."+facts.Table != target.Name {
		return output.NewError(output.CodePrecondition,
			target.Name+" changed identity immediately before pg_repack", output.ExitPrecondition,
			"Retry the repack after the relation is stable; vaire will not write a replaced relation.").
			WithDetails(map[string]any{
				"reason": maint.RepackReasonRelationChanged, "table": target.Name, "database": spec.Name,
				"expected_oid": target.OID, "actual_oid": facts.OID,
			})
	}
	return nil
}

func repackOutputSkipReason(output string) (string, bool) {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "skipping repack") && strings.Contains(lower, "timeout"):
		return maint.RepackReasonLockTimeout, true
	case strings.Contains(lower, "timed out, do not cancel conflicting backends"):
		return maint.RepackReasonLockTimeout, true
	case strings.Contains(lower, "unsafe queries remain"):
		return maint.RepackReasonUnsafeDDL, true
	case strings.Contains(lower, "invalid index:"):
		return maint.RepackReasonInvalidIndex, true
	}
	return "", false
}

// classifyRepackCompletion turns process status plus before/after storage and
// the controlled English client output into one truthful outcome. External
// process observables cannot attribute a completed rewrite: relfilenode
// changes are diagnostic only, and absence of a warning is never proof. A
// known skip transcript is skipped only when storage facts agree; any
// contradiction is unknown.
func classifyRepackCompletion(before maint.RepackStorage, res repackExecResult, after maint.RepackStorage, afterErr error) (outcome, reason, detail string) {
	storageKnown := afterErr == nil && before.OID > 0 && after.OID > 0 &&
		before.RelFileNode > 0 && after.RelFileNode > 0 &&
		before.TableSpace != "" && after.TableSpace != ""
	storageChanged := storageKnown && before != after
	skipReason, skipped := repackOutputSkipReason(res.Output)
	if res.Err != nil {
		detail := "completion_unknown/process_error: " + res.Err.Error()
		if storageChanged {
			detail += "; storage changed, but the rewrite completion is not attributable"
		}
		return maint.RepackOutcomeUnknown, maint.RepackReasonCompletionUnknown, detail
	}
	if skipped {
		if !storageKnown {
			return maint.RepackOutcomeUnknown, maint.RepackReasonCompletionUnknown,
				"completion_unknown/clean_exit: pg_repack reported a skip, but valid storage facts are unavailable; completion is unknown"
		}
		if storageChanged {
			return maint.RepackOutcomeUnknown, maint.RepackReasonCompletionUnknown,
				"completion_unknown/clean_exit: pg_repack reported a skip, but storage changed; completion is not attributable"
		}
		return maint.RepackOutcomeSkipped, skipReason, res.Output
	}
	if !storageKnown {
		return maint.RepackOutcomeUnknown, maint.RepackReasonCompletionUnknown,
			"completion_unknown/clean_exit: pg_repack exited successfully, but valid storage facts are unavailable; rewrite completion could not be confirmed"
	}
	if storageChanged {
		return maint.RepackOutcomeUnknown, maint.RepackReasonCompletionUnknown,
			"completion_unknown/clean_exit: pg_repack exited successfully and storage changed, but rewrite completion is not attributable from external observables"
	}
	return maint.RepackOutcomeUnknown, maint.RepackReasonCompletionUnknown,
		"completion_unknown/clean_exit: pg_repack exited successfully, but no completed rewrite could be attributed"
}

// revalidateRepackTable rechecks the facts that can make a previously
// validated table unsafe or ineligible: protection, follower state, running
// state, activation fence, versions, relation/key/storage and size. It keeps
// the admitted canonical relation and OID so a different search_path cannot
// select another same-named relation.
func revalidateRepackTable(ctx context.Context, cfg config.Cluster, expected, spec config.ClusterSpec, target repackTarget) (int64, repackTarget, *maint.RepackSkip, error) {
	if targetIdentityChanged(expected, spec) {
		return 0, repackTarget{}, &maint.RepackSkip{Table: target.Name, Reason: "config_changed",
			Message: "the target identity of " + expected.Name + " changed before this table started"}, nil
	}
	if spec.Protected {
		return 0, repackTarget{}, &maint.RepackSkip{Table: target.Name, Reason: maint.RepackReasonProtected,
			Message: "repack is not supported on a protected database"}, nil
	}
	if spec.IsFollower() {
		return 0, repackTarget{}, &maint.RepackSkip{Table: target.Name, Reason: "is_follower",
			Message: spec.Name + " is a follower; its source's tables are maintained on the source"}, nil
	}
	if !clusterRunning(spec) {
		return 0, repackTarget{}, &maint.RepackSkip{Table: target.Name, Reason: maint.RepackReasonClusterNotRunning,
			Message: "the cluster is not running"}, nil
	}
	if err := refuseAnyActivationFence(cfg, spec.Name); err != nil {
		return 0, repackTarget{}, &maint.RepackSkip{Table: target.Name, Reason: repackReason(err), Message: err.Error()}, nil
	}
	if _, err := checkRepackVersions(ctx, spec); err != nil {
		return 0, repackTarget{}, &maint.RepackSkip{Table: target.Name, Reason: repackReason(err), Message: err.Error()}, nil
	}
	return inspectRepackTable(ctx, spec, target.Name, target.OID)
}

func checkRepackSpace(cfg config.Cluster, spec config.ClusterSpec, table string, size int64) ([]backup.SpaceNeed, error) {
	need, err := maint.RepackSpaceNeed(size)
	if err != nil {
		return nil, err
	}
	needs, err := maintRepackSpace([]backup.SpaceNeed{{
		Path:    cfg.Server.DataDirectory(spec.Name),
		Need:    need,
		Purpose: "pg_repack's copy of " + table + " (twice its table, index and TOAST footprint, plus the host reserve)",
	}})
	if err != nil {
		return needs, err
	}
	for _, n := range needs {
		if n.Unknown {
			return needs, output.NewError(output.CodePrecondition,
				"the free space on "+n.Path+" could not be read, so a repack cannot prove its rewrite destination",
				output.ExitPrecondition,
				"Fix the PGDATA mount or permissions and retry; repack never proceeds on unknown free space.").
				WithDetails(map[string]any{"reason": maint.RepackReasonSpaceUnknown, "database": spec.Name, "table": table, "space": needs})
		}
	}
	return needs, nil
}

func boolValue(s string) bool { return s == "t" || s == "true" }

// planRepack validates a manual request and selects its tables. A named table
// is refused on any unsupported fact; without --table, unsupported candidates
// are skipped with reasons and the rest are planned.
func planRepack(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, o *maint.Options, now time.Time, privilegedVersionProbe bool) (repackPlan, error) {
	if spec.Protected {
		return repackPlan{}, ProtectedRepackError(spec.Name)
	}
	if spec.IsFollower() {
		return repackPlan{}, followerError(spec, "maintenance")
	}
	if !clusterRunning(spec) {
		return repackPlan{}, repackNotRunning(spec.Name)
	}
	versionCheck := "deferred_to_job"
	var versions maint.RepackVersions
	if privilegedVersionProbe {
		var err error
		versions, err = checkRepackVersions(ctx, spec)
		if err != nil {
			return repackPlan{}, err
		}
		versionCheck = "matched"
	} else {
		facts, err := checkRepackFacts(ctx, spec)
		if err != nil {
			return repackPlan{}, err
		}
		versions = maint.RepackVersions{Client: facts.Client, Extension: facts.Installed}
	}
	var deadline time.Time
	if !o.Now {
		in, left := windowLeft(cfg, now)
		if !in {
			return repackPlan{}, repackOutsideWindow(cfg, spec.Name)
		}
		deadline = now.Add(left)
	}
	var candidates []string
	named := o.Table != ""
	if named {
		candidates = []string{o.Table}
	} else {
		rows, err := maintRepackPG(spec).Query(ctx, spec.DatabaseName(), maint.ReportSetup+maint.BloatSQL(), nil)
		if err != nil {
			return repackPlan{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
				"Read the bloat estimate to choose tables for repack.")
		}
		candidates = maint.RepackCandidates(maint.ParseBloat(spec.Name, rows, 0), cfg.Maintenance.RepackBloatThreshold, 1)
	}
	plan := repackPlan{Versions: versions, VersionCheck: versionCheck, Deadline: deadline}
	for _, table := range candidates {
		size, target, problem, err := inspectRepackTable(ctx, spec, table, 0)
		if err != nil {
			if named {
				return repackPlan{}, err
			}
			plan.Skipped = append(plan.Skipped, maint.RepackSkip{Table: table, Reason: maint.RepackReasonFailed, Message: err.Error()})
			continue
		}
		if problem != nil {
			if named {
				return repackPlan{}, repackProblemError(spec.Name, table, *problem)
			}
			plan.Skipped = append(plan.Skipped, *problem)
			continue
		}
		space, err := checkRepackSpace(cfg, spec, target.Name, size)
		if err != nil {
			if named {
				return repackPlan{}, err
			}
			plan.Skipped = append(plan.Skipped, maint.RepackSkip{Table: target.Name, Reason: repackReason(err), Message: err.Error()})
			continue
		}
		plan.Tables = append(plan.Tables, target.Name)
		plan.Targets = append(plan.Targets, target)
		plan.Space = append(plan.Space, space...)
	}
	return plan, nil
}

// jobRepack revalidates and repacks the plan's tables one at a time. A named
// table failure fails the job; a no-table failure is reported after the other
// tables ran. It never kills another backend.
func jobRepack(ctx context.Context, env *jobEnv, p maint.Options, spec config.ClusterSpec) (any, error) {
	if err := refuseAnyActivationFence(env.Cluster, spec.Name); err != nil {
		return nil, err
	}
	plan, err := planRepack(ctx, env.Cluster, spec, &p, repackNow(), true)
	if err != nil {
		return nil, err
	}
	run := maint.RepackRun{DB: spec.Name, Tables: plan.Tables, Failed: map[string]string{}, Unknown: map[string]string{}}
	for _, skip := range plan.Skipped {
		run.Skipped = append(run.Skipped, skip)
		run.Counts.AddSkipped(skip.Reason)
	}
	for i, target := range plan.Targets {
		table := target.Name
		// Reload and revalidate under the job's locks immediately before this
		// table; the lock identity is the admission identity, not the config
		// loaded before the locks.
		freshCfg, freshSpec, err := reloadRepackTarget(spec)
		if err != nil {
			if p.Table != "" {
				return nil, err
			}
			run.Failed[table] = err.Error()
			run.Counts.AddFailed()
			continue
		}
		env.Cluster = freshCfg
		size, freshTarget, problem, err := revalidateRepackTable(ctx, freshCfg, spec, freshSpec, target)
		if err != nil {
			if p.Table != "" {
				return nil, err
			}
			run.Failed[table] = err.Error()
			run.Counts.AddFailed()
			continue
		}
		if problem != nil {
			if p.Table != "" {
				return nil, repackProblemError(freshSpec.Name, table, *problem)
			}
			run.Skipped = append(run.Skipped, *problem)
			run.Counts.AddSkipped(problem.Reason)
			continue
		}
		if _, err := checkRepackSpace(freshCfg, freshSpec, freshTarget.Name, size); err != nil {
			if p.Table != "" {
				return nil, err
			}
			run.Skipped = append(run.Skipped, maint.RepackSkip{Table: freshTarget.Name, Reason: repackReason(err), Message: err.Error()})
			run.Counts.AddSkipped(repackReason(err))
			continue
		}
		before, err := repackStorage(ctx, freshSpec, freshTarget)
		if err != nil {
			run.Unknown[table] = "completion_unknown/not_started: storage identity unavailable before execution: " + err.Error()
			run.Counts.AddUnknown()
			logLine(env.Report, env.Job, "pg_repack for "+table+" was not started; rewrite completion is unknown: "+err.Error())
			continue
		}
		// Re-read the admitted canonical relation immediately before the
		// write. This is not a DDL lock; it refuses a same-named relation in
		// another schema or a replaced OID instead of writing the wrong one.
		if err := verifyRepackTargetIdentity(ctx, freshSpec, freshTarget); err != nil {
			if p.Table != "" {
				return nil, err
			}
			run.Skipped = append(run.Skipped, maint.RepackSkip{Table: freshTarget.Name, Reason: repackReason(err), Message: err.Error()})
			run.Counts.AddSkipped(repackReason(err))
			continue
		}
		// The window is checked after every probe and immediately before
		// execution. Expiry starts no new rewrite; the job returns so its
		// caller releases the locks, and the running pg_repack is never killed.
		if !p.Now && !plan.Deadline.IsZero() && repackNow().After(plan.Deadline) {
			if p.Table != "" {
				return nil, repackWindowEnded(freshSpec.Name)
			}
			repackSkipRemaining(&run, plan.Targets, i, maint.RepackReasonWindowEnded,
				"the maintenance window ended while this table was being prepared")
			break
		}
		execRes := maintRepackExec(ctx, env, freshSpec, freshTarget.Name)
		after, afterErr := repackStorage(ctx, freshSpec, freshTarget)
		outcome, reason, detail := classifyRepackCompletion(before, execRes, after, afterErr)
		switch outcome {
		case maint.RepackOutcomeRepacked:
			run.Repacked = append(run.Repacked, table)
			run.Counts.AddRepacked()
			logLine(env.Report, env.Job, "repacked "+table+" in "+spec.Name)
		case maint.RepackOutcomeSkipped:
			run.Skipped = append(run.Skipped, maint.RepackSkip{Table: table, Reason: reason, Message: detail})
			run.Counts.AddSkipped(reason)
			logLine(env.Report, env.Job, "pg_repack skipped "+table+": "+reason)
		case maint.RepackOutcomeUnknown:
			run.Unknown[table] = detail
			run.Counts.AddUnknown()
			logLine(env.Report, env.Job, "pg_repack for "+table+": rewrite completion could not be confirmed: "+detail)
		default:
			run.Failed[table] = detail
			run.Counts.AddFailed()
			logLine(env.Report, env.Job, "pg_repack did not complete "+table+": "+detail)
		}
	}
	run.Counts.At = stamp()
	res := map[string]any{
		"db": spec.Name, "kind": maint.Repack, "tables": run.Tables,
		"repacked": run.Repacked, "failed": run.Failed, "unknown": run.Unknown,
		"skipped": run.Skipped, "counts": run.Counts, "versions": plan.Versions,
	}
	if len(run.Failed) > 0 {
		return res, repackFailed(run)
	}
	return res, nil
}

// acquireAutoRepackLocks takes the heavy-I/O lock and the cluster lock shared
// without waiting, in the existing node → heavy-I/O → cluster order. A busy
// lock returns its reason; the caller releases nothing.
func acquireAutoRepackLocks(ctx context.Context, env *jobEnv, spec config.ClusterSpec) (engine.Release, string, error) {
	heavy, err := engine.AcquireHeavyIO(ctx, env.Job, rpc.NewRequestID())
	if err != nil {
		if engine.IsLockHeld(err) {
			return nil, maint.RepackReasonHeavyIOBusy, nil
		}
		return nil, "", err
	}
	cluster, err := engine.AcquireCluster(ctx, spec.Name, engine.LockShared, env.Job, rpc.NewRequestID(), 0)
	if err != nil {
		heavy()
		if engine.IsLockHeld(err) {
			return nil, maint.RepackReasonClusterBusy, nil
		}
		return nil, "", err
	}
	return func() { cluster(); heavy() }, "", nil
}

func autoRepackSkip(run *maint.RepackRun, table, reason, message string) {
	run.Skipped = append(run.Skipped, maint.RepackSkip{Table: table, Reason: reason, Message: message})
	run.Counts.AddSkipped(reason)
}

func autoRepackBusy(run *maint.RepackRun, table, reason string) {
	run.Busy = append(run.Busy, maint.RepackSkip{Table: table, Reason: reason})
	run.Counts.AddBusy()
}

func autoRepackSkipRemaining(run *maint.RepackRun, candidates []string, from int, reason, message string) {
	for _, table := range candidates[from:] {
		autoRepackSkip(run, table, reason, message)
	}
}

func repackSkipRemaining(run *maint.RepackRun, targets []repackTarget, from int, reason, message string) {
	for _, target := range targets[from:] {
		run.Skipped = append(run.Skipped, maint.RepackSkip{Table: target.Name, Reason: reason, Message: message})
		run.Counts.AddSkipped(reason)
	}
}

func repackDBWideReason(reason string) bool {
	switch reason {
	case maint.RepackReasonProtected, maint.RepackReasonExtensionMissing,
		maint.RepackReasonClientMissing, maint.RepackReasonVersionMismatch,
		"is_follower", maint.RepackReasonClusterNotRunning,
		maint.RepackReasonActivationFence, "config_changed",
		maint.RepackReasonSpaceUnknown, maint.RepackReasonDefaultTablespace:
		return true
	}
	return false
}

// autoRepack is maint-weekly's opt-in repack. It runs one table at a time,
// biggest first, revalidating every table immediately before it starts. Window
// end and busy locks stop starting new tables; a running pg_repack is not
// killed. Per-database counts and reasons are returned for state and metrics.
func autoRepack(ctx context.Context, env *jobEnv, reports map[string]maint.BloatReport, now time.Time) map[string]maint.RepackRun {
	cfg := env.Cluster
	inWindow, left := windowLeft(cfg, now)
	deadline := now.Add(left)
	out := map[string]maint.RepackRun{}
	for _, spec := range cfg.Clusters {
		candidates := maint.RepackCandidates(reports[spec.Name], cfg.Maintenance.RepackBloatThreshold, maint.AutoRepackMinBytes)
		run := maint.RepackRun{DB: spec.Name, Tables: candidates, Failed: map[string]string{}, Unknown: map[string]string{}}
		finish := func() {
			run.Counts.At = stamp()
			out[spec.Name] = run
		}
		switch {
		case spec.IsFollower():
			for _, table := range candidates {
				autoRepackSkip(&run, table, "is_follower", spec.Name+" is a follower; its source's tables are maintained on the source")
			}
			finish()
			continue
		case !clusterRunning(spec):
			for _, table := range candidates {
				autoRepackSkip(&run, table, maint.RepackReasonClusterNotRunning, "the cluster is not running")
			}
			finish()
			continue
		case spec.Protected:
			for _, table := range candidates {
				autoRepackSkip(&run, table, maint.RepackReasonProtected, "repack is not supported on a protected database")
			}
			finish()
			continue
		case !inWindow:
			for _, table := range candidates {
				autoRepackSkip(&run, table, maint.RepackReasonOutsideWindow, "it is outside the maintenance window")
			}
			finish()
			continue
		}
		for i, table := range candidates {
			if autoRepackNow().After(deadline) {
				autoRepackSkipRemaining(&run, candidates, i, maint.RepackReasonWindowEnded, "the maintenance window ended before this table started")
				break
			}
			release, busy, err := autoRepackLocks(ctx, env, spec)
			if busy != "" {
				for _, rest := range candidates[i:] {
					autoRepackBusy(&run, rest, busy)
				}
				break
			}
			if err != nil {
				run.Failed[table] = err.Error()
				run.Counts.AddFailed()
				logLine(env.Report, env.Job, "auto-repack of "+table+" could not take its locks: "+err.Error())
				continue
			}
			freshCfg, freshSpec, err := reloadRepackTarget(spec)
			if err != nil {
				release()
				reason := repackReason(err)
				autoRepackSkipRemaining(&run, candidates, i, reason, err.Error())
				break
			}
			env.Cluster = freshCfg
			size, freshTarget, problem, err := revalidateRepackTable(ctx, freshCfg, spec, freshSpec, repackTarget{Name: table})
			if err != nil {
				release()
				run.Failed[table] = err.Error()
				run.Counts.AddFailed()
				logLine(env.Report, env.Job, "auto-repack of "+table+" failed validation: "+err.Error())
				continue
			}
			if problem != nil {
				release()
				autoRepackSkip(&run, table, problem.Reason, problem.Message)
				if repackDBWideReason(problem.Reason) {
					autoRepackSkipRemaining(&run, candidates, i+1, problem.Reason, problem.Message)
					break
				}
				continue
			}
			if _, err := checkRepackSpace(freshCfg, freshSpec, freshTarget.Name, size); err != nil {
				release()
				reason := repackReason(err)
				autoRepackSkip(&run, table, reason, err.Error())
				if repackDBWideReason(reason) {
					autoRepackSkipRemaining(&run, candidates, i+1, reason, err.Error())
					break
				}
				continue
			}
			before, err := repackStorage(ctx, freshSpec, freshTarget)
			if err != nil {
				release()
				run.Unknown[table] = "completion_unknown/not_started: storage identity unavailable before execution: " + err.Error()
				run.Counts.AddUnknown()
				logLine(env.Report, env.Job, "auto-repack of "+table+" was not started; rewrite completion is unknown: "+err.Error())
				continue
			}
			if err := verifyRepackTargetIdentity(ctx, freshSpec, freshTarget); err != nil {
				release()
				reason := repackReason(err)
				autoRepackSkip(&run, table, reason, err.Error())
				if repackDBWideReason(reason) {
					autoRepackSkipRemaining(&run, candidates, i+1, reason, err.Error())
					break
				}
				continue
			}
			if autoRepackNow().After(deadline) {
				release()
				autoRepackSkipRemaining(&run, candidates, i, maint.RepackReasonWindowEnded, "the maintenance window ended while this table was being prepared")
				break
			}
			execRes := maintRepackExec(ctx, env, freshSpec, freshTarget.Name)
			after, afterErr := repackStorage(ctx, freshSpec, freshTarget)
			outcome, reason, detail := classifyRepackCompletion(before, execRes, after, afterErr)
			release()
			switch outcome {
			case maint.RepackOutcomeRepacked:
				run.Repacked = append(run.Repacked, table)
				run.Counts.AddRepacked()
				logLine(env.Report, env.Job, "auto-repacked "+table+" in "+freshSpec.Name)
			case maint.RepackOutcomeSkipped:
				autoRepackSkip(&run, table, reason, detail)
				logLine(env.Report, env.Job, "auto-repack skipped "+table+": "+reason)
			case maint.RepackOutcomeUnknown:
				run.Unknown[table] = detail
				run.Counts.AddUnknown()
				logLine(env.Report, env.Job, "auto-repack for "+table+": rewrite completion could not be confirmed: "+detail)
			default:
				run.Failed[table] = detail
				run.Counts.AddFailed()
				logLine(env.Report, env.Job, "auto-repack did not complete "+table+": "+detail)
			}
		}
		finish()
	}
	return out
}
