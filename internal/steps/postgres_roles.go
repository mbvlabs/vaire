package steps

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/catalog"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/secrets"
)

const postgresRolesID = "postgres-roles-v1"

// systemDatabases are never reported as orphaned.
var systemDatabases = map[string]bool{"postgres": true, "template0": true, "template1": true}

// postgresRoles creates, in one cluster, the service roles, eru_direct_only,
// the eru schema with both pooler auth functions, and that cluster's
// database with its four roles, grants and extensions (RFC 12).
type postgresRoles struct{ clusterStep }

func (s postgresRoles) ID() string { return s.id(postgresRolesID) }

func (postgresRoles) Instance(cluster string) engine.Step {
	return postgresRoles{clusterStep{cluster: cluster}}
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
	// MonitorSQL joins the inputs so every cluster re-runs it once when it
	// changes (Phase 4).
	MonitorSQL string         `json:"monitor_sql"`
	DB         databaseInputs `json:"database"`
}

type databaseInputs struct {
	Name             string   `json:"name"`
	StatementTimeout string   `json:"statement_timeout"`
	Extensions       []string `json:"extensions"`
	// Protected takes eru_admin's write access away (AdminAccessSQL), so
	// protecting or lifting a protection re-runs the step.
	Protected bool `json:"protected,omitempty"`
}

func (s postgresRoles) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil, err
	}
	return canonical(roleInputs{
		Admin: true, Monitor: true, Auth: true, AuthSQL: pgbouncerAuthSQL, MonitorSQL: monitorSQL,
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
	for _, role := range append([]string{"eru_admin", "eru_monitor", "pgbouncer_auth", RoleDirectOnly, spec.Name + "_owner"}, prefixed(spec.Name, DatabaseRoles)...) {
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
		src, err := queryColumn(ctx, local, "postgres", "SELECT prosrc FROM pg_proc WHERE proname = '"+fn.name+"'")
		if err != nil || len(src) == 0 {
			return incomplete("eru." + fn.name + " is missing"), nil
		}
		if !strings.Contains(src[0], fn.needs) {
			return incomplete("eru." + fn.name + " is out of date"), nil
		}
	}
	member, err := queryColumn(ctx, local, "postgres", adminOwnerMemberSQL(spec.Name))
	if err != nil || len(member) != 1 {
		return incomplete("cannot read eru_admin's membership in " + spec.Name + "_owner"), nil
	}
	if (member[0] == "t") == spec.Protected {
		if spec.Protected {
			return incomplete("eru_admin can still write to protected " + spec.Name), nil
		}
		return incomplete("eru_admin is not a member of " + spec.Name + "_owner"), nil
	}
	signal, err := queryColumn(ctx, local, "postgres", adminSignalSQL)
	if err != nil || len(signal) != 1 {
		return incomplete("cannot read eru_admin's membership in pg_signal_backend"), nil
	}
	if (signal[0] == "t") == spec.Protected {
		if spec.Protected {
			return incomplete("eru_admin can still terminate sessions of protected " + spec.Name), nil
		}
		return incomplete("eru_admin is not a member of pg_signal_backend"), nil
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
	cluster.WriteString("-- managed by eru - do not edit; use cluster.toml\n")
	service := []struct {
		role, secret, attrs, grants string
	}{
		{"eru_admin", SecretAdminPassword, "LOGIN CREATEDB CREATEROLE", adminGrants(spec)},
		{"eru_monitor", SecretMonitor, "LOGIN", "pg_monitor"},
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
	if err := local.Exec(ctx, "postgres", cluster.String(), nil); err != nil {
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

// monitorSQL sets eru_monitor up for the exporters (Phase 4 D4): a
// postgres_exporter probe needs a connection slot exactly when the pools
// are full, so the role shares the three reserved slots with eru_admin.
// CONNECTION LIMIT 2 is one probe: postgres_exporter 0.20.1's /probe opens
// two connections at once (the legacy exporter's, for pg_up and the default
// metrics, and the probe collectors'), so a limit of 1 would fail one of
// them on every scrape; it leaves at least one reserved slot to eru_admin.
// The timeouts keep a probe from holding a backend or waiting on a lock.
const monitorSQL = `GRANT pg_use_reserved_connections TO eru_monitor;
ALTER ROLE eru_monitor CONNECTION LIMIT 2;
ALTER ROLE eru_monitor SET statement_timeout = '10s';
ALTER ROLE eru_monitor SET lock_timeout = '1s';
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

// adminOwnerMemberSQL reports, as t or f, whether eru_admin is a member of
// db's owner role, directly or not.
func adminOwnerMemberSQL(db string) string {
	return "SELECT pg_has_role('eru_admin', " + literal(db+"_owner") + ", 'MEMBER')"
}

// adminSignalSQL reports, as t or f, whether eru_admin is a member of
// pg_signal_backend.
const adminSignalSQL = "SELECT pg_has_role('eru_admin', 'pg_signal_backend', 'MEMBER')"

// adminGrants are the predefined roles eru_admin holds in spec's cluster.
// On a protected database it holds no pg_signal_backend: it could end the
// database's sessions from a read-only eru sql, and eru maint kill there
// needs a code from the authenticator app (node.handleMaintKill terminates
// as the superuser with SET ROLE pg_signal_backend instead).
func adminGrants(spec config.ClusterSpec) string {
	if spec.Protected {
		return "pg_monitor, pg_maintain, pg_use_reserved_connections"
	}
	return "pg_monitor, pg_signal_backend, pg_maintain, pg_use_reserved_connections"
}

// AdminAccessSQL is eru_admin's access to db, run in postgres. On an
// unprotected database eru_admin is a member of <db>_owner, so eru sql,
// psql and console --write act as the owner. On a protected one it is not:
// every grant of the owner role to eru_admin goes, with CASCADE, so the
// grants eru_admin made from its ADMIN OPTION go too, and the owner role
// cannot log in. eru_admin gets pg_read_all_data for reads, keeps
// pg_maintain for vacuum, analyze and reindex, and loses pg_signal_backend
// (adminGrants). A write then needs a
// short-lived role the node creates after it checks a code from the
// authenticator app (db.write-access). eru_admin's own sessions on the
// database end the moment its membership goes, since one may have SET ROLE
// to the owner already. The eru_admin password alone, in a context or
// through eru tunnel, can then only read a protected database.
func AdminAccessSQL(db config.ClusterSpec) string {
	owner := db.Name + "_owner"
	if !db.Protected {
		return fmt.Sprintf(`GRANT %s TO eru_admin WITH ADMIN OPTION;
DO $eru$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_auth_members WHERE roleid = 'pg_read_all_data'::regrole AND member = 'eru_admin'::regrole) THEN
    REVOKE pg_read_all_data FROM eru_admin;
  END IF;
END $eru$;
`, pg.Quote(owner))
	}
	return fmt.Sprintf(`GRANT pg_read_all_data TO eru_admin;
ALTER ROLE %[1]s NOLOGIN;
DO $eru$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_auth_members WHERE roleid = 'pg_signal_backend'::regrole AND member = 'eru_admin'::regrole) THEN
    REVOKE pg_signal_backend FROM eru_admin;
  END IF;
END $eru$;
DO $eru$
DECLARE
  g name;
  was boolean := pg_has_role('eru_admin', %[2]s, 'MEMBER');
BEGIN
  FOR g IN SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.grantor
      WHERE m.roleid = %[2]s::regrole AND m.member = 'eru_admin'::regrole LOOP
    BEGIN
      EXECUTE format('REVOKE %%I FROM eru_admin GRANTED BY %%I CASCADE', %[2]s, g);
    EXCEPTION WHEN OTHERS THEN
      NULL; -- gone already with an earlier grant's CASCADE
    END;
  END LOOP;
  IF pg_has_role('eru_admin', %[2]s, 'MEMBER') THEN
    RAISE EXCEPTION 'eru_admin is still a member of %%', %[2]s;
  END IF;
  IF was THEN
    PERFORM pg_terminate_backend(pid) FROM pg_stat_activity
      WHERE usename = 'eru_admin' AND datname = %[3]s AND backend_type = 'client backend' AND pid <> pg_backend_pid();
  END IF;
END $eru$;
`, pg.Quote(owner), literal(owner), literal(db.Name))
}

// literal quotes s as an SQL string literal.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// DatabaseConnectSQL is the database-level access SQL for database target,
// granted to the roles of roleDB, run in postgres. A restore from a dump
// uses it for the database it loads into a new cluster.
func DatabaseConnectSQL(target, roleDB string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REVOKE ALL ON DATABASE %s FROM PUBLIC;\n", pg.Quote(target))
	fmt.Fprintf(&b, "GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s, %s, %s, eru_admin, eru_monitor;\n",
		pg.Quote(target), pg.Quote(roleDB+"_migrate"), pg.Quote(roleDB+"_app"), pg.Quote(roleDB+"_ro"))
	return b.String()
}

// DatabaseGrantsSQL is postgres-roles-v1's schema, default-privilege and
// extension SQL for db, run inside the database it is for. A restore from a
// dump and eru db fork run it in the databases they load.
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

// RoleDirectOnly marks login roles that must connect to PostgreSQL directly:
// PgBouncer's auth query returns nothing for its members, so they are refused
// on 6432 and 443. Each <db>_migrate is a member, because migrations need
// session features that transaction pooling breaks.
const RoleDirectOnly = "eru_direct_only"

// directOnlyMembersSQL lists the direct members of RoleDirectOnly.
const directOnlyMembersSQL = `SELECT m.rolname FROM pg_auth_members a
  JOIN pg_roles m ON m.oid = a.member JOIN pg_roles g ON g.oid = a.roleid
  WHERE g.rolname = '` + RoleDirectOnly + `'`

// pgbouncerAuthSQL is the SECURITY DEFINER auth functions and their grants
// (§14.2, RFC 8.1): pgbouncer_get_auth for the transaction pooler refuses
// members of eru_direct_only; pgbouncer_get_auth_session for the session
// pooler refuses only postgres and the pooler's own service roles, and lets
// eru_admin in for eru sql, psql, console and tunnel (RFC 6.3). Both refuse
// a role past its VALID UNTIL, so a short-lived write role
// (db.write-access) cannot log in again through a pooled server connection
// once it expires.
const pgbouncerAuthSQL = `CREATE SCHEMA IF NOT EXISTS eru AUTHORIZATION postgres;
CREATE OR REPLACE FUNCTION eru.pgbouncer_get_auth(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $eru$
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','eru_monitor')
    AND (valuntil IS NULL OR valuntil > now())
    AND NOT pg_has_role(usename, '` + RoleDirectOnly + `', 'MEMBER')
$eru$;
CREATE OR REPLACE FUNCTION eru.pgbouncer_get_auth_session(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $eru$
  -- eru_admin may log in: operator connections come through here (RFC 6.3).
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','eru_monitor')
    AND (valuntil IS NULL OR valuntil > now())
$eru$;
REVOKE ALL ON FUNCTION eru.pgbouncer_get_auth(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION eru.pgbouncer_get_auth_session(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA eru TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION eru.pgbouncer_get_auth(text) TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION eru.pgbouncer_get_auth_session(text) TO pgbouncer_auth;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
GRANT CONNECT ON DATABASE postgres TO pgbouncer_auth, eru_admin, eru_monitor;
`

// roleVerifier returns the SCRAM verifier for a login role from the request's
// secrets, or input_required when the secret is absent (D5).
func roleVerifier(rt engine.Runtime, secretKey, role, db string) (string, error) {
	password := rt.SecretValues[secretKey]
	if password == "" {
		hint := "Re-run init with --resume so the operator can supply the password."
		if db != "" {
			hint = "Databases are created with eru db create " + db + ", which supplies the passwords; remove " + db + " from cluster.toml (eru config edit) and create it that way."
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

// writeRuntimePass writes /etc/eru/runtime/<name>.pass from a supplied secret
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
// that look like eru databases.
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
// postgres. A restore from a dump creates them in its new cluster.
func AuthFunctionsSQL() string { return pgbouncerAuthSQL }
