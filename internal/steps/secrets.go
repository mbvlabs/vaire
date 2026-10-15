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
	// SecretMetricsHash is the bcrypt hash of the /metrics credential
	// (Phase 4). The operator computes it; the node never sees the password.
	SecretMetricsHash = "metrics.htpasswd" //nolint:gosec // G101: a key name, not a value.
)

// Backup values on the RPC wire (Phase 3). The S3 secret key and the
// pgBackRest cipher pass are secrets. The key ID and the age recipient are not
// secret; they ride in the same map because it is the only per-request
// channel a step gets, and like the secrets they never enter an input hash.
const (
	SecretBackupKeySecret    = "backup.s3_key_secret" //nolint:gosec // G101: a key name, not a value.
	SecretBackupCipherPass   = "backup.cipher_pass"   //nolint:gosec // G101: a key name, not a value.
	SecretBackupKeyID        = "backup.s3_key_id"     //nolint:gosec // G101: a key name, not a value.
	SecretBackupAgeRecipient = "backup.age_recipient" //nolint:gosec // G101: a key name, not a value.
	// SecretBackupAgeIdentity opens long-retention dumps. Only a restore from
	// a dump sends it, and the node keeps it in memory for that job.
	SecretBackupAgeIdentity = "backup.age_identity" //nolint:gosec // G101: a key name, not a value.
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
