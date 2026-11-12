package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/secrets"
)

const postgresRolesID = "postgres-roles-v1"

// AuthDB is the database of each cluster that holds the pooler auth
// functions: the <db>__auth alias in both poolers' [databases] opens it, so
// auth_query runs there (render.AuthAlias). Roles are cluster-wide, so a
// write role made from it is visible there too.
const AuthDB = "postgres"

// systemDatabases are never reported as orphaned.
var systemDatabases = map[string]bool{"postgres": true, "template0": true, "template1": true}

// postgresRoles creates, in one cluster, the service roles, vaire_direct_only,
// the vaire schema with both pooler auth functions, and that cluster's
// database with its four roles, grants and extensions (RFC 12).
type postgresRoles struct{ clusterStep }

func (s postgresRoles) ID() string { return s.id(postgresRolesID) }

func (postgresRoles) Instance(cluster string) engine.Step {
	return postgresRoles{clusterStep{cluster: cluster}}
}

// SkipReason skips a follower (Phase 6): its roles, database and auth
// functions are its source's, replicated, and it takes no writes.
func (s postgresRoles) SkipReason(cfg config.Cluster) string {
	if spec, err := s.spec(cfg); err == nil && spec.IsFollower() {
		return "follower"
	}
	return ""
}

func (s postgresRoles) Describe(config.Cluster) string {
	return "create the service roles, the auth functions and " + s.cluster + "'s roles, database, grants and extensions"
}

// roleInputs is everything Apply depends on except passwords (D5).
type roleInputs struct {
	Admin   bool   `json:"admin"`
	Monitor bool   `json:"monitor"`
	Auth    bool   `json:"auth"`
	AuthSQL string `json:"auth_sql"`
	// AccessSQL makes changes to the admin access policy reapply on upgrade,
	// even when the database's desired configuration has not changed.
	AccessSQL string `json:"access_sql"`
	// MonitorSQL joins the inputs so every cluster re-runs it once when it
	// changes (Phase 4).
	MonitorSQL string         `json:"monitor_sql"`
	DB         databaseInputs `json:"database"`
}

type databaseInputs struct {
	Name             string   `json:"name"`
	StatementTimeout string   `json:"statement_timeout"`
	Extensions       []string `json:"extensions"`
	// Protected takes vaire_admin's write access away (AdminAccessSQL), so
	// protecting or lifting a protection re-runs the step.
	Protected bool `json:"protected,omitempty"`
}

func (s postgresRoles) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil, err
	}
	return canonical(roleInputs{
		Admin: true, Monitor: true, Auth: true, AuthSQL: pgbouncerAuthSQL, AccessSQL: AdminRoleSQL(spec), MonitorSQL: monitorSQL,
		DB: databaseInputs{Name: spec.Name, StatementTimeout: spec.StatementTimeout, Extensions: spec.Extensions, Protected: spec.Protected},
	})
}

func (s postgresRoles) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	local := ClusterPG(spec)
	roles, err := queryColumn(ctx, local, "postgres", "SELECT rolname FROM pg_roles")
	if err != nil {
		return incomplete("cannot read pg_roles"), nil
	}
	roleSet := stringSet(roles)
	for _, role := range append([]string{"vaire_admin", "vaire_monitor", "pgbouncer_auth", RoleDirectOnly, spec.Name + "_owner"}, prefixed(spec.Name, DatabaseRoles)...) {
		if !roleSet[role] {
			return incomplete("role " + role + " is missing"), nil
		}
	}
	dbs, err := queryColumn(ctx, local, "postgres", "SELECT datname FROM pg_database")
	if err != nil {
		return incomplete("cannot read pg_database"), nil
	}
	if !stringSet(dbs)[spec.Name] {
		return incomplete("database " + spec.Name + " is missing"), nil
	}
	for _, fn := range []struct{ name, needs string }{
		{"pgbouncer_get_auth", RoleDirectOnly},
		{"pgbouncer_get_auth_session", "operator connections"},
	} {
		src, err := queryColumn(ctx, local, AuthDB, "SELECT prosrc FROM pg_proc WHERE proname = '"+fn.name+"'")
		if err != nil || len(src) == 0 {
			return incomplete("vaire." + fn.name + " is missing"), nil
		}
		if !strings.Contains(src[0], fn.needs) {
			return incomplete("vaire." + fn.name + " is out of date"), nil
		}
	}
	member, err := queryColumn(ctx, local, "postgres", adminOwnerMemberSQL(spec.Name))
	if err != nil || len(member) != 1 {
		return incomplete("cannot read vaire_admin's membership in " + spec.Name + "_owner"), nil
	}
	if (member[0] == "t") == spec.Protected {
		if spec.Protected {
			return incomplete("vaire_admin can still write to protected " + spec.Name), nil
		}
		return incomplete("vaire_admin is not a member of " + spec.Name + "_owner"), nil
	}
	for _, r := range []struct{ role, protectedGap string }{
		{"pg_signal_backend", "vaire_admin can still terminate sessions of protected " + spec.Name},
		{"pg_maintain", "vaire_admin can still lock the tables of protected " + spec.Name},
	} {
		held, err := queryColumn(ctx, local, "postgres", adminMemberSQL(r.role))
		if err != nil || len(held) != 1 {
			return incomplete("cannot read vaire_admin's membership in " + r.role), nil
		}
		if (held[0] == "t") == spec.Protected {
			if spec.Protected {
				return incomplete(r.protectedGap), nil
			}
			return incomplete("vaire_admin is not a member of " + r.role), nil
		}
	}
	direct, err := queryColumn(ctx, local, "postgres", directOnlyMembersSQL)
	if err != nil {
		return incomplete("cannot read " + RoleDirectOnly + " members"), nil
	}
	if !stringSet(direct)[spec.Name+"_"+RoleMigrate] {
		return incomplete("role " + spec.Name + "_" + RoleMigrate + " is not in " + RoleDirectOnly), nil
	}
	installed, err := queryColumn(ctx, local, spec.Name, "SELECT extname FROM pg_extension")
	if err != nil {
		return incomplete("cannot read extensions in " + spec.Name), nil
	}
	have := stringSet(installed)
	for _, ext := range spec.Extensions {
		entry, ok := catalog.Lookup(ext)
		if !ok || entry.ExtName == "" {
			continue
		}
		if !have[entry.ExtName] {
			return incomplete("extension " + entry.ExtName + " is missing in " + spec.Name), nil
		}
	}
	for _, name := range []string{"pgbouncer_auth", "monitor"} {
		if _, err := os.Stat(RuntimeDir + "/" + name + ".pass"); err != nil {
			return incomplete(RuntimeDir + "/" + name + ".pass is missing"), nil
		}
	}
	return complete(), nil
}

func (s postgresRoles) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	local := ClusterPG(spec)
	roles, err := queryColumn(ctx, local, "postgres", "SELECT rolname FROM pg_roles")
	if err != nil {
		return engine.ApplyResult{}, err
	}
	roleSet := stringSet(roles)
	dbs, err := queryColumn(ctx, local, "postgres", "SELECT datname FROM pg_database")
	if err != nil {
		return engine.ApplyResult{}, err
	}
	dbSet := stringSet(dbs)

	var cluster strings.Builder
	cluster.WriteString("-- managed by vaire - do not edit; use cluster.toml\n")
	service := []struct {
		role, secret, attrs, grants string
	}{
		{"vaire_admin", SecretAdminPassword, "LOGIN CREATEDB CREATEROLE", adminGrants(spec)},
		{"vaire_monitor", SecretMonitor, "LOGIN", "pg_monitor"},
		{"pgbouncer_auth", SecretPgbouncerAuth, "LOGIN", ""},
	}
	for _, r := range service {
		if !roleSet[r.role] {
			v, err := serviceVerifier(ctx, cfg, spec, rt, r.role, r.secret)
			if err != nil {
				return engine.ApplyResult{}, err
			}
			fmt.Fprintf(&cluster, "CREATE ROLE %s %s PASSWORD '%s';\n", pg.Quote(r.role), r.attrs, v)
		}
		if r.grants != "" {
			fmt.Fprintf(&cluster, "GRANT %s TO %s;\n", r.grants, pg.Quote(r.role))
		}
	}
	if !roleSet[RoleDirectOnly] {
		fmt.Fprintf(&cluster, "CREATE ROLE %s NOLOGIN;\n", RoleDirectOnly)
	}
	cluster.WriteString(monitorSQL)
	cluster.WriteString(pgbouncerAuthSQL)

	sql, err := databaseSQL(ctx, rt, spec, roleSet, dbSet)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	cluster.WriteString(sql)
	if err := local.Exec(ctx, AuthDB, cluster.String(), nil); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := local.Exec(ctx, spec.Name, databaseGrantsSQL(spec), nil); err != nil {
		return engine.ApplyResult{}, err
	}

	if err := writeRuntimePass(rt, "pgbouncer_auth", SecretPgbouncerAuth); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeRuntimePass(rt, "monitor", SecretMonitor); err != nil {
		return engine.ApplyResult{}, err
	}

	return engine.ApplyResult{Restart: engine.RestartNone, Warnings: orphanedWarnings(spec, dbs, roleSet)}, nil
}

// monitorSQL sets vaire_monitor up for the exporters (Phase 4 D4): a
// postgres_exporter probe needs a connection slot exactly when the pools
// are full, so the role shares the three reserved slots with vaire_admin.
// CONNECTION LIMIT 2 is one probe: postgres_exporter 0.20.1's /probe opens
// two connections at once (the legacy exporter's, for pg_up and the default
// metrics, and the probe collectors'), so a limit of 1 would fail one of
// them on every scrape; it leaves at least one reserved slot to vaire_admin.
// The timeouts keep a probe from holding a backend or waiting on a lock.
const monitorSQL = `GRANT pg_use_reserved_connections TO vaire_monitor;
ALTER ROLE vaire_monitor CONNECTION LIMIT 2;
ALTER ROLE vaire_monitor SET statement_timeout = '10s';
ALTER ROLE vaire_monitor SET lock_timeout = '1s';
`

// serviceVerifier is the SCRAM verifier for a service role in spec's
// cluster. Service passwords are setup-wide (RFC 12): another cluster's
// verifier is copied, so every cluster holds the same one, and only the
// first cluster computes it from the request's secret.
func serviceVerifier(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime, role, secretKey string) (string, error) {
	for _, other := range cfg.Clusters {
		if other.Name == spec.Name {
			continue
		}
		rows, err := queryColumn(ctx, ClusterPG(other), "postgres", "SELECT rolpassword FROM pg_authid WHERE rolname = '"+role+"'")
		if err == nil && len(rows) == 1 && strings.HasPrefix(rows[0], "SCRAM-SHA-256$") {
			secrets.Add(rows[0])
			return rows[0], nil
		}
	}
	return roleVerifier(rt, secretKey, role, "")
}

// databaseSQL creates the roles and the database, cluster-wide, in postgres.
func databaseSQL(ctx context.Context, rt engine.Runtime, db config.ClusterSpec, roleSet, dbSet map[string]bool) (string, error) {
	var b strings.Builder
	owner := db.Name + "_owner"
	if !roleSet[owner] {
		fmt.Fprintf(&b, "CREATE ROLE %s NOLOGIN;\n", pg.Quote(owner))
	}
	for _, role := range DatabaseRoles {
		name := db.Name + "_" + role
		if !roleSet[name] {
			v, err := roleVerifier(rt, SecretDatabaseRole(db.Name, role), name, db.Name)
			if err != nil {
				return "", err
			}
			switch role {
			case RoleMigrate:
				fmt.Fprintf(&b, "CREATE ROLE %s LOGIN PASSWORD '%s' IN ROLE %s;\n", pg.Quote(name), v, pg.Quote(owner))
			default:
				fmt.Fprintf(&b, "CREATE ROLE %s LOGIN PASSWORD '%s';\n", pg.Quote(name), v)
			}
		}
	}
	fmt.Fprintf(&b, "ALTER ROLE %s SET role = '%s';\n", pg.Quote(db.Name+"_migrate"), owner)
	fmt.Fprintf(&b, "GRANT %s TO %s;\n", RoleDirectOnly, pg.Quote(db.Name+"_migrate"))
	fmt.Fprintf(&b, "ALTER ROLE %s SET statement_timeout = '%s';\n", pg.Quote(db.Name+"_app"), db.StatementTimeout)
	fmt.Fprintf(&b, "ALTER ROLE %s SET default_transaction_read_only = on;\n", pg.Quote(db.Name+"_ro"))
	if !dbSet[db.Name] {
		fmt.Fprintf(&b, "CREATE DATABASE %s OWNER %s;\n", pg.Quote(db.Name), pg.Quote(owner))
	}
	b.WriteString(DatabaseConnectSQL(db.Name, db.Name))
	b.WriteString(AdminAccessSQL(db))
	return b.String(), nil
}

// adminOwnerMemberSQL reports, as t or f, whether vaire_admin is a member of
// db's owner role, directly or not.
func adminOwnerMemberSQL(db string) string {
	return "SELECT pg_has_role('vaire_admin', " + literal(db+"_owner") + ", 'MEMBER')"
}

// adminMemberSQL reports, as t or f, whether vaire_admin is a member of the
// predefined role role.
func adminMemberSQL(role string) string {
	return "SELECT pg_has_role('vaire_admin', " + literal(role) + ", 'MEMBER')"
}

// adminGrants are the predefined roles vaire_admin holds in spec's cluster.
// On a protected database it holds neither pg_signal_backend nor
// pg_maintain. pg_signal_backend would let it end the database's sessions
// from a read-only vaire sql, and vaire maint kill there needs a code from the
// authenticator app (node.handleMaintKill terminates as the superuser with
// SET ROLE pg_signal_backend instead). pg_maintain would let it LOCK TABLE
// the application's tables (and CLUSTER, VACUUM FULL or REFRESH
// MATERIALIZED VIEW them) from the same session: a read-only option it can
// turn off stops none of these. vaire maint there runs as the superuser with
// SET ROLE pg_maintain (node.maintPG), so vacuum, analyze and reindex still
// need no code.
func adminGrants(spec config.ClusterSpec) string {
	if spec.Protected {
		return "pg_monitor, pg_use_reserved_connections"
	}
	return "pg_monitor, pg_signal_backend, pg_maintain, pg_use_reserved_connections"
}

// AdminRoleSQL is vaire_admin's predefined roles and access to db
// (adminGrants, AdminAccessSQL), run in postgres. A restore from a dump
// runs it in its new cluster after copying the live cluster's roles, so the
// copy's vaire_admin follows db's protection as it is now.
func AdminRoleSQL(db config.ClusterSpec) string {
	return "GRANT " + adminGrants(db) + " TO vaire_admin;\n" + AdminAccessSQL(db)
}

// AdminAccessSQL is vaire_admin's access to db, run in postgres. On an
// unprotected database vaire_admin is a member of <db>_owner, so vaire sql,
// psql and console --write act as the owner. On a protected one it is not:
// every grant of the owner role to vaire_admin goes, with CASCADE, so the
// grants vaire_admin made from its ADMIN OPTION go too, and the owner role
// cannot log in. vaire_admin gets pg_read_all_data for reads and loses
// pg_signal_backend and pg_maintain (adminGrants). A write then needs a
// short-lived role the node creates after it checks a code from the
// authenticator app (db.write-access). vaire_admin's own sessions on the
// database end the moment its membership goes, since one may have SET ROLE
// to the owner already. The vaire_admin password alone, in a context or
// through vaire tunnel, can then only read a protected database.
func AdminAccessSQL(db config.ClusterSpec) string {
	return AdminAccessSQLFor(db.Name+"_owner", db.Name, db.Protected)
}

// AdminAccessSQLFor is AdminAccessSQL for a database and owner role whose
// names differ from one database's prefix: db unfollow settles a promoted
// follower while the source's database and roles still exist, and again after
// a partial rename, so the policy names what PostgreSQL actually holds.
func AdminAccessSQLFor(owner, database string, protected bool) string {
	if !protected {
		return fmt.Sprintf(`GRANT %s TO vaire_admin WITH ADMIN OPTION;
DO $vaire$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_auth_members WHERE roleid = 'pg_read_all_data'::regrole AND member = 'vaire_admin'::regrole) THEN
    REVOKE pg_read_all_data FROM vaire_admin;
  END IF;
END $vaire$;
`, pg.Quote(owner))
	}
	return fmt.Sprintf(`GRANT pg_read_all_data TO vaire_admin;
ALTER ROLE %[1]s NOLOGIN;
DO $vaire$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_auth_members WHERE roleid = 'pg_signal_backend'::regrole AND member = 'vaire_admin'::regrole) THEN
    REVOKE pg_signal_backend FROM vaire_admin;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_auth_members WHERE roleid = 'pg_maintain'::regrole AND member = 'vaire_admin'::regrole) THEN
    REVOKE pg_maintain FROM vaire_admin;
  END IF;
END $vaire$;
DO $vaire$
DECLARE
  g name;
  was boolean := pg_has_role('vaire_admin', %[2]s, 'MEMBER');
BEGIN
  FOR g IN SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.grantor
      WHERE m.roleid = %[2]s::regrole AND m.member = 'vaire_admin'::regrole LOOP
    BEGIN
      EXECUTE format('REVOKE %%I FROM vaire_admin GRANTED BY %%I CASCADE', %[2]s, g);
    EXCEPTION WHEN OTHERS THEN
      NULL; -- gone already with an earlier grant's CASCADE
    END;
  END LOOP;
  IF pg_has_role('vaire_admin', %[2]s, 'MEMBER') THEN
    RAISE EXCEPTION 'vaire_admin is still a member of %%', %[2]s;
  END IF;
  IF was THEN
    PERFORM pg_terminate_backend(pid) FROM pg_stat_activity
      WHERE usename = 'vaire_admin' AND datname = %[3]s AND backend_type = 'client backend' AND pid <> pg_backend_pid();
  END IF;
END $vaire$;
`, pg.Quote(owner), literal(owner), literal(database))
}

// literal quotes s as an SQL string literal.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// DatabaseConnectSQL is the database-level access SQL for database target,
// granted to the roles of roleDB, run in postgres. A restore from a dump
// uses it for the database it loads into a new cluster.
func DatabaseConnectSQL(target, roleDB string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REVOKE ALL ON DATABASE %s FROM PUBLIC;\n", pg.Quote(target))
	fmt.Fprintf(&b, "GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s, %s, %s, vaire_admin, vaire_monitor;\n",
		pg.Quote(target), pg.Quote(roleDB+"_migrate"), pg.Quote(roleDB+"_app"), pg.Quote(roleDB+"_ro"))
	return b.String()
}

// DatabaseGrantsSQL is postgres-roles-v1's schema, default-privilege and
// extension SQL for db, run inside the database it is for. A restore from a
// dump and vaire db fork run it in the databases they load.
func DatabaseGrantsSQL(db config.ClusterSpec) string { return databaseGrantsSQL(db) }

// databaseGrantsSQL is the schema and default-privilege SQL run inside db.
func databaseGrantsSQL(db config.ClusterSpec) string {
	owner := pg.Quote(db.Name + "_owner")
	app := pg.Quote(db.Name + "_app")
	ro := pg.Quote(db.Name + "_ro")
	var b strings.Builder
	fmt.Fprintf(&b, "GRANT USAGE ON SCHEMA public TO %s, %s;\n", app, ro)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s;\n", owner, app)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s;\n", owner, app)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO %s;\n", owner, app)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT ON TABLES TO %s;\n", owner, ro)
	for _, ext := range db.Extensions {
		entry, ok := catalog.Lookup(ext)
		if !ok || entry.ExtName == "" {
			continue
		}
		fmt.Fprintf(&b, "CREATE EXTENSION IF NOT EXISTS %s;\n", pg.Quote(entry.ExtName))
	}
	return b.String()
}

// ExtensionState is one pg_extension row (or one row a fresh database will
// install): the extension name, its installed version and its schema.
type ExtensionState struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
}

// defaultCopyExtension is the instrumentation every vaire database installs,
// which a copy destination must keep in addition to the source's extensions.
const defaultCopyExtension = "pg_stat_statements"

// pgExtensionControlDir is PostgreSQL 18's installed extension control
// directory. It is a variable so rootless tests supply their own files.
var pgExtensionControlDir = filepath.Join("/usr/share/postgresql", strconv.Itoa(PGVersion), "extension")

// CopyExtensionPlan resolves the source's extensions against vaire's catalog
// and the destination's installed PostgreSQL 18 extension metadata. It
// returns the pg_extension rows a fresh vaire database will have, and the
// config extension names in dependency order. A source extension that vaire
// does not support, whose control metadata is missing, or whose version or
// schema differs from what CREATE EXTENSION will install fails before any
// destination is provisioned.
func CopyExtensionPlan(source []ExtensionState) ([]ExtensionState, []string, error) {
	seen := map[string]bool{}
	for _, ext := range source {
		if err := validateExtensionState(ext); err != nil {
			return nil, nil, err
		}
		if seen[ext.Name] {
			return nil, nil, copyExtensionError("extension_incompatible",
				"the source reports extension "+safeExtensionName(ext.Name)+" more than once",
				"Do not copy from this source until its extension catalog is consistent.")
		}
		seen[ext.Name] = true
	}

	plan := map[string]ExtensionState{}
	configured := map[string]bool{defaultCopyExtension: true}
	for _, ext := range source {
		if ext.Name != "plpgsql" {
			entry, ok := catalog.Lookup(ext.Name)
			if !ok || entry.ExtName == "" {
				return nil, nil, copyExtensionError("extension_missing",
					"the source extension "+safeExtensionName(ext.Name)+" is not supported by vaire",
					"Remove the extension from the source, or install a compatible vaire support for it.")
			}
			configured[entry.ExtName] = true
		}
	}
	// Add dependencies named by control metadata, so CREATE EXTENSION can
	// create them in order. A dependency outside the catalog is unsupported.
	var addRequirements func(string, map[string]bool) error
	addRequirements = func(name string, visiting map[string]bool) error {
		if name == "plpgsql" {
			return nil
		}
		if visiting[name] {
			return copyExtensionError("extension_incompatible",
				"the destination extension metadata has a dependency cycle at "+safeExtensionName(name),
				"Report this as a vaire bug; do not retry the copy.")
		}
		control, err := readExtensionControl(name)
		if err != nil {
			return err
		}
		visiting[name] = true
		for _, req := range control.requires {
			if req == "plpgsql" {
				continue
			}
			entry, ok := catalog.Lookup(req)
			if !ok || entry.ExtName == "" {
				return copyExtensionError("extension_missing",
					"extension "+safeExtensionName(name)+" requires "+safeExtensionName(req)+", which vaire does not support",
					"Remove "+safeExtensionName(name)+" from the source, or install compatible vaire support for "+safeExtensionName(req)+".")
			}
			configured[entry.ExtName] = true
			if err := addRequirements(entry.ExtName, visiting); err != nil {
				return err
			}
		}
		delete(visiting, name)
		return nil
	}
	for name := range configured {
		if err := addRequirements(name, map[string]bool{}); err != nil {
			return nil, nil, err
		}
	}

	// Resolve every source extension's exact version and schema, and the
	// default instrumentation's, from PG18's installed control metadata.
	for _, ext := range source {
		control, err := readExtensionControl(ext.Name)
		if err != nil {
			return nil, nil, err
		}
		if ext.Version != control.version {
			return nil, nil, copyExtensionError("extension_version_incompatible",
				"extension "+safeExtensionName(ext.Name)+" is version "+safeExtensionName(ext.Version)+" on the source but version "+safeExtensionName(control.version)+" on the destination",
				"Align the extension versions before copying; vaire does not convert extension versions automatically.")
		}
		if ext.Schema != control.schema {
			return nil, nil, copyExtensionError("extension_schema_incompatible",
				"extension "+safeExtensionName(ext.Name)+" is in schema "+safeExtensionName(ext.Schema)+" on the source but in schema "+safeExtensionName(control.schema)+" on the destination",
				"Install the destination extension in the same schema; vaire does not rewrite extension schemas.")
		}
		plan[ext.Name] = ext
	}
	// plpgsql is installed in every database and is not in db.Extensions, but
	// a fresh destination still has this exact pg_extension row.
	if _, ok := plan["plpgsql"]; !ok {
		control, err := readExtensionControl("plpgsql")
		if err != nil {
			return nil, nil, err
		}
		plan["plpgsql"] = ExtensionState{Name: "plpgsql", Version: control.version, Schema: control.schema}
	}
	for name := range configured {
		if name == "plpgsql" {
			continue
		}
		if _, ok := plan[name]; ok {
			continue
		}
		control, err := readExtensionControl(name)
		if err != nil {
			return nil, nil, err
		}
		plan[name] = ExtensionState{Name: name, Version: control.version, Schema: control.schema}
	}

	ordered, err := extensionDependencyOrder(configured)
	if err != nil {
		return nil, nil, err
	}
	out := make([]ExtensionState, 0, len(plan))
	for _, ext := range plan {
		out = append(out, ext)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, ordered, nil
}

func extensionDependencyOrder(configured map[string]bool) ([]string, error) {
	var out []string
	state := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return copyExtensionError("extension_incompatible",
				"the destination extension metadata has a dependency cycle at "+safeExtensionName(name),
				"Report this as a vaire bug; do not retry the copy.")
		case 2:
			return nil
		}
		state[name] = 1
		control, err := readExtensionControl(name)
		if err != nil {
			return err
		}
		for _, req := range control.requires {
			if req == "plpgsql" || !configured[req] {
				continue
			}
			if err := visit(req); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, name)
		return nil
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// extensionControl is the subset of one PG18 .control file a copy needs.
type extensionControl struct {
	version  string
	schema   string
	requires []string
}

var extensionControlNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// readExtensionControl reads <name>.control from the destination's installed
// PG18 extension directory. Only a fixed key/value shape is accepted; a
// malformed or missing file fails closed.
func readExtensionControl(name string) (extensionControl, error) {
	if !extensionControlNamePattern.MatchString(name) {
		return extensionControl{}, copyExtensionError("extension_incompatible",
			"the extension name is not a safe control-file name",
			"Do not copy from this source until its extension catalog is consistent.")
	}
	path := filepath.Join(pgExtensionControlDir, name+".control")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: name is validated and the directory is fixed.
	if err != nil {
		return extensionControl{}, copyExtensionError("extension_missing",
			"extension "+safeExtensionName(name)+" is not installed on the destination",
			"Install the extension package on the node before copying, or remove it from the source.")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return extensionControl{}, copyExtensionError("extension_incompatible",
				"the destination extension metadata for "+safeExtensionName(name)+" is malformed",
				"Do not copy until the node's extension package is repaired.")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, "'\"")
		values[key] = value
	}
	control := extensionControl{version: values["default_version"], schema: values["schema"]}
	if control.version == "" {
		return extensionControl{}, copyExtensionError("extension_incompatible",
			"the destination extension metadata for "+safeExtensionName(name)+" has no default version",
			"Do not copy until the node's extension package is repaired.")
	}
	if control.schema == "" {
		control.schema = "public"
	}
	for _, req := range strings.FieldsFunc(values["requires"], func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		if req != "" {
			control.requires = append(control.requires, req)
		}
	}
	available, err := extensionInstallPath(name, control.version)
	if err != nil {
		return extensionControl{}, err
	}
	if !available {
		return extensionControl{}, copyExtensionError("extension_version_incompatible",
			"the destination has no installation path for extension "+safeExtensionName(name)+" version "+safeExtensionName(control.version),
			"Align the extension versions before copying; vaire does not convert extension versions automatically.")
	}
	return control, nil
}

// extensionInstallPath reports whether PostgreSQL 18 can install version from
// a direct base script or from a base script plus update scripts. It does not
// assume a direct <extension>--<version>.sql file exists.
func extensionInstallPath(name, target string) (bool, error) {
	entries, err := os.ReadDir(pgExtensionControlDir)
	if err != nil {
		return false, copyExtensionError("extension_missing",
			"the destination extension directory could not be read",
			"Do not copy until the node's PostgreSQL extension directory is readable.")
	}
	prefix := name + "--"
	bases := map[string]bool{}
	updates := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		file := entry.Name()
		if !strings.HasPrefix(file, prefix) || !strings.HasSuffix(file, ".sql") {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(file, prefix), ".sql")
		if rest == "" {
			continue
		}
		if from, to, ok := strings.Cut(rest, "--"); ok {
			if from == "" || to == "" {
				continue
			}
			updates[from] = append(updates[from], to)
			continue
		}
		bases[rest] = true
	}
	if bases[target] {
		return true, nil
	}
	seen := map[string]bool{}
	queue := make([]string, 0, len(bases))
	for version := range bases {
		seen[version] = true
		queue = append(queue, version)
	}
	for len(queue) > 0 {
		version := queue[0]
		queue = queue[1:]
		for _, next := range updates[version] {
			if seen[next] {
				continue
			}
			if next == target {
				return true, nil
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	return false, nil
}

func validateExtensionState(ext ExtensionState) error {
	if !extensionControlNamePattern.MatchString(ext.Name) || ext.Version == "" || ext.Schema == "" ||
		strings.ContainsAny(ext.Version+ext.Schema, "\x00\r\n") {
		return copyExtensionError("extension_incompatible",
			"the extension inventory has an invalid name, version or schema",
			"Do not copy from this source until its extension catalog is complete.")
	}
	return nil
}

func safeExtensionName(value string) string {
	if len(value) > 120 {
		value = value[:120] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, value)
}

func copyExtensionError(reason, message, hint string) error {
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, hint).
		WithDetails(map[string]any{"reason": reason})
}

// copyObjectGrantsTemplate grants app and ro the same ordinary access the
// existing public-schema defaults give them, to objects already in every
// imported application schema, and installs the matching future-owner
// defaults there. Extension-owned schemas and objects are left alone.
const copyObjectGrantsTemplate = `DO $vaire$
DECLARE
  s name;
  r record;
BEGIN
  FOR s IN
    SELECT n.nspname
    FROM pg_namespace n
    WHERE n.nspname <> 'information_schema'
      AND n.nspname <> 'vaire'
      AND n.nspname NOT LIKE 'pg\_%'
      AND NOT EXISTS (
        SELECT 1 FROM pg_depend d
        WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e'
      )
  LOOP
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO {{APP}}, {{RO}}', s);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE {{OWNER}} IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {{APP}}', s);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE {{OWNER}} IN SCHEMA %I GRANT USAGE, SELECT ON SEQUENCES TO {{APP}}', s);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE {{OWNER}} IN SCHEMA %I GRANT EXECUTE ON FUNCTIONS TO {{APP}}', s);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE {{OWNER}} IN SCHEMA %I GRANT SELECT ON TABLES TO {{RO}}', s);
    FOR r IN
      SELECT c.relname, c.relkind
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE n.nspname = s
        AND c.relowner = {{OWNER_LITERAL}}::regrole
        AND c.relkind IN ('r','p','v','m','f','S')
        AND NOT EXISTS (
          SELECT 1 FROM pg_depend d
          WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
        )
    LOOP
      IF r.relkind = 'S' THEN
        EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE %I.%I TO {{APP}}', s, r.relname);
      ELSE
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE %I.%I TO {{APP}}', s, r.relname);
        EXECUTE format('GRANT SELECT ON TABLE %I.%I TO {{RO}}', s, r.relname);
      END IF;
    END LOOP;
    FOR r IN
      SELECT p.proname, pg_get_function_identity_arguments(p.oid) AS args
      FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
      WHERE n.nspname = s
        AND p.proowner = {{OWNER_LITERAL}}::regrole
        AND p.prokind = 'f'
        AND NOT EXISTS (
          SELECT 1 FROM pg_depend d
          WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
        )
    LOOP
      EXECUTE format('GRANT EXECUTE ON FUNCTION %I.%I(%s) TO {{APP}}', s, r.proname, r.args);
    END LOOP;
  END LOOP;
END $vaire$;
`

// CopyObjectGrantsSQL applies vaire's ordinary existing-object access policy
// to every imported application schema in database: schema USAGE for app and
// ro, table DML for app, table SELECT for ro, sequence USAGE/SELECT for app,
// ordinary function EXECUTE for app, and the matching future-owner defaults.
// It skips system and vaire schemas and extension-owned objects, and never
// grants owner or global privileges.
func CopyObjectGrantsSQL(database string) string {
	owner := pg.Quote(database + "_owner")
	app := pg.Quote(database + "_app")
	ro := pg.Quote(database + "_ro")
	return strings.NewReplacer(
		"{{OWNER}}", owner,
		"{{OWNER_LITERAL}}", literal(database+"_owner"),
		"{{APP}}", app,
		"{{RO}}", ro,
	).Replace(copyObjectGrantsTemplate)
}

// copyObjectVerifyTemplate counts imported objects outside vaire's policy.
// app_schemas is exactly the schemas the grant helper touches.
const copyObjectVerifyTemplate = `WITH app_schemas AS (
  SELECT n.oid, n.nspname
  FROM pg_namespace n
  WHERE n.nspname <> 'information_schema'
    AND n.nspname <> 'vaire'
    AND n.nspname NOT LIKE 'pg\_%'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend d
      WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e'
    )
), owned_class AS (
  SELECT c.oid, c.relkind, c.relnamespace
  FROM pg_class c
  WHERE c.relowner = {{OWNER_LITERAL}}::regrole
    AND c.relkind IN ('r','p','v','m','f','S')
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
    )
)
SELECT
  (SELECT count(*) FROM pg_database WHERE datname = current_database() AND pg_get_userbyid(datdba) <> {{OWNER_LITERAL}}) AS database_owner_mismatch,
  (SELECT count(*) FROM pg_database
   WHERE datname = current_database()
     AND (NOT has_database_privilege({{APP_LITERAL}}, datname, 'CONNECT')
       OR NOT has_database_privilege({{RO_LITERAL}}, datname, 'CONNECT')
       OR NOT has_database_privilege({{MIGRATE_LITERAL}}, datname, 'CONNECT'))) AS database_connect_missing,
  (SELECT count(*) FROM pg_roles WHERE rolname = 'vaire_admin'
   AND NOT pg_has_role('vaire_admin', {{OWNER_LITERAL}}::regrole, 'MEMBER')) AS admin_policy_missing,
  (SELECT count(*) FROM pg_roles WHERE rolname = {{MIGRATE_LITERAL}}
   AND NOT pg_has_role({{MIGRATE_LITERAL}}, {{OWNER_LITERAL}}::regrole, 'MEMBER')) AS migrate_owner_missing,
  (SELECT count(*) FROM (
    SELECT c.oid FROM pg_class c JOIN app_schemas s ON s.oid = c.relnamespace
    WHERE c.relkind IN ('r','p','v','m','f','S') AND c.relowner <> {{OWNER_LITERAL}}::regrole
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
    UNION ALL
    SELECT p.oid FROM pg_proc p JOIN app_schemas s ON s.oid = p.pronamespace
    WHERE p.prokind = 'f' AND p.proowner <> {{OWNER_LITERAL}}::regrole
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
    UNION ALL
    SELECT t.oid FROM pg_type t JOIN app_schemas s ON s.oid = t.typnamespace
    WHERE t.typtype IN ('d','e') AND t.typowner <> {{OWNER_LITERAL}}::regrole
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
  ) bad) AS object_owner_mismatch,
  (SELECT count(*) FROM app_schemas s
   WHERE NOT has_schema_privilege({{APP_LITERAL}}, s.oid, 'USAGE')
      OR NOT has_schema_privilege({{RO_LITERAL}}, s.oid, 'USAGE')) AS schema_usage_missing,
  (SELECT count(*) FROM owned_class c JOIN app_schemas s ON s.oid = c.relnamespace
   WHERE c.relkind IN ('r','p','v','m')
     AND (NOT has_table_privilege({{APP_LITERAL}}, c.oid, 'SELECT,INSERT,UPDATE,DELETE')
       OR NOT has_table_privilege({{RO_LITERAL}}, c.oid, 'SELECT'))) AS table_privilege_missing,
  (SELECT count(*) FROM owned_class c JOIN app_schemas s ON s.oid = c.relnamespace
   WHERE c.relkind = 'f'
     AND (NOT has_table_privilege({{APP_LITERAL}}, c.oid, 'SELECT,INSERT,UPDATE,DELETE')
       OR NOT has_table_privilege({{RO_LITERAL}}, c.oid, 'SELECT'))) AS foreign_table_privilege_missing,
  (SELECT count(*) FROM owned_class c JOIN app_schemas s ON s.oid = c.relnamespace
   WHERE c.relkind = 'S'
     AND (NOT has_sequence_privilege({{APP_LITERAL}}, c.oid, 'USAGE')
       OR NOT has_sequence_privilege({{APP_LITERAL}}, c.oid, 'SELECT'))) AS sequence_privilege_missing,
  (SELECT count(*) FROM pg_proc p JOIN app_schemas s ON s.oid = p.pronamespace
   WHERE p.prokind = 'f' AND p.proowner = {{OWNER_LITERAL}}::regrole
     AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
     AND NOT has_function_privilege({{APP_LITERAL}}, p.oid, 'EXECUTE')) AS function_privilege_missing
`

// CopyObjectVerifySQL returns one row of counts of policy violations in
// database. Every count must be zero for a copy to succeed.
func CopyObjectVerifySQL(database string) string {
	return strings.NewReplacer(
		"{{OWNER_LITERAL}}", literal(database+"_owner"),
		"{{APP_LITERAL}}", literal(database+"_app"),
		"{{RO_LITERAL}}", literal(database+"_ro"),
		"{{MIGRATE_LITERAL}}", literal(database+"_migrate"),
	).Replace(copyObjectVerifyTemplate)
}

// CopyAnalyzeSQL runs ANALYZE as the database's owner, not as the postgres
// superuser.
func CopyAnalyzeSQL(database string) string {
	return "SET ROLE " + pg.Quote(database+"_owner") + ";\nANALYZE;\nRESET ROLE;\n"
}

// RoleDirectOnly marks login roles that must connect to PostgreSQL directly:
// PgBouncer's auth query returns nothing for its members, so they are refused
// on 6432 and 443. Each <db>_migrate is a member, because migrations need
// session features that transaction pooling breaks.
const RoleDirectOnly = "vaire_direct_only"

// directOnlyMembersSQL lists the direct members of RoleDirectOnly.
const directOnlyMembersSQL = `SELECT m.rolname FROM pg_auth_members a
  JOIN pg_roles m ON m.oid = a.member JOIN pg_roles g ON g.oid = a.roleid
  WHERE g.rolname = '` + RoleDirectOnly + `'`

// pgbouncerAuthSQL is the SECURITY DEFINER auth functions and their grants
// (§14.2, RFC 8.1): pgbouncer_get_auth for the transaction pooler refuses
// members of vaire_direct_only; pgbouncer_get_auth_session for the session
// pooler refuses only postgres and the pooler's own service roles, and lets
// vaire_admin in for vaire sql, psql, console and tunnel (RFC 6.3). Both refuse
// a role past its VALID UNTIL, so a short-lived write role
// (db.write-access) cannot log in again through a pooled server connection
// once it expires.
const pgbouncerAuthSQL = `CREATE SCHEMA IF NOT EXISTS vaire AUTHORIZATION postgres;
CREATE OR REPLACE FUNCTION vaire.pgbouncer_get_auth(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $vaire$
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','vaire_monitor')
    AND (valuntil IS NULL OR valuntil > now())
    AND NOT pg_has_role(usename, '` + RoleDirectOnly + `', 'MEMBER')
$vaire$;
CREATE OR REPLACE FUNCTION vaire.pgbouncer_get_auth_session(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $vaire$
  -- vaire_admin may log in: operator connections come through here (RFC 6.3).
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','vaire_monitor')
    AND (valuntil IS NULL OR valuntil > now())
$vaire$;
REVOKE ALL ON FUNCTION vaire.pgbouncer_get_auth(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION vaire.pgbouncer_get_auth_session(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA vaire TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION vaire.pgbouncer_get_auth(text) TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION vaire.pgbouncer_get_auth_session(text) TO pgbouncer_auth;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
GRANT CONNECT ON DATABASE postgres TO pgbouncer_auth, vaire_admin, vaire_monitor;
`

// roleVerifier returns the SCRAM verifier for a login role from the request's
// secrets, or input_required when the secret is absent (D5).
func roleVerifier(rt engine.Runtime, secretKey, role, db string) (string, error) {
	password := rt.SecretValues[secretKey]
	if password == "" {
		hint := "Re-run init with --resume so the operator can supply the password."
		if db != "" {
			hint = "Databases are created with vaire db create " + db + ", which supplies the passwords; remove " + db + " from cluster.toml (vaire config edit) and create it that way."
		}
		return "", output.NewError(
			output.CodeInputRequired,
			"the password for role "+role+" was not supplied",
			output.ExitInput,
			hint,
		).WithDetails(map[string]any{"role": role, "secret": secretKey, "database": db})
	}
	secrets.Add(password)
	return pg.SCRAMVerifier(password)
}

// writeRuntimePass writes /etc/vaire/runtime/<name>.pass from a supplied secret
// when the file is missing. It never generates a password on the node (D5).
func writeRuntimePass(rt engine.Runtime, name, secretKey string) error {
	path := RuntimeDir + "/" + name + ".pass"
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	password := rt.SecretValues[secretKey]
	if password == "" {
		return output.NewError(
			output.CodeInputRequired,
			"the service password for "+name+" was not supplied",
			output.ExitInput,
			"Re-run init with --resume so the operator can supply svc."+name+".",
		).WithDetails(map[string]any{"secret": secretKey, "path": path})
	}
	secrets.Add(password)
	return writeFile(path, password+"\n", 0o600)
}

// orphanedWarnings reports databases in spec's cluster other than its own
// that look like vaire databases.
func orphanedWarnings(spec config.ClusterSpec, dbs []string, roleSet map[string]bool) []output.Warning {
	var warnings []output.Warning
	for _, name := range dbs {
		if systemDatabases[name] || name == spec.Name {
			continue
		}
		if roleSet[name+"_owner"] {
			warnings = append(warnings, output.Warning{
				Code:    "orphaned_database",
				Message: "database " + name + " in cluster " + spec.Name + " is not in cluster.toml",
			})
		}
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Message < warnings[j].Message })
	return warnings
}

func queryColumn(ctx context.Context, local pg.Local, db, sql string) ([]string, error) {
	rows, err := local.Query(ctx, db, sql, nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		for _, v := range row {
			out = append(out, v)
			break
		}
	}
	return out, nil
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// prefixed returns name_role for each role.
func prefixed(name string, roles []string) []string {
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		out = append(out, name+"_"+role)
	}
	return out
}

// AuthFunctionsSQL is the pooler auth functions and their grants, run in
// AuthDB. A restore from a dump creates them in its new cluster.
func AuthFunctionsSQL() string { return pgbouncerAuthSQL }
