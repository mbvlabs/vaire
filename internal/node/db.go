package node

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/secrets"
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
	// NoFinalDump skips the final long-retention dump db drop takes when
	// backups are configured.
	NoFinalDump bool `json:"no_final_dump"`
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

// listDatabases reads each [[clusters]] entry's database from its own
// cluster. A cluster that is not up lists as not existing.
func listDatabases(ctx context.Context, env *Env) ([]databaseInfo, error) {
	out := make([]databaseInfo, 0, len(env.Cluster.Clusters))
	for _, spec := range env.Cluster.Clusters {
		info := databaseInfo{Name: spec.Name, InConfig: true}
		local := steps.ClusterPG(spec)
		rows, err := local.Query(ctx, "postgres",
			"SELECT pg_database_size(datname) AS size FROM pg_database WHERE datname = "+quoteLiteral(spec.Name), nil)
		if err != nil || len(rows) == 0 {
			out = append(out, info)
			continue
		}
		info.Exists = true
		if _, err := fmt.Sscan(rows[0]["size"], &info.SizeBytes); err != nil {
			info.SizeBytes = 0
		}
		roles, err := existingRolesIn(ctx, local, spec.Name)
		if err == nil {
			info.Roles = roles
		}
		exts, err := local.Query(ctx, spec.Name, "SELECT extname FROM pg_extension ORDER BY extname", nil)
		if err == nil {
			for _, row := range exts {
				info.Extensions = append(info.Extensions, row["extname"])
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
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
	timeout := p.StatementTimeout
	if timeout == "" {
		timeout = "60s"
	}
	extensions := p.Extensions
	if len(extensions) == 0 {
		extensions = []string{"pg_stat_statements"}
	}
	db, err := createDatabase(ctx, env, config.ClusterSpec{Name: p.Name, StatementTimeout: timeout, Extensions: extensions})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"database": db}}, nil
}

// createDatabase adds db to cluster.toml and runs postgres-roles-v1 for it,
// with the role passwords from the request's secrets. db create and db fork
// share it.
func createDatabase(ctx context.Context, env *Env, db config.ClusterSpec) (config.ClusterSpec, error) {
	store := state.FileStore{}
	cfg, previous, err := store.Load()
	if err != nil {
		return db, err
	}
	if _, ok := cfg.ClusterByName(db.Name); ok {
		return db, existingDatabase(db.Name)
	}
	if steps.ClusterExists(ctx, db.Name) {
		return db, existingDatabase(db.Name)
	}
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(db.Name, role)
		if env.Req.Secrets[key] == "" {
			return db, output.NewError(output.CodeInputRequired, "the password for "+db.Name+"_"+role+" was not supplied", output.ExitInput,
				"Generate it on the operator and pass it as "+key+".").WithDetails(map[string]any{"secret": key})
		}
	}
	if err := placeCluster(cfg, &db); err != nil {
		return db, err
	}
	cfg.Clusters = append(cfg.Clusters, db)
	if err := cfg.Validate(); err != nil {
		return db, config.ValidationError(err)
	}
	raw, err := cfg.Encode()
	if err != nil {
		return db, err
	}
	if err := store.Save(raw); err != nil {
		return db, err
	}
	env.Cluster = cfg
	// The new cluster's own step instances: create it, configure it, then
	// its roles and database (RFC 11.2).
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		DB:     db.Name,
		Report: env.Report,
	}); err != nil {
		// Keep cluster.toml in step with the server: an entry whose cluster
		// was never created would make the retry refuse the name. Once the
		// cluster exists, eru apply --db finishes it.
		if !steps.ClusterExists(ctx, db.Name) {
			_ = store.Save(previous)
			return db, err
		}
		return db, withHint(err, "Finish it with eru apply --db "+db.Name+".")
	}
	if err := reloadPoolers(ctx, env, cfg); err != nil {
		return db, withHint(err, "Finish it with eru apply.")
	}
	created, _ := cfg.ClusterByName(db.Name)
	return created, nil
}

// reloadPoolers runs pgbouncer-v1, which renders an entry per database and
// reloads the poolers (RFC 8.1). It skips itself once no database is left.
func reloadPoolers(ctx context.Context, env *Env, cfg config.Cluster) error {
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:   steps.PoolerStepIDs(),
		Report: env.Report,
	})
	return err
}

func existingDatabase(name string) error {
	return output.NewError(output.CodePrecondition, "database "+name+" already exists", output.ExitPrecondition,
		"Pick another name, or drop it first with eru db drop "+name+".").WithDetails(map[string]any{"reason": "database_exists", "database": name})
}

// databaseExists reports whether database name exists in db's cluster.
func databaseExists(ctx context.Context, cfg config.Cluster, db, name string) (bool, error) {
	local, err := clusterPG(cfg, db)
	if err != nil {
		return false, err
	}
	return databaseIn(ctx, local, name)
}

// databaseIn reports whether database name exists in the cluster at local.
func databaseIn(ctx context.Context, local pg.Local, name string) (bool, error) {
	rows, err := local.Query(ctx, "postgres", "SELECT datname FROM pg_database WHERE datname = "+quoteLiteral(name), nil)
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
	spec, inConfig := cfg.ClusterByName(p.Name)
	clusterExists := steps.ClusterExists(ctx, p.Name)
	if !inConfig && !clusterExists {
		return Result{}, output.NewError(output.CodePrecondition, "database "+p.Name+" does not exist", output.ExitPrecondition,
			"Run eru db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": p.Name})
	}

	if err := restoreInProgress(p.Name, "db drop"); err != nil {
		return Result{}, err
	}
	local := steps.ClusterPG(spec)
	exists := false
	if inConfig && clusterExists {
		exists, _ = databaseIn(ctx, local, p.Name)
	}
	var warnings []output.Warning
	var final *backup.Dump
	switch {
	case !exists:
	case !cfg.BackupConfigured():
		warnings = append(warnings, output.Warning{Code: "no_backup_before_drop",
			Message: "backups are not configured, so no final dump of " + p.Name + " was taken"})
	case p.NoFinalDump:
		warnings = append(warnings, output.Warning{Code: "no_backup_before_drop",
			Message: "--no-final-dump: no final dump of " + p.Name + " was taken"})
	default:
		dump, err := dumpToBucket(ctx, cfg, engine.Shell{Secrets: secrets.Default, Log: env.Log}, "db.drop", env.Report, p.Name, true)
		if err != nil {
			return Result{}, output.WrapError(output.CodePrecondition, fmt.Errorf("the final dump of %s failed, so it was not dropped: %w", p.Name, err),
				output.ExitPrecondition, "Fix the bucket, or drop without it with --no-final-dump.").WithDetails(map[string]any{"reason": "final_dump_failed"})
		}
		final = &dump
	}

	// The database's roles live in its cluster, so they go with it.
	var roles []string
	if exists {
		roles, _ = existingRolesIn(ctx, local, p.Name)
	}
	if clusterExists {
		if err := dropCluster(ctx, env, p.Name); err != nil {
			return Result{}, err
		}
	}

	kept := make([]config.ClusterSpec, 0, len(cfg.Clusters))
	for _, db := range cfg.Clusters {
		if db.Name != p.Name {
			kept = append(kept, db)
		}
	}
	cfg.Clusters = kept
	raw, err := cfg.Encode()
	if err != nil {
		return Result{}, err
	}
	if err := store.Save(raw); err != nil {
		return Result{}, err
	}
	env.Cluster = cfg
	if err := reloadPoolers(ctx, env, cfg); err != nil {
		return Result{}, withHint(err, p.Name+" is dropped, but the poolers still route to it. Fix it with eru apply.")
	}
	data := map[string]any{"dropped": p.Name, "roles_dropped": roles}
	if final != nil {
		data["final_dump"] = final
	}
	return Result{Data: data, Warnings: warnings}, nil
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
	return restoreNameRefusal(name)
}

// confirmRequired refuses a destructive request that carries no confirm token.
// The dispatcher has already checked any token that was sent.
func confirmRequired(message, token string) error {
	return output.NewError(output.CodeUnsafeAction, message, output.ExitUnsafe,
		"Re-run with the token in details.confirm_token after a human approves it.").
		WithDetails(map[string]any{"confirm_token": token})
}

// dropCluster stops and deletes 18/<name>, its data directory and its
// cgroup drop-in. db drop then removes its pooler entries; its timers and
// stanza are unit 21's.
func dropCluster(ctx context.Context, env *Env, name string) error {
	sh := engine.Shell{Secrets: secrets.Default, Log: env.Log}
	if err := sh.Exec(ctx, "db.drop", engine.Command{Name: "pg_dropcluster", Args: []string{"18", name, "--stop"}}, env.Report); err != nil {
		return err
	}
	if err := os.RemoveAll(steps.PGServiceDropInDir(name)); err != nil {
		return err
	}
	return sh.Exec(ctx, "db.drop", engine.Command{Name: "systemctl", Args: []string{"daemon-reload"}}, env.Report)
}

// existingRolesIn returns the eru roles of database name that exist in the
// cluster at local.
func existingRolesIn(ctx context.Context, local pg.Local, name string) ([]string, error) {
	wanted := append([]string{name + "_owner"}, prefixedRoles(name)...)
	quoted := make([]string, len(wanted))
	for i, role := range wanted {
		quoted[i] = quoteLiteral(role)
	}
	rows, err := local.Query(ctx, "postgres",
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
	local, err := clusterPG(env.Cluster, p.Name)
	if err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("rotating a password disconnects clients that use the old one", p.Name)
	}
	if err := restoreInProgress(p.Name, "rotating a password"); err != nil {
		return Result{}, err
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
	if err := local.Exec(ctx, "postgres", sql, nil); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"rotated": true, "role": role, "user": p.Name + "_" + role}}, nil
}

// quoteLiteral single-quotes a validated identifier used as a string literal.
func quoteLiteral(value string) string {
	return "'" + value + "'"
}

// placeCluster fills what db create assigns: the node (the master), a free
// port, a new stanza and the default size.
func placeCluster(cfg config.Cluster, db *config.ClusterSpec) error {
	master, ok := cfg.Master()
	if !ok {
		return output.NewError(output.CodeConfig, "cluster.toml has no master node", output.ExitConfig, "")
	}
	db.Node = master.Name
	port, err := cfg.FreeClusterPort()
	if err != nil {
		return output.NewError(output.CodePrecondition, err.Error(), output.ExitPrecondition,
			"Drop a database you no longer need, or put the next one on its own node (Phase 10).").
			WithDetails(map[string]any{"reason": "no_cluster_port"})
	}
	db.Port = port
	if db.Stanza, err = config.NewStanza(db.Name); err != nil {
		return err
	}
	if db.Size == "" {
		db.Size = cfg.Postgres.DefaultSize
	}
	return nil
}
