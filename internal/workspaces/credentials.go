package workspaces

// Role is a database user and its password.
type Role struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// DatabaseCreds holds the setup-wide database credentials: the admin role
// and the service roles (D4). Application database passwords are never kept:
// vaire db create shows them once.
type DatabaseCreds struct {
	Admin    Role            `json:"admin"`
	Services map[string]Role `json:"services,omitempty"`
}

// SecretValues returns every secret in the credentials.
func (c DatabaseCreds) SecretValues() []string {
	out := make([]string, 1, 1+len(c.Services))

	out[0] = c.Admin.Password
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
// keys, the pgBackRest repository cipher and the age key pair that encrypts
// and opens the master recovery bundles. The operator generates the cipher
// and the identity and stores them here before anything reaches the node.
// The node only ever receives the recipient.
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

	// Encryption is the age X25519 key pair for the master recovery
	// bundles: the node encrypts to Recipient, and Identity stays on the
	// operator to open them.
	Encryption struct {
		Identity  string `json:"identity,omitempty"`
		Recipient string `json:"recipient,omitempty"`
	} `json:"encryption"`
}

// HasBucket reports whether the kit holds a bucket's keys and cipher, not
// only the recovery identity server init creates.
func (b BackupCreds) HasBucket() bool {
	return b.AccessKeyID != "" || b.PgBackRest.CipherPass != ""
}

// SecretValues returns every secret in the recovery kit.
func (b BackupCreds) SecretValues() []string {
	return []string{b.SecretAccessKey, b.PgBackRest.CipherPass, b.Encryption.Identity}
}
