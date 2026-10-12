package steps

// Secret key names on the RPC wire (D6). The operator generates every database
// password and passes it in the request's secrets map.
const (
	// SecretAdminPassword is the eru_admin password.
	SecretAdminPassword = "db.admin"
	// SecretPgbouncerAuth is the pgbouncer_auth service password key name.
	SecretPgbouncerAuth = "svc.pgbouncer_auth" //nolint:gosec // G101: a key name, not a value.
	// SecretMonitor is the eru_monitor service password key name.
	SecretMonitor = "svc.eru_monitor" //nolint:gosec // G101: a key name, not a value.
)

// Database roles with a login and a password.
const (
	RoleMigrate = "migrate"
	RoleApp     = "app"
	RoleRO      = "ro"
)

// DatabaseRoles lists the login roles eru creates per database.
var DatabaseRoles = []string{RoleMigrate, RoleApp, RoleRO}

// SecretDatabaseRole is the secret key for a database role password, for
// example db.app.migrate.
func SecretDatabaseRole(db, role string) string {
	return "db." + db + "." + role
}

// ServiceRoles are the cluster-wide service roles with a stored password.
var ServiceRoles = []string{"pgbouncer_auth", "eru_monitor"}

// ServiceRoleSecret maps a service role name to its secret key.
func ServiceRoleSecret(role string) string {
	switch role {
	case "pgbouncer_auth":
		return SecretPgbouncerAuth
	case "eru_monitor":
		return SecretMonitor
	}
	return ""
}
