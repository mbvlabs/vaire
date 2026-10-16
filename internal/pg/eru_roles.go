package pg

import (
	"slices"
	"strings"
)

// eru's own roles. Application roles (<db>_app, <db>_ro, <db>_migrate,
// <db>_owner) are not among them: their sessions are the application's, not
// eru's.
const (
	RoleAdmin    = "eru_admin"
	RoleMonitor  = "eru_monitor"
	RoleAuth     = "pgbouncer_auth"
	roleWritePfx = "eru_write_"
)

// Node steps also run as postgres, and protected maintenance switches to
// PostgreSQL's maintenance/signal roles. Hide those administrative roles too;
// --all-sessions includes them, including manually issued administrative work.
var eruRoles = [...]string{RoleAdmin, RoleMonitor, RoleAuth, "postgres", "pg_maintain", "pg_signal_backend"}

// IsEruRole reports whether role is one of eru's own: the roles eru uses
// (<db> or <db>__restoring over the socket, eru sql/psql/console/top, the
// exporters) or a short-lived write role (db.write-access). eru queries and
// eru top leave their sessions out by default.
func IsEruRole(role string) bool {
	return slices.Contains(eruRoles[:], role) || strings.HasPrefix(role, roleWritePfx)
}

// EruRoleSQL is the SQL equivalent of IsEruRole. column is a trusted SQL
// expression, never user input. Filter before LIMIT so internal work cannot
// crowd application statements out of a top-N result.
func EruRoleSQL(column string) string {
	return "(" + column + " IN ('" + strings.Join(eruRoles[:], "', '") + "') OR starts_with(" + column + ", '" + roleWritePfx + "'))"
}

// IsEruSession reports whether a session is eru's own: one of eru's roles, or
// any client that set an application_name beginning with "eru" (eru top,
// eru-console). Both eru queries and eru top leave these out unless
// --all-sessions is given.
func IsEruSession(role, app string) bool {
	return IsEruRole(role) || strings.HasPrefix(app, "eru")
}
