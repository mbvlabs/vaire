package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/spf13/cobra"
)

// remoteConfig fetches cluster.toml from the node and parses it.
func remoteConfig(cmd *cobra.Command, s *session, p *progress) (config.Cluster, configData, error) {
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return config.Cluster{}, configData{}, err
	}
	cfg, err := config.ParseCluster([]byte(data.TOML))
	if err != nil {
		return config.Cluster{}, data, err
	}
	return cfg, data, nil
}

type databaseInfo struct {
	Name       string   `json:"name"`
	InConfig   bool     `json:"in_config"`
	Exists     bool     `json:"exists"`
	SizeBytes  int64    `json:"size_bytes"`
	Extensions []string `json:"extensions"`
	Roles      []string `json:"roles"`
}

func runDBList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := s.node.Call(commandContext(cmd), "db.list", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Databases []databaseInfo `json:"databases"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}
	lines := make([]string, 0, len(data.Databases))
	for _, db := range data.Databases {
		state := "configured"
		if !db.InConfig {
			state = "orphaned"
		} else if !db.Exists {
			state = "pending"
		}
		lines = append(lines, fmt.Sprintf("  %-24s %-10s %s", db.Name, state, strings.Join(db.Extensions, ",")))
	}
	return output.Success(cmd, output.Result{
		Data:     data,
		Message:  strings.Join(lines, "\n"),
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

func runDBCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	if !config.DatabasePattern.MatchString(name) {
		hint := "Use lowercase letters, digits and underscores; the name is also the prefix of its roles."
		if fixed := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.ToLower(name)); config.DatabasePattern.MatchString(fixed) {
			hint = "Try " + fixed + ". Hyphens aren't allowed because the name is also the prefix of its roles."
		}
		return output.NewError(output.CodeValidation, "database name "+name+" must match "+config.DatabasePattern.String(), output.ExitConfig, hint).
			WithDetails(map[string]any{"name": name})
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, _, err := remoteConfig(cmd, s, p)
	if err != nil {
		return err
	}
	creds, err := contexts.LoadCredentials(s.ctx.Name)
	if err != nil {
		return err
	}
	if creds.Database == nil {
		creds.Database = &contexts.DatabaseCreds{}
	}
	timeout := stringFlag(cmd, "statement-timeout")
	if timeout == "" {
		timeout = "60s"
	}
	extensions, _ := cmd.Flags().GetStringSlice("extensions")
	db := config.Database{Name: name, StatementTimeout: timeout, Extensions: extensions}
	if len(extensions) == 0 {
		db.Extensions = []string{"pg_stat_statements"}
	}
	rpc, generated, err := ensureSecrets(cfg, creds.Database, []config.Database{db})
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]any{"database": db}, Message: "dry run: would create database " + name, Warnings: append(s.warnings, staleWarning(generated)...)})
	}
	reply, err := s.node.Call(commandContext(cmd), "db.create", map[string]any{
		"name":              name,
		"statement_timeout": timeout,
		"extensions":        db.Extensions,
	}, remote.CallOptions{Secrets: rpc, OnEvent: p.event})
	if err != nil {
		return err
	}
	if err := contexts.SaveCredentials(s.ctx.Name, creds); err != nil {
		return err
	}
	entry, _ := creds.Database.Database(name)
	message := fmt.Sprintf("created database %s and its roles", name)
	if urls := renderURLs(entry); urls != "" {
		message += "\n" + urls
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"database": entry},
		Message:  message,
		Warnings: append(append(s.warnings, reply.Warnings...), staleWarning(generated)...),
	})
}

func runDBDrop(cmd *cobra.Command, args []string) error {
	name := args[0]
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if !dryRun(cmd) {
		if err := Confirm(cmd, name); err != nil {
			return err
		}
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"database": name}, Message: "dry run: would drop " + name, Warnings: s.warnings})
	}
	reply, err := s.node.Call(commandContext(cmd), "db.drop", map[string]string{"name": name}, remote.CallOptions{Confirm: name, OnEvent: p.event})
	if err != nil {
		return err
	}
	creds, err := contexts.LoadCredentials(s.ctx.Name)
	if err != nil {
		return err
	}
	if creds.Database != nil {
		creds.Database.Databases = removeDatabase(creds.Database.Databases, name)
		if err := contexts.SaveCredentials(s.ctx.Name, creds); err != nil {
			return err
		}
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]string{"dropped": name},
		Message:  "dropped database " + name,
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

func runDBCredentials(cmd *cobra.Command, args []string) error {
	name := args[0]
	rotate := flagTrue(cmd, "rotate")
	role := stringFlag(cmd, "role")
	if role == "" {
		role = steps.RoleApp
	}
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	creds, err := contexts.LoadCredentials(c.Name)
	if err != nil {
		return err
	}
	if creds.Database == nil {
		return output.NewError(output.CodePrecondition, "this context has no database credentials", output.ExitPrecondition,
			"Re-run eru login with a newer secrets document.")
	}
	if !rotate {
		user, password, ok := creds.Database.DBRole(name, role)
		if !ok {
			return output.NewError(output.CodeValidation, "no credentials for "+name+" role "+role, output.ExitConfig,
				"Run eru db credentials "+name+" --role migrate, or rotate with --rotate.")
		}
		entry, _ := creds.Database.Database(name)
		return output.Success(cmd, output.Result{
			Data:    map[string]string{"user": user, "password": password, "urls": strings.Join(urlLines(entry), "\n")},
			Message: renderURLs(entry),
		})
	}
	// Rotate.
	if !dryRun(cmd) {
		if err := Confirm(cmd, name); err != nil {
			return err
		}
	}
	password, err := secrets.Base62(32)
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"database": name, "role": role}, Message: "dry run: would rotate " + name + " role " + role})
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	secretKey := steps.SecretDatabaseRole(name, role)
	if _, err := s.node.Call(commandContext(cmd), "db.credentials", map[string]any{
		"name": name, "role": role, "rotate": true,
	}, remote.CallOptions{Secrets: map[string]string{secretKey: password}, Confirm: name, OnEvent: p.event}); err != nil {
		return err
	}
	setRolePassword(creds.Database, name, role, password)
	if err := contexts.SaveCredentials(c.Name, creds); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]string{"database": name, "role": role, "password": password},
		Message:  fmt.Sprintf("rotated %s role %s; the secrets document is stale, run eru secrets export", name, role),
		Warnings: []output.Warning{{Code: "secrets_document_stale", Message: "run eru secrets export to refresh the secrets document"}},
	})
}

func renderURLs(entry contexts.DBSecret) string {
	lines := urlLines(entry)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func urlLines(entry contexts.DBSecret) []string {
	keys := make([]string, 0, len(entry.URLs))
	for k := range entry.URLs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, "  "+k+": "+entry.URLs[k])
	}
	return lines
}

func removeDatabase(dbs []contexts.DBSecret, name string) []contexts.DBSecret {
	out := dbs[:0]
	for _, db := range dbs {
		if db.Name != name {
			out = append(out, db)
		}
	}
	return out
}

func setRolePassword(creds *contexts.DatabaseCreds, db, role, password string) {
	for i := range creds.Databases {
		if creds.Databases[i].Name != db {
			continue
		}
		if creds.Databases[i].Roles == nil {
			creds.Databases[i].Roles = map[string]contexts.Role{}
		}
		r := creds.Databases[i].Roles[role]
		if r.User == "" {
			r.User = db + "_" + role
		}
		r.Password = password
		creds.Databases[i].Roles[role] = r
	}
}
