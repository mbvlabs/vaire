package cli

import (
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/secretsdoc"
	"github.com/mbvlabs/eru/internal/steps"
)

// ensureSecrets fills the operator's database credentials for the given
// databases plus the admin and service roles, generating any password it does
// not have (D7). It returns the RPC secret map and whether anything was
// generated.
func ensureSecrets(cfg config.Cluster, creds *contexts.DatabaseCreds, dbs []config.Database) (map[string]string, bool, error) {
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

	idx := map[string]int{}
	for i := range creds.Databases {
		idx[creds.Databases[i].Name] = i
	}
	for _, db := range dbs {
		i, ok := idx[db.Name]
		if !ok {
			creds.Databases = append(creds.Databases, contexts.DBSecret{Name: db.Name, Roles: map[string]contexts.Role{}})
			i = len(creds.Databases) - 1
			idx[db.Name] = i
		}
		entry := &creds.Databases[i]
		if entry.Roles == nil {
			entry.Roles = map[string]contexts.Role{}
		}
		for _, role := range steps.DatabaseRoles {
			r := entry.Roles[role]
			if r.User == "" {
				r.User = db.Name + "_" + role
			}
			if r.Password == "" {
				pw, err := secrets.Base62(32)
				if err != nil {
					return nil, false, err
				}
				r.Password = pw
				generated = true
			}
			entry.Roles[role] = r
			rpc[steps.SecretDatabaseRole(db.Name, role)] = r.Password
		}
		entry.URLs = secretsdoc.DatabaseURLs(cfg.Domain, cfg.TLS.Mode, db.Name, entry.Roles)
	}

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

// staleWarning is the warning emitted when the operator generated new
// passwords that are not yet in the secrets document (D7).
func staleWarning(generated bool) []output.Warning {
	if !generated {
		return nil
	}
	return []output.Warning{{
		Code:    "secrets_document_stale",
		Message: "new database passwords were generated; run eru secrets export to refresh the secrets document",
	}}
}
