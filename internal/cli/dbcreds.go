package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/secretsdoc"
	"github.com/mbvlabs/eru/internal/steps"
)

// ensureClusterSecrets fills the operator's cluster-wide database
// credentials, the admin and service roles, generating any password it does
// not have (D7). It returns the RPC secret map and whether anything was
// generated. Application database passwords are never generated here: only
// eru db create and eru db credentials --rotate make them.
func ensureClusterSecrets(creds *contexts.DatabaseCreds) (map[string]string, bool, error) {
	if creds.Admin.User == "" {
		creds.Admin.User = "eru_admin"
	}
	generated := false
	rpc := map[string]string{}

	if creds.Admin.Password == "" {
		pw, err := secrets.Base62(32)
		if err != nil {
			return nil, false, err
		}
		creds.Admin.Password = pw
		generated = true
	}
	rpc[steps.SecretAdminPassword] = creds.Admin.Password

	if creds.Services == nil {
		creds.Services = map[string]contexts.Role{}
	}
	for _, role := range steps.ServiceRoles {
		r := creds.Services[role]
		if r.User == "" {
			r.User = role
		}
		if r.Password == "" {
			pw, err := secrets.Base62(32)
			if err != nil {
				return nil, false, err
			}
			r.Password = pw
			generated = true
		}
		creds.Services[role] = r
		rpc[steps.ServiceRoleSecret(role)] = r.Password
	}
	return rpc, generated, nil
}

// databaseSecret is one application database's roles and connection URLs.
// eru shows it once and never stores it.
type databaseSecret struct {
	Name  string                   `json:"name"`
	Roles map[string]contexts.Role `json:"roles"`
	URLs  map[string]string        `json:"urls"`
	Notes string                   `json:"notes"`
}

// newDatabaseSecret generates passwords for roles of database name and
// returns them with the RPC secret map.
func newDatabaseSecret(cfg config.Cluster, name string, roles []string) (databaseSecret, map[string]string, error) {
	out := databaseSecret{Name: name, Roles: map[string]contexts.Role{}, Notes: secretsdoc.ConnectionNotes}
	rpc := map[string]string{}
	for _, role := range roles {
		pw, err := secrets.Base62(32)
		if err != nil {
			return databaseSecret{}, nil, err
		}
		out.Roles[role] = contexts.Role{User: name + "_" + role, Password: pw}
		rpc[steps.SecretDatabaseRole(name, role)] = pw
	}
	out.URLs = secretsdoc.DatabaseURLs(cfg.Domain, cfg.TLS.Mode, name, out.Roles)
	return out, rpc, nil
}

// String renders the roles, passwords and URLs for a human.
func (d databaseSecret) String() string {
	var lines []string
	for _, role := range steps.DatabaseRoles {
		if r, ok := d.Roles[role]; ok {
			lines = append(lines, fmt.Sprintf("  %s user: %s", role, r.User), fmt.Sprintf("  %s password: %s", role, r.Password))
		}
	}
	for _, k := range sortedStrings(d.URLs) {
		lines = append(lines, "  "+k+" URL: "+d.URLs[k])
	}
	return strings.Join(lines, "\n")
}

func sortedStrings(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// staleWarning is the warning emitted when the operator generated new
// passwords that are not yet in the secrets document (D7).
func staleWarning(generated bool) []output.Warning {
	if !generated {
		return nil
	}
	return []output.Warning{{
		Code:    "secrets_document_stale",
		Message: "new cluster passwords were generated; run eru secrets export to refresh the secrets document",
	}}
}
