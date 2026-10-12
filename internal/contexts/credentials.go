package contexts

// Role is a database user and its password.
type Role struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// DatabaseCreds holds the cluster-wide database credentials: the admin role
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
