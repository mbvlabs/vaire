package pg

import (
	"slices"
	"strings"
)

// vaire's own roles. Application roles (<db>_app, <db>_ro, <db>_migrate,
// <db>_owner) are not among them: their sessions are the application's, not
// vaire's.
const (
	RoleAdmin    = "vaire_admin"
	RoleMonitor  = "vaire_monitor"
	RoleAuth     = "pgbouncer_auth"
	roleWritePfx = "vaire_write_"
)

// Node steps also run as postgres, and protected maintenance switches to
// PostgreSQL's maintenance/signal roles. Hide those administrative roles too;
// --all-sessions includes them, including manually issued administrative work.
var vaireRoles = [...]string{RoleAdmin, RoleMonitor, RoleAuth, "postgres", "pg_maintain", "pg_signal_backend"}

// IsVaireRole reports whether role is one of vaire's own: the roles vaire uses
// (<db> or <db>__restoring over the socket, vaire sql/psql/console/top, the
// exporters) or a short-lived write role (db.write-access). vaire queries and
// vaire top leave their sessions out by default.
func IsVaireRole(role string) bool {
	return slices.Contains(vaireRoles[:], role) || strings.HasPrefix(role, roleWritePfx)
}

// VaireRoleSQL is the SQL equivalent of IsVaireRole. column is a trusted SQL
// expression, never user input. Filter before LIMIT so internal work cannot
// crowd application statements out of a top-N result.
func VaireRoleSQL(column string) string {
	return "(" + column + " IN ('" + strings.Join(vaireRoles[:], "', '") + "') OR starts_with(" + column + ", '" + roleWritePfx + "'))"
}

// IsVaireSession reports whether a session is vaire's own: one of vaire's roles, or
// any client that set an application_name beginning with "vaire" (vaire top,
// vaire-console). Both vaire queries and vaire top leave these out unless
// --all-sessions is given.
func IsVaireSession(role, app string) bool {
	return IsVaireRole(role) || strings.HasPrefix(app, "vaire")
}
