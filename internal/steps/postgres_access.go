package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
)

const postgresAccessID = "postgres-access-v1"

// Change this revision when the policy or eligibility SQL changes.
const accessPolicyRevision = "owner-global-v1"

// PostgresAccessStep returns the unwired per-cluster access policy template.
// Callers must supply exact access authorization through engine.Options.
func PostgresAccessStep() engine.Step { return postgresAccess{} }

type postgresAccess struct {
	clusterStep
	inspect   func(context.Context, config.ClusterSpec) (AccessCoverage, error)
	reconcile func(context.Context, config.ClusterSpec) error
}

func (s postgresAccess) ID() string { return s.id(postgresAccessID) }
func (s postgresAccess) Instance(cluster string) engine.Step {
	s.cluster = cluster
	return s
}
func (s postgresAccess) AccessTarget() string { return s.cluster }
func (s postgresAccess) Describe(config.Cluster) string {
	return "install automatic owner-created schema access for " + s.cluster
}
func (s postgresAccess) SkipReason(cfg config.Cluster) string {
	if spec, err := s.spec(cfg); err == nil && spec.IsFollower() {
		return "follower"
	}
	return ""
}
func (s postgresAccess) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil, err
	}
	return canonical(struct {
		Revision string `json:"revision"`
		Database string `json:"database"`
		Owner    string `json:"owner"`
		App      string `json:"app"`
		RO       string `json:"ro"`
	}{accessPolicyRevision, spec.Name, spec.Name + "_owner", spec.Name + "_app", spec.Name + "_ro"})
}
func (s postgresAccess) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	inspect := s.inspect
	if inspect == nil {
		inspect = inspectAccessLocal
	}
	coverage, err := inspect(ctx, spec)
	if err != nil {
		return engine.CheckResult{}, fmt.Errorf("access coverage unknown: %w", err)
	}
	switch coverage.Status {
	case "installed":
		return engine.CheckResult{Complete: true, Detail: "automatic owner access installed"}, nil
	case "pending":
		return engine.CheckResult{Detail: fmt.Sprintf("automatic owner access expands eligible existing schema and object grants and global owner defaults (defaults %d, schemas %d, tables %d, sequences %d, routines %d missing); coordinate migration DDL quiescence; future defaults have no private-schema exclusions",
			coverage.DefaultMissing, coverage.SchemaMissing, coverage.TableMissing, coverage.SequenceMissing, coverage.RoutineMissing)}, nil
	default:
		return engine.CheckResult{}, errors.New("access coverage unknown")
	}
}
func (s postgresAccess) Apply(ctx context.Context, cfg config.Cluster, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if spec.IsFollower() {
		return engine.ApplyResult{}, errors.New("cannot reconcile access on a follower")
	}
	reconcile := s.reconcile
	if reconcile == nil {
		reconcile = reconcileAccessLocal
	}
	if err := reconcile(ctx, spec); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Restart: engine.RestartNone}, nil
}

func inspectAccessLocal(ctx context.Context, spec config.ClusterSpec) (AccessCoverage, error) {
	local := ClusterPG(spec)
	return InspectAccess(ctx, spec.Name, func(ctx context.Context, sql string) (string, error) {
		rows, err := local.Query(ctx, spec.Name, sql, nil)
		if err != nil {
			return "", err
		}
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0]["coverage"] == "" {
			return "", errors.New("access inspection did not return one coverage row")
		}
		return rows[0]["coverage"], nil
	})
}

func reconcileAccessLocal(ctx context.Context, spec config.ClusterSpec) error {
	local := ClusterPG(spec)
	return ReconcileAccess(ctx, spec.Name, func(ctx context.Context, sql string) error {
		return local.Exec(ctx, spec.Name, sql, nil)
	})
}

// AccessDefaultPrivilegesSQL returns the global, owner-scoped future-object
// grants. It does not reconcile existing objects or authorize an upgrade.
// Callers must install it in the same transaction as reconciliation, during
// an externally coordinated migration-quiescent window.
func AccessDefaultPrivilegesSQL(database string) string {
	owner := pg.Quote(database + "_owner")
	app := pg.Quote(database + "_app")
	ro := pg.Quote(database + "_ro")
	var b strings.Builder
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT USAGE ON SCHEMAS TO %s, %s;\n", owner, app, ro)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s;\n", owner, app)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT USAGE, SELECT ON SEQUENCES TO %s;\n", owner, app)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT EXECUTE ON FUNCTIONS TO %s;\n", owner, app)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT SELECT ON TABLES TO %s;\n", owner, ro)
	return b.String()
}

// accessCatalog is shared by inspection and reconciliation so their eligibility
// rules cannot diverge. A schema's owner must be the managed owner, not merely
// a role that owner belongs to (notably pg_database_owner).
const accessCatalog = `WITH application_schemas AS (
 SELECT n.oid, n.nspname, n.nspowner
 FROM pg_namespace n
 WHERE n.nspname <> 'information_schema' AND n.nspname <> 'vaire'
   AND n.nspname NOT LIKE 'pg\_%'
   AND NOT EXISTS (SELECT 1 FROM pg_depend d
     WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.objsubid = 0
       AND d.refclassid = 'pg_extension'::regclass AND d.refobjsubid = 0 AND d.deptype = 'e')
), eligible_schemas AS (
 SELECT * FROM application_schemas WHERE nspowner = {{OWNER}}::regrole
), application_relations AS (
 SELECT c.oid, c.relname, c.relkind, c.relnamespace, c.relowner
 FROM pg_class c JOIN application_schemas s ON s.oid = c.relnamespace
 WHERE c.relkind IN ('r','p','v','m','f','S')
   AND NOT EXISTS (SELECT 1 FROM pg_depend d
     WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.objsubid = 0
       AND d.refclassid = 'pg_extension'::regclass AND d.refobjsubid = 0 AND d.deptype = 'e')
), eligible_relations AS (
 SELECT c.* FROM application_relations c JOIN eligible_schemas s ON s.oid = c.relnamespace
 WHERE c.relowner = {{OWNER}}::regrole
), application_routines AS (
 SELECT p.oid, p.proname, p.pronamespace, p.proowner
 FROM pg_proc p JOIN application_schemas s ON s.oid = p.pronamespace
 WHERE p.prokind IN ('f','p')
   AND NOT EXISTS (SELECT 1 FROM pg_depend d
     WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.objsubid = 0
       AND d.refclassid = 'pg_extension'::regclass AND d.refobjsubid = 0 AND d.deptype = 'e')
), eligible_routines AS (
 SELECT p.* FROM application_routines p JOIN eligible_schemas s ON s.oid = p.pronamespace
 WHERE p.proowner = {{OWNER}}::regrole
), application_types AS (
 SELECT t.typowner
 FROM pg_type t JOIN application_schemas s ON s.oid = t.typnamespace
 WHERE (t.typtype IN ('d','e','r','m')
   OR (t.typtype = 'c' AND EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = t.typrelid AND c.relkind = 'c')))
   AND NOT EXISTS (SELECT 1 FROM pg_depend d
     WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.objsubid = 0
       AND d.refclassid = 'pg_extension'::regclass AND d.refobjsubid = 0 AND d.deptype = 'e')
)`

func accessSQL(template, database string) string {
	return strings.NewReplacer(
		"{{OWNER}}", accessLiteral(pg.Quote(database+"_owner")),
		"{{APP}}", accessLiteral(database+"_app"),
		"{{RO}}", accessLiteral(database+"_ro"),
		"{{APP_ROLE}}", accessLiteral(pg.Quote(database+"_app")),
		"{{RO_ROLE}}", accessLiteral(pg.Quote(database+"_ro")),
	).Replace(template)
}

func accessLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return "E" + literal(value)
}

// AccessReconcileSQL is a single transaction. The executor must stop on the
// first SQL error and close/rollback the session on failure. The caller must
// separately authorize this widening and coordinate migration DDL quiescence.
// Non-owner schemas, including public when owned by pg_database_owner, are
// outside this existing-object sweep. Future global defaults have no exclusions.
func AccessReconcileSQL(database string) string {
	sweep := `DECLARE r record;
BEGIN
 FOR r IN ` + accessCatalog + `
 SELECT 'schema' AS kind, s.nspname AS schema_name, NULL::name AS object_name, NULL::text AS args FROM eligible_schemas s
 UNION ALL
 SELECT CASE WHEN c.relkind = 'S' THEN 'sequence' ELSE 'table' END,
        s.nspname, c.relname, NULL::text
 FROM eligible_relations c JOIN eligible_schemas s ON s.oid = c.relnamespace
 UNION ALL
 SELECT 'routine', s.nspname, p.proname, pg_get_function_identity_arguments(p.oid)
 FROM eligible_routines p JOIN eligible_schemas s ON s.oid = p.pronamespace
 LOOP
   CASE r.kind
   WHEN 'schema' THEN
     EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I, %I', r.schema_name, {{APP}}, {{RO}});
   WHEN 'table' THEN
     EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE %I.%I TO %I', r.schema_name, r.object_name, {{APP}});
     EXECUTE format('GRANT SELECT ON TABLE %I.%I TO %I', r.schema_name, r.object_name, {{RO}});
   WHEN 'sequence' THEN
     EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE %I.%I TO %I', r.schema_name, r.object_name, {{APP}});
   WHEN 'routine' THEN
     EXECUTE format('GRANT EXECUTE ON ROUTINE %I.%I(%s) TO %I', r.schema_name, r.object_name, r.args, {{APP}});
   END CASE;
 END LOOP;
END;
`
	sweep = accessSQL(sweep, database)
	tag := "$vaire$"
	for strings.Contains(sweep, tag) {
		tag = strings.TrimSuffix(tag, "$") + "_$"
	}
	return "BEGIN;\nSET LOCAL search_path = pg_catalog;\n" + AccessDefaultPrivilegesSQL(database) +
		"DO " + tag + sweep + tag + ";\nCOMMIT;\n"
}

// AccessExec executes a complete script in one session, stops on the first
// error, and closes or rolls back that session on error.
type AccessExec func(context.Context, string) error

// ReconcileAccess does not authorize the transition. An error is returned
// unchanged; callers must not journal success on failure.
func ReconcileAccess(ctx context.Context, database string, exec AccessExec) error {
	if exec == nil {
		return errors.New("access reconciliation is unavailable")
	}
	return exec(ctx, AccessReconcileSQL(database))
}

// AccessCoverage reports observed coverage, not a journal state. Uncovered
// creators are informational: they do not make owner policy incomplete.
type AccessCoverage struct {
	Status            string   `json:"status"`
	DefaultMissing    int      `json:"default_missing"`
	SchemaMissing     int      `json:"schema_missing"`
	TableMissing      int      `json:"table_missing"`
	SequenceMissing   int      `json:"sequence_missing"`
	RoutineMissing    int      `json:"routine_missing"`
	UncoveredCreators []string `json:"uncovered_creators"`
}

// AccessInspectionSQL checks global namespace-zero defaults directly. Object
// checks use has_*_privilege, which evaluates each object's ACL (including
// PostgreSQL's built-in defaults for NULL ACLs), never pg_default_acl.
func AccessInspectionSQL(database string) string {
	sql := accessCatalog + `,
 required_defaults(kind, grantee, privilege) AS (VALUES
   ('n', {{APP_ROLE}}::regrole, 'USAGE'), ('n', {{RO_ROLE}}::regrole, 'USAGE'),
   ('r', {{APP_ROLE}}::regrole, 'SELECT'), ('r', {{APP_ROLE}}::regrole, 'INSERT'),
   ('r', {{APP_ROLE}}::regrole, 'UPDATE'), ('r', {{APP_ROLE}}::regrole, 'DELETE'), ('r', {{RO_ROLE}}::regrole, 'SELECT'),
   ('S', {{APP_ROLE}}::regrole, 'USAGE'), ('S', {{APP_ROLE}}::regrole, 'SELECT'),
   ('f', {{APP_ROLE}}::regrole, 'EXECUTE')
), missing_defaults AS (
 SELECT r.* FROM required_defaults r
 WHERE NOT EXISTS (
   SELECT 1 FROM pg_default_acl d
   CROSS JOIN LATERAL aclexplode(d.defaclacl) a
   WHERE d.defaclrole = {{OWNER}}::regrole AND d.defaclnamespace = 0
     AND d.defaclobjtype = r.kind::"char" AND a.grantee = r.grantee
     AND a.privilege_type = r.privilege
 )
), uncovered AS (
 SELECT nspowner AS role FROM application_schemas WHERE nspowner <> {{OWNER}}::regrole
 UNION SELECT relowner FROM application_relations WHERE relowner <> {{OWNER}}::regrole
 UNION SELECT proowner FROM application_routines WHERE proowner <> {{OWNER}}::regrole
 UNION SELECT typowner FROM application_types WHERE typowner <> {{OWNER}}::regrole
)
SELECT json_build_object(
 'identities', json_build_array({{OWNER}}::regrole::oid, {{APP_ROLE}}::regrole::oid, {{RO_ROLE}}::regrole::oid),
 'default_missing', (SELECT count(*) FROM missing_defaults),
 'schema_missing', (SELECT count(*) FROM eligible_schemas s
   WHERE NOT has_schema_privilege({{APP}}, s.oid, 'USAGE')
      OR NOT has_schema_privilege({{RO}}, s.oid, 'USAGE')),
 'table_missing', (SELECT count(*) FROM eligible_relations c WHERE c.relkind <> 'S'
   AND (NOT has_table_privilege({{APP}}, c.oid, 'SELECT')
     OR NOT has_table_privilege({{APP}}, c.oid, 'INSERT')
     OR NOT has_table_privilege({{APP}}, c.oid, 'UPDATE')
     OR NOT has_table_privilege({{APP}}, c.oid, 'DELETE')
     OR NOT has_table_privilege({{RO}}, c.oid, 'SELECT'))),
 'sequence_missing', (SELECT count(*) FROM eligible_relations c WHERE c.relkind = 'S'
   AND (NOT has_sequence_privilege({{APP}}, c.oid, 'USAGE')
     OR NOT has_sequence_privilege({{APP}}, c.oid, 'SELECT'))),
 'routine_missing', (SELECT count(*) FROM eligible_routines p
   WHERE NOT has_function_privilege({{APP}}, p.oid, 'EXECUTE')),
 'uncovered_creators', COALESCE((SELECT json_agg(pg_get_userbyid(role) ORDER BY pg_get_userbyid(role)) FROM uncovered), '[]'::json)
) AS coverage;
`
	return accessSQL(sql, database)
}

// AccessQuery reads exactly one JSON coverage value from one database. The
// adapter must return an error for failed, missing, or multiple result rows.
type AccessQuery func(context.Context, string) (string, error)

// InspectAccess never treats failed or malformed inspection as installed.
func InspectAccess(ctx context.Context, database string, query AccessQuery) (AccessCoverage, error) {
	unknown := AccessCoverage{Status: "unknown"}
	if query == nil {
		return unknown, errors.New("access inspection is unavailable")
	}
	raw, err := query(ctx, AccessInspectionSQL(database))
	if err != nil {
		return unknown, err
	}
	var result struct {
		Identities        []uint32 `json:"identities"`
		DefaultMissing    *int     `json:"default_missing"`
		SchemaMissing     *int     `json:"schema_missing"`
		TableMissing      *int     `json:"table_missing"`
		SequenceMissing   *int     `json:"sequence_missing"`
		RoutineMissing    *int     `json:"routine_missing"`
		UncoveredCreators []string `json:"uncovered_creators"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return unknown, fmt.Errorf("invalid access inspection: %w", err)
	}
	if len(result.Identities) != 3 || result.Identities[0] == 0 || result.Identities[1] == 0 || result.Identities[2] == 0 {
		return unknown, errors.New("incomplete access role inspection")
	}
	counts := []*int{result.DefaultMissing, result.SchemaMissing, result.TableMissing, result.SequenceMissing, result.RoutineMissing}
	for _, count := range counts {
		if count == nil || *count < 0 {
			return unknown, errors.New("incomplete access inspection")
		}
	}
	if result.UncoveredCreators == nil {
		return unknown, errors.New("incomplete access creator inspection")
	}
	coverage := AccessCoverage{
		Status: "installed", DefaultMissing: *result.DefaultMissing, SchemaMissing: *result.SchemaMissing,
		TableMissing: *result.TableMissing, SequenceMissing: *result.SequenceMissing,
		RoutineMissing: *result.RoutineMissing, UncoveredCreators: result.UncoveredCreators,
	}
	for _, count := range counts {
		if *count != 0 {
			coverage.Status = "pending"
		}
	}
	return coverage, nil
}
