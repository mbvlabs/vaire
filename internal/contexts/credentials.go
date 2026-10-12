package contexts

// Role is a database user and its password.
type Role struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// DBSecret is one database's roles and connection URLs.
type DBSecret struct {
	Name  string            `json:"name"`
	Roles map[string]Role   `json:"roles"`
	URLs  map[string]string `json:"urls"`
}

// DatabaseCreds holds the cluster's database credentials: the admin role, one
// entry per application database, and the service roles (D4).
type DatabaseCreds struct {
	Admin     Role            `json:"admin"`
	Databases []DBSecret      `json:"databases"`
	Services  map[string]Role `json:"services,omitempty"`
}

// DBRole returns the user and password for a database role.
func (c DatabaseCreds) DBRole(db, role string) (user, password string, ok bool) {
	for _, d := range c.Databases {
		if d.Name != db {
			continue
		}
		if r, found := d.Roles[role]; found {
			return r.User, r.Password, true
		}
		return "", "", false
	}
	return "", "", false
}

// Database returns the credential entry called name.
func (c DatabaseCreds) Database(name string) (DBSecret, bool) {
	for _, d := range c.Databases {
		if d.Name == name {
			return d, true
		}
	}
	return DBSecret{}, false
}

// SecretValues returns every secret in the credentials.
func (c DatabaseCreds) SecretValues() []string {
	out := []string{c.Admin.Password}
	for _, db := range c.Databases {
		for _, r := range db.Roles {
			out = append(out, r.Password)
		}
		for _, u := range db.URLs {
			out = append(out, u)
		}
	}
	for _, r := range c.Services {
		out = append(out, r.Password)
	}
	return out
}

// DBRole returns the user and password for a database role in the credentials.
func (c Credentials) DBRole(db, role string) (user, password string, ok bool) {
	if c.Database == nil {
		return "", "", false
	}
	return c.Database.DBRole(db, role)
}

// Databases lists the application database names in the credentials.
func (c Credentials) Databases() []string {
	if c.Database == nil {
		return nil
	}
	out := make([]string, 0, len(c.Database.Databases))
	for _, db := range c.Database.Databases {
		out = append(out, db.Name)
	}
	return out
}
