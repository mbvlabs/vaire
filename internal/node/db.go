package node

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/state"
	"github.com/mbvlabs/eru/internal/steps"
)

// databaseInfo is one entry in db.list.
type databaseInfo struct {
	Name       string   `json:"name"`
	InConfig   bool     `json:"in_config"`
	Exists     bool     `json:"exists"`
	SizeBytes  int64    `json:"size_bytes"`
	Extensions []string `json:"extensions"`
	Roles      []string `json:"roles"`
}

type dbCreateParams struct {
	Name             string   `json:"name"`
	StatementTimeout string   `json:"statement_timeout"`
	Extensions       []string `json:"extensions"`
}

type dbDropParams struct {
	Name string `json:"name"`
}

type dbCredentialsParams struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Rotate bool   `json:"rotate"`
}

func handleDBList(ctx context.Context, env *Env) (Result, error) {
	databases, err := listDatabases(ctx, env)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"databases": databases}}, nil
}

// listDatabases merges cluster.toml with the live databases.
func listDatabases(ctx context.Context, env *Env) ([]databaseInfo, error) {
	rows, err := runtimeFor(env, true).PG.Query(ctx, "postgres",
		"SELECT datname, pg_database_size(datname) FROM pg_database WHERE NOT datistemplate ORDER BY datname", nil)
	if err != nil {
		return nil, err
	}
	live := map[string]int64{}
	for _, row := range rows {
		var size int64
		if _, err := fmt.Sscan(row["pg_database_size"], &size); err != nil {
			size = 0
		}
		live[row["datname"]] = size
	}
	roleRows, err := runtimeFor(env, true).PG.Query(ctx, "postgres", "SELECT rolname FROM pg_roles", nil)
	if err != nil {
		return nil, err
	}
	roleSet := map[string]bool{}
	for _, row := range roleRows {
		roleSet[row["rolname"]] = true
	}

	names := map[string]bool{}
	configured := map[string]config.Database{}
	for _, db := range env.Cluster.Databases {
		names[db.Name] = true
		configured[db.Name] = db
	}
	for name := range live {
		if name == "postgres" {
			continue
		}
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	out := make([]databaseInfo, 0, len(sorted))
	for _, name := range sorted {
		info := databaseInfo{Name: name, InConfig: false, Exists: false}
		if _, ok := configured[name]; ok {
			info.InConfig = true
		}
		if size, ok := live[name]; ok {
			info.Exists = true
			info.SizeBytes = size
		}
		for _, role := range append([]string{"owner"}, steps.DatabaseRoles...) {
			if roleSet[name+"_"+role] {
				info.Roles = append(info.Roles, name+"_"+role)
			}
		}
		if info.Exists {
			exts, err := runtimeFor(env, true).PG.Query(ctx, name, "SELECT extname FROM pg_extension ORDER BY extname", nil)
			if err == nil {
				for _, row := range exts {
					info.Extensions = append(info.Extensions, row["extname"])
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func handleDBCreate(ctx context.Context, env *Env) (Result, error) {
	var p dbCreateParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := checkDatabaseName(p.Name); err != nil {
		return Result{}, err
	}
	store := state.FileStore{}
	cfg, previous, err := store.Load()
	if err != nil {
		return Result{}, err
	}
	if _, ok := cfg.DatabaseByName(p.Name); ok {
		return Result{}, existingDatabase(p.Name)
	}
	if exists, err := databaseExists(ctx, env, p.Name); err == nil && exists {
		return Result{}, existingDatabase(p.Name)
	}
	timeout := p.StatementTimeout
	if timeout == "" {
		timeout = "60s"
	}
	extensions := p.Extensions
	if len(extensions) == 0 {
		extensions = []string{"pg_stat_statements"}
	}
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(p.Name, role)
		if env.Req.Secrets[key] == "" {
			return Result{}, output.NewError(output.CodeInputRequired, "the password for "+p.Name+"_"+role+" was not supplied", output.ExitInput,
				"Generate it on the operator and pass it as "+key+".").WithDetails(map[string]any{"secret": key})
		}
	}
	cfg.Databases = append(cfg.Databases, config.Database{Name: p.Name, StatementTimeout: timeout, Extensions: extensions})
	if err := cfg.Validate(); err != nil {
		return Result{}, config.ValidationError(err)
	}
	raw, err := cfg.Encode()
	if err != nil {
		return Result{}, err
	}
	if err := store.Save(raw); err != nil {
		return Result{}, err
	}
	env.Cluster = cfg
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:   steps.RolesStepIDs(),
		Report: env.Report,
	}); err != nil {
		// Keep cluster.toml in step with the server: an entry whose database
		// was never created would make the retry refuse the name.
		if exists, existsErr := databaseExists(ctx, env, p.Name); existsErr == nil && !exists {
			_ = store.Save(previous)
		}
		return Result{}, err
	}
	db, _ := cfg.DatabaseByName(p.Name)
	return Result{Data: map[string]any{"database": db}}, nil
}

func existingDatabase(name string) error {
	return output.NewError(output.CodePrecondition, "database "+name+" already exists", output.ExitPrecondition,
		"Pick another name, or drop it first with eru db drop "+name+".").WithDetails(map[string]any{"reason": "database_exists", "database": name})
}

func databaseExists(ctx context.Context, env *Env, name string) (bool, error) {
	rows, err := runtimeFor(env, true).PG.Query(ctx, "postgres", "SELECT datname FROM pg_database WHERE datname = "+quoteLiteral(name), nil)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

func handleDBDrop(ctx context.Context, env *Env) (Result, error) {
	var p dbDropParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := checkDatabaseName(p.Name); err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("dropping "+p.Name+" deletes all of its data", p.Name)
	}
	store := state.FileStore{}
	cfg, _, err := store.Load()
	if err != nil {
		return Result{}, err
	}
	_, inConfig := cfg.DatabaseByName(p.Name)
	exists, err := databaseExists(ctx, env, p.Name)
	if err != nil {
		return Result{}, err
	}
	if !inConfig && !exists {
		return Result{}, output.NewError(output.CodePrecondition, "database "+p.Name+" does not exist", output.ExitPrecondition,
			"Run eru db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": p.Name})
	}

	rt := runtimeFor(env, false)
	if err := rt.PG.Exec(ctx, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE);\n", pg.Quote(p.Name)), nil); err != nil {
		return Result{}, err
	}
	// Objects and grants the roles hold elsewhere (the postgres database, shared
	// objects) would block DROP ROLE; hand them to postgres and drop the rest.
	roles, err := existingRoles(ctx, env, p.Name)
	if err != nil {
		return Result{}, err
	}
	if len(roles) > 0 {
		quoted := make([]string, len(roles))
		for i, role := range roles {
			quoted[i] = pg.Quote(role)
		}
		list := strings.Join(quoted, ", ")
		drop := fmt.Sprintf("REASSIGN OWNED BY %s TO postgres;\nDROP OWNED BY %s;\n", list, list)
		for _, role := range quoted {
			drop += fmt.Sprintf("DROP ROLE IF EXISTS %s;\n", role)
		}
		if err := rt.PG.Exec(ctx, "postgres", drop, nil); err != nil {
			return Result{}, err
		}
	}

	kept := make([]config.Database, 0, len(cfg.Databases))
	for _, db := range cfg.Databases {
		if db.Name != p.Name {
			kept = append(kept, db)
		}
	}
	cfg.Databases = kept
	raw, err := cfg.Encode()
	if err != nil {
		return Result{}, err
	}
	if err := store.Save(raw); err != nil {
		return Result{}, err
	}
	return Result{
		Data: map[string]any{"dropped": p.Name, "roles_dropped": roles},
		Warnings: []output.Warning{{
			Code:    "no_backup_before_drop",
			Message: "no backup was taken before dropping " + p.Name + "; pgBackRest arrives in Phase 3",
		}},
	}, nil
}

// checkDatabaseName refuses names outside the pattern and the system databases.
func checkDatabaseName(name string) error {
	if !config.DatabasePattern.MatchString(name) {
		return output.NewError(output.CodeValidation, "database name "+name+" must match "+config.DatabasePattern.String(), output.ExitConfig, "")
	}
	switch name {
	case "postgres", "template0", "template1":
		return output.NewError(output.CodeValidation, "database "+name+" is a system database eru does not manage", output.ExitConfig, "")
	}
	return nil
}

// confirmRequired refuses a destructive request that carries no confirm token.
// The dispatcher has already checked any token that was sent.
func confirmRequired(message, token string) error {
	return output.NewError(output.CodeUnsafeAction, message, output.ExitUnsafe,
		"Re-run with the token in details.confirm_token after a human approves it.").
		WithDetails(map[string]any{"confirm_token": token})
}

// existingRoles returns the eru roles of database name that exist.
func existingRoles(ctx context.Context, env *Env, name string) ([]string, error) {
	wanted := append([]string{name + "_owner"}, prefixedRoles(name)...)
	quoted := make([]string, len(wanted))
	for i, role := range wanted {
		quoted[i] = quoteLiteral(role)
	}
	rows, err := runtimeFor(env, true).PG.Query(ctx, "postgres",
		"SELECT rolname FROM pg_roles WHERE rolname IN ("+strings.Join(quoted, ", ")+") ORDER BY rolname", nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row["rolname"])
	}
	return out, nil
}

func prefixedRoles(name string) []string {
	out := make([]string, len(steps.DatabaseRoles))
	for i, role := range steps.DatabaseRoles {
		out[i] = name + "_" + role
	}
	return out
}

func handleDBCredentials(ctx context.Context, env *Env) (Result, error) {
	var p dbCredentialsParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if !p.Rotate {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"credentials live on the operator, not the node",
			output.ExitPrecondition,
			"Run eru db credentials "+p.Name+" on your machine, or pass --rotate to set a new password.",
		).WithDetails(map[string]any{"reason": "operator_only", "database": p.Name})
	}
	if err := checkDatabaseName(p.Name); err != nil {
		return Result{}, err
	}
	if _, ok := env.Cluster.DatabaseByName(p.Name); !ok {
		return Result{}, output.NewError(output.CodePrecondition, "database "+p.Name+" is not in cluster.toml", output.ExitPrecondition,
			"Run eru db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": p.Name})
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("rotating a password disconnects clients that use the old one", p.Name)
	}
	role := p.Role
	if role == "" {
		role = steps.RoleApp
	}
	switch role {
	case steps.RoleMigrate, steps.RoleApp, steps.RoleRO:
	default:
		return Result{}, output.NewError(output.CodeValidation, "role must be migrate, app or ro", output.ExitConfig, "")
	}
	secretKey := steps.SecretDatabaseRole(p.Name, role)
	password := env.Req.Secrets[secretKey]
	if password == "" {
		return Result{}, output.NewError(output.CodeInputRequired, "the new password was not supplied", output.ExitInput,
			"Generate it on the operator and pass it as "+secretKey+".").WithDetails(map[string]any{"secret": secretKey})
	}
	verifier, err := pg.SCRAMVerifier(password)
	if err != nil {
		return Result{}, err
	}
	sql := fmt.Sprintf("ALTER ROLE %s PASSWORD '%s';\n", pg.Quote(p.Name+"_"+role), verifier)
	if err := runtimeFor(env, false).PG.Exec(ctx, "postgres", sql, nil); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"rotated": true, "role": role, "user": p.Name + "_" + role}}, nil
}

// quoteLiteral single-quotes a validated identifier used as a string literal.
func quoteLiteral(value string) string {
	return "'" + value + "'"
}
