package contexts

// Role is a database user and its password.
type Role struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// DatabaseCreds holds the setup-wide database credentials: the admin role
// and the service roles (D4). Application database passwords are never kept:
// eru db create shows them once.
type DatabaseCreds struct {
	Admin    Role            `json:"admin"`
	Services map[string]Role `json:"services,omitempty"`
}

// SecretValues returns every secret in the credentials.
func (c DatabaseCreds) SecretValues() []string {
	out := []string{c.Admin.Password}
	for _, r := range c.Services {
		out = append(out, r.Password)
	}
	return out
}

// MetricsUser is the user of the /metrics endpoint's basic auth.
const MetricsUser = "metrics"

// MetricsCreds is the /metrics endpoint's basic-auth credential (Phase 4).
// The operator generates it; the node gets only its bcrypt hash.
type MetricsCreds struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// BackupCreds is the recovery kit (Phases 3 and 5): where the bucket is, its
// keys, the pgBackRest repository cipher and the age identity that opens the
// long-retention dumps. The operator generates the cipher and the identity
// and stores them here before anything reaches the node. The node never sees
// the age identity except for the length of a restore from a dump.
type BackupCreds struct {
	Provider        string `json:"provider"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix"`
	URIStyle        string `json:"uri_style"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	PgBackRest      struct {
		CipherPass string `json:"cipher_pass"`
	} `json:"pgbackrest"`

	// Dumps holds the age X25519 identity the long-retention dumps are
	// encrypted to.
	Dumps struct {
		AgeIdentity  string `json:"age_identity,omitempty"`
		AgeRecipient string `json:"age_recipient,omitempty"`
	} `json:"dumps"`
}

// HasBucket reports whether the kit holds a bucket's keys and cipher, not
// only the recovery identity server init creates.
func (b BackupCreds) HasBucket() bool {
	return b.AccessKeyID != "" || b.PgBackRest.CipherPass != ""
}

// SecretValues returns every secret in the recovery kit.
func (b BackupCreds) SecretValues() []string {
	return []string{b.SecretAccessKey, b.PgBackRest.CipherPass, b.Dumps.AgeIdentity}
}
