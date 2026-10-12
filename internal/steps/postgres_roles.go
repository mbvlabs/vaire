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

type postgresRoles struct{}

func (postgresRoles) ID() string { return postgresRolesID }

func (postgresRoles) Describe(config.Cluster) string {
	return "create roles, databases, grants and extensions"
}

// roleInputs is everything Apply depends on except passwords (D5).
type roleInputs struct {
	Admin   bool             `json:"admin"`
	Monitor bool             `json:"monitor"`
	Auth    bool             `json:"auth"`
	DBs     []databaseInputs `json:"databases"`
}

type databaseInputs struct {
	Name             string   `json:"name"`
	StatementTimeout string   `json:"statement_timeout"`
	Extensions       []string `json:"extensions"`
}

func buildRoleInputs(cfg config.Cluster) roleInputs {
	in := roleInputs{Admin: true, Monitor: true, Auth: true}
	for _, db := range cfg.Databases {
		in.DBs = append(in.DBs, databaseInputs{
			Name:             db.Name,
			StatementTimeout: db.StatementTimeout,
			Extensions:       db.Extensions,
		})
	}
	return in
}

func (postgresRoles) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	return canonical(buildRoleInputs(cfg))
}

func (postgresRoles) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	roles, err := queryColumn(ctx, rt, "postgres", "SELECT rolname FROM pg_roles")
	if err != nil {
		return incomplete("cannot read pg_roles"), nil
	}
	roleSet := stringSet(roles)
	for _, role := range []string{"eru_admin", "eru_monitor", "pgbouncer_auth"} {
		if !roleSet[role] {
			return incomplete("role " + role + " is missing"), nil
		}
	}
	dbs, err := queryColumn(ctx, rt, "postgres", "SELECT datname FROM pg_database")
	if err != nil {
		return incomplete("cannot read pg_database"), nil
	}
	dbSet := stringSet(dbs)
	for _, db := range cfg.Databases {
		for _, role := range append([]string{db.Name + "_owner"}, prefixed(db.Name, DatabaseRoles)...) {
			if !roleSet[role] {
				return incomplete("role " + role + " is missing"), nil
			}
		}
		if !dbSet[db.Name] {
			return incomplete("database " + db.Name + " is missing"), nil
		}
	}
	if !roleSet[RoleDirectOnly] {
		return incomplete("role " + RoleDirectOnly + " is missing"), nil
	}
	fn, err := queryColumn(ctx, rt, "postgres", "SELECT prosrc FROM pg_proc WHERE proname = 'pgbouncer_get_auth'")
	if err != nil || len(fn) == 0 {
		return incomplete("eru.pgbouncer_get_auth is missing"), nil
	}
	if !strings.Contains(fn[0], RoleDirectOnly) {
		return incomplete("eru.pgbouncer_get_auth does not refuse " + RoleDirectOnly), nil
	}
	direct, err := queryColumn(ctx, rt, "postgres", directOnlyMembersSQL)
	if err != nil {
		return incomplete("cannot read " + RoleDirectOnly + " members"), nil
	}
	directSet := stringSet(direct)
	for _, db := range cfg.Databases {
		if !directSet[db.Name+"_"+RoleMigrate] {
			return incomplete("role " + db.Name + "_" + RoleMigrate + " is not in " + RoleDirectOnly), nil
		}
	}
	for _, db := range cfg.Databases {
		installed, err := queryColumn(ctx, rt, db.Name, "SELECT extname FROM pg_extension")
		if err != nil {
			return incomplete("cannot read extensions in " + db.Name), nil
		}
		have := stringSet(installed)
		for _, ext := range db.Extensions {
			entry, ok := catalog.Lookup(ext)
			if !ok || entry.ExtName == "" {
				continue
			}
			if !have[entry.ExtName] {
				return incomplete("extension " + entry.ExtName + " is missing in " + db.Name), nil
			}
		}
	}
	for _, name := range []string{"pgbouncer_auth", "monitor"} {
		if _, err := os.Stat(RuntimeDir + "/" + name + ".pass"); err != nil {
			return incomplete(RuntimeDir + "/" + name + ".pass is missing"), nil
		}
	}
	return complete(), nil
}

func (s postgresRoles) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	roles, err := queryColumn(ctx, rt, "postgres", "SELECT rolname FROM pg_roles")
	if err != nil {
		return engine.ApplyResult{}, err
	}
	roleSet := stringSet(roles)
	dbs, err := queryColumn(ctx, rt, "postgres", "SELECT datname FROM pg_database")
	if err != nil {
		return engine.ApplyResult{}, err
	}
	dbSet := stringSet(dbs)

	var cluster strings.Builder
	cluster.WriteString("-- managed by eru - do not edit; use cluster.toml\n")
	if !roleSet["eru_admin"] {
		v, err := roleVerifier(rt, SecretAdminPassword, "eru_admin", "")
		if err != nil {
			return engine.ApplyResult{}, err
		}
		fmt.Fprintf(&cluster, "CREATE ROLE %s LOGIN CREATEDB CREATEROLE PASSWORD '%s';\n", pg.Quote("eru_admin"), v)
	}
	cluster.WriteString("GRANT pg_monitor, pg_signal_backend, pg_maintain, pg_use_reserved_connections TO eru_admin;\n")
	if !roleSet["eru_monitor"] {
		v, err := roleVerifier(rt, SecretMonitor, "eru_monitor", "")
		if err != nil {
			return engine.ApplyResult{}, err
		}
		fmt.Fprintf(&cluster, "CREATE ROLE %s LOGIN PASSWORD '%s';\n", pg.Quote("eru_monitor"), v)
	}
	cluster.WriteString("GRANT pg_monitor TO eru_monitor;\n")
	if !roleSet["pgbouncer_auth"] {
		v, err := roleVerifier(rt, SecretPgbouncerAuth, "pgbouncer_auth", "")
		if err != nil {
			return engine.ApplyResult{}, err
		}
		fmt.Fprintf(&cluster, "CREATE ROLE %s LOGIN PASSWORD '%s';\n", pg.Quote("pgbouncer_auth"), v)
	}
	if !roleSet[RoleDirectOnly] {
		fmt.Fprintf(&cluster, "CREATE ROLE %s NOLOGIN;\n", RoleDirectOnly)
	}
	cluster.WriteString(pgbouncerAuthSQL)

	for _, db := range cfg.Databases {
		sql, err := databaseSQL(ctx, rt, db, roleSet, dbSet)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		cluster.WriteString(sql)
	}
	if err := rt.PG.Exec(ctx, "postgres", cluster.String(), nil); err != nil {
		return engine.ApplyResult{}, err
	}

	for _, db := range cfg.Databases {
		if err := rt.PG.Exec(ctx, db.Name, databaseGrantsSQL(db), nil); err != nil {
			return engine.ApplyResult{}, err
		}
	}

	if err := writeRuntimePass(rt, "pgbouncer_auth", SecretPgbouncerAuth); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeRuntimePass(rt, "monitor", SecretMonitor); err != nil {
		return engine.ApplyResult{}, err
	}

	warnings := orphanedWarnings(cfg, dbs, roleSet)
	return engine.ApplyResult{Restart: engine.RestartNone, Warnings: warnings}, nil
}

// databaseSQL creates the roles and the database, cluster-wide, in postgres.
func databaseSQL(ctx context.Context, rt engine.Runtime, db config.Database, roleSet, dbSet map[string]bool) (string, error) {
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
	fmt.Fprintf(&b, "REVOKE ALL ON DATABASE %s FROM PUBLIC;\n", pg.Quote(db.Name))
	fmt.Fprintf(&b, "GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s, %s, %s, eru_admin, eru_monitor;\n",
		pg.Quote(db.Name), pg.Quote(db.Name+"_migrate"), pg.Quote(db.Name+"_app"), pg.Quote(db.Name+"_ro"))
	fmt.Fprintf(&b, "GRANT %s TO eru_admin WITH ADMIN OPTION;\n", pg.Quote(owner))
	return b.String(), nil
}

// databaseGrantsSQL is the schema and default-privilege SQL run inside db.
func databaseGrantsSQL(db config.Database) string {
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

// pgbouncerAuthSQL is the SECURITY DEFINER function and its grants (§14.2).
const pgbouncerAuthSQL = `CREATE SCHEMA IF NOT EXISTS eru AUTHORIZATION postgres;
CREATE OR REPLACE FUNCTION eru.pgbouncer_get_auth(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $eru$
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','eru_monitor')
    AND NOT pg_has_role(usename, '` + RoleDirectOnly + `', 'MEMBER')
$eru$;
REVOKE ALL ON FUNCTION eru.pgbouncer_get_auth(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA eru TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION eru.pgbouncer_get_auth(text) TO pgbouncer_auth;
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

// orphanedWarnings reports databases that exist but are no longer in
// cluster.toml and look like eru databases.
func orphanedWarnings(cfg config.Cluster, dbs []string, roleSet map[string]bool) []output.Warning {
	configured := map[string]bool{}
	for _, db := range cfg.Databases {
		configured[db.Name] = true
	}
	var warnings []output.Warning
	for _, name := range dbs {
		if systemDatabases[name] || configured[name] {
			continue
		}
		if roleSet[name+"_owner"] {
			warnings = append(warnings, output.Warning{
				Code:    "orphaned_database",
				Message: "database " + name + " is no longer in cluster.toml; drop it with eru db drop " + name,
			})
		}
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Message < warnings[j].Message })
	return warnings
}

func queryColumn(ctx context.Context, rt engine.Runtime, db, sql string) ([]string, error) {
	rows, err := rt.PG.Query(ctx, db, sql, nil)
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
