package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/steps"
)

// Writing to a protected database. eru sql, psql and console connect as
// eru_admin, whose password every context holds, so a read-only session
// option alone would stop nobody: on a protected database eru_admin is not a
// member of <db>_owner at all (steps.AdminAccessSQL) and can only read. A
// write takes a short-lived login role in <db>_owner that db.write-access
// creates once a current code from the authenticator app checks out. The
// CLI asks for the code once, when the command starts; the role can log in
// for writeRoleLogin, so a code never unlocks a later command.
//
// The CLI makes the role's password and sends it as the request's
// write_password secret, as it does for every password (D5); the result
// carries none. The node redacts every registered secret from every event
// it writes, its result included, so a password the node made and returned
// would reach the CLI as the redaction placeholder.

// writeRolePrefix names the short-lived write roles.
const writeRolePrefix = "eru_write_"

// WritePasswordSecret is the request secret that carries the write role's
// password, made by the CLI.
const WritePasswordSecret = "write_password"

// writeRolePasswordMin is the shortest write_password db.write-access
// takes: the CLI sends 32 base62 characters.
const writeRolePasswordMin = 32

// writeRoleLogin is how long a write role can log in. A session it opened
// lasts as long as the client keeps it.
const writeRoleLogin = 2 * time.Minute

// WriteAccess is the db.write-access result. Protected false means the
// database is not protected: the CLI writes as eru_admin, as before. The
// role's password is the request's write_password secret.
type WriteAccess struct {
	Protected  bool   `json:"protected"`
	User       string `json:"user,omitempty"`
	ValidUntil string `json:"valid_until,omitempty"`
}

type writeAccessParams struct {
	DB string `json:"db"`
}

// writeRole is one short-lived write role.
type writeRole struct {
	User       string
	Password   string
	ValidUntil time.Time
}

// grantWriteFunc creates role in spec's cluster. Tests replace it.
type grantWriteFunc func(ctx context.Context, spec config.ClusterSpec, role writeRole) error

// handleWriteAccess hands out a write role for a protected database after
// checking the request's protect_code. The role logs in with the request's
// write_password secret.
func handleWriteAccess(ctx context.Context, env *Env) (Result, error) {
	return defaultProtectGuard.writeAccess(ctx, env, createWriteRole)
}

func (g protectGuard) writeAccess(ctx context.Context, env *Env, grant grantWriteFunc) (Result, error) {
	var p writeAccessParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if p.DB == "" {
		return Result{}, output.NewError(output.CodeInputRequired, "db.write-access needs db", output.ExitInput,
			"Pass --db with one of the server's databases.").WithDetails(map[string]any{"flag": "--db"})
	}
	spec, ok := env.Cluster.ClusterByName(p.DB)
	if !ok || !spec.Protected {
		// A copy (a restore's or a fork's) is never protected itself.
		return Result{Data: WriteAccess{}}, nil
	}
	if err := g.checkWrite(spec.Name, "writing to it", env.Req.Secrets[ProtectCodeSecret]); err != nil {
		return Result{}, err
	}
	password := env.Req.Secrets[WritePasswordSecret]
	if len(password) < writeRolePasswordMin {
		return Result{}, output.NewError(output.CodeInputRequired,
			"db.write-access needs the write role's password in the write_password secret", output.ExitInput,
			"Run the write with an eru CLI of the node's version: it sends the password.").
			WithDetails(map[string]any{"secret": WritePasswordSecret})
	}
	role, err := newWriteRole(g.now(), password)
	if err != nil {
		return Result{}, err
	}
	if err := grant(ctx, spec, role); err != nil {
		return Result{}, err
	}
	return Result{Data: WriteAccess{Protected: true, User: role.User,
		ValidUntil: role.ValidUntil.UTC().Format(time.RFC3339)}}, nil
}

// checkWrite verifies the protect_code a write to protected database name
// needs. action names the write in the refusal when no secret is set up.
func (g protectGuard) checkWrite(name, action, code string) error {
	ok, err := g.configured()
	if err != nil {
		return err
	}
	if !ok {
		return protectNotInitialized(output.CodeDatabaseProtected,
			"database "+name+" is protected and "+action+" needs two-factor protection, which this node does not have set up", output.ExitUnsafe)
	}
	if strings.TrimSpace(code) == "" {
		return output.NewError(output.CodeDatabaseProtected,
			"database "+name+" is protected, so "+action+" needs a code from the authenticator app", output.ExitUnsafe,
			"A person runs the command at an interactive terminal, without --yes or structured output, and types the current 6-digit code from the authenticator app set up with eru protect init. Reads need no code.").
			WithDetails(map[string]any{"reason": "write_requires_human", "database": name})
	}
	_, err = g.verify(code, false)
	return err
}

// requireWriteCode wraps a method handler so that, when the database it
// names (params[field]) is protected and applies says the request writes,
// it runs only with a current code from the authenticator app in the
// request's protect_code secret. A nil applies covers every request that is
// not a dry run.
func (g protectGuard) requireWriteCode(action, field string, applies func(rpc.Request) bool,
	handler func(context.Context, *Env) (Result, error)) func(context.Context, *Env) (Result, error) {
	return func(ctx context.Context, env *Env) (Result, error) {
		writes := !env.Req.DryRun
		if applies != nil {
			writes = applies(env.Req)
		}
		if writes {
			var params map[string]any
			_ = json.Unmarshal(env.Req.Params, &params)
			if name, _ := params[field].(string); name != "" {
				if spec, ok := env.Cluster.ClusterByName(name); ok && spec.Protected {
					if err := g.checkWrite(name, action, env.Req.Secrets[ProtectCodeSecret]); err != nil {
						return Result{}, err
					}
				}
			}
		}
		return handler(ctx, env)
	}
}

// sqlWrites reports whether a sql request writes.
func sqlWrites(req rpc.Request) bool {
	var p struct {
		Write bool `json:"write"`
	}
	return json.Unmarshal(req.Params, &p) == nil && p.Write
}

// newWriteRole makes a write role's name and expiry for password.
func newWriteRole(now time.Time, password string) (writeRole, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return writeRole{}, err
	}
	return writeRole{User: writeRolePrefix + hex.EncodeToString(suffix), Password: password, ValidUntil: now.Add(writeRoleLogin)}, nil
}

// createWriteRole drops the write roles that expired and have no session
// left, then creates role in spec's cluster: a login role that inherits
// nothing and may SET ROLE to <db>_owner, as eru sql, psql and console do,
// so whatever it creates belongs to the owner.
func createWriteRole(ctx context.Context, spec config.ClusterSpec, role writeRole) error {
	local := steps.ClusterPG(spec)
	dropExpiredWriteRoles(ctx, local, spec)
	verifier, err := pg.SCRAMVerifier(role.Password)
	if err != nil {
		return err
	}
	if err := local.Exec(ctx, steps.AuthDB, writeRoleSQL(spec, role, verifier), nil); err != nil {
		return output.WrapError(output.CodeExternalCommand, fmt.Errorf("create the write role: %w", err), output.ExitExternal, "")
	}
	return nil
}

// writeRoleSQL creates role, run in steps.AuthDB, where the session
// pooler's auth_query looks it up.
func writeRoleSQL(spec config.ClusterSpec, role writeRole, verifier string) string {
	user := pg.Quote(role.User)
	return fmt.Sprintf("CREATE ROLE %s LOGIN NOINHERIT PASSWORD '%s' VALID UNTIL '%s' IN ROLE %s;\nGRANT CONNECT, TEMPORARY ON DATABASE %s TO %s;\n",
		user, verifier, role.ValidUntil.UTC().Format(time.RFC3339), pg.Quote(spec.Name+"_owner"), pg.Quote(spec.Name), user)
}

// expiredWriteRolesSQL lists the write roles past their VALID UNTIL with
// no session left.
const expiredWriteRolesSQL = `SELECT rolname FROM pg_roles
  WHERE rolname LIKE 'eru\_write\_%' AND rolvaliduntil < now()
    AND rolname NOT IN (SELECT usename FROM pg_stat_activity WHERE usename IS NOT NULL)`

// dropExpiredWriteRoles drops expired write roles, giving whatever one
// created to the owner. It is best effort: a role left over cannot log in.
func dropExpiredWriteRoles(ctx context.Context, local pg.Local, spec config.ClusterSpec) {
	rows, err := local.Query(ctx, "postgres", expiredWriteRolesSQL, nil)
	if err != nil {
		return
	}
	owner := pg.Quote(spec.Name + "_owner")
	for _, row := range rows {
		name := row["rolname"]
		if !strings.HasPrefix(name, writeRolePrefix) {
			continue
		}
		role := pg.Quote(name)
		_ = local.Exec(ctx, spec.Name, fmt.Sprintf("REASSIGN OWNED BY %s TO %s;\nDROP OWNED BY %s;\n", role, owner, role), nil)
		_ = local.Exec(ctx, "postgres", "DROP ROLE IF EXISTS "+role+";\n", nil)
	}
}

// protectionChanged lists the databases in both old and updated whose
// protection differs.
func protectionChanged(old, updated config.Cluster) []string {
	var out []string
	for _, c := range updated.Clusters {
		if o, ok := old.ClusterByName(c.Name); ok && o.Protected != c.Protected {
			out = append(out, c.Name)
		}
	}
	return out
}

// syncProtectedAccess runs the roles step of each database whose
// protection changes, so eru_admin's write access follows the flag at once:
// protecting one takes it away, lifting the protection gives it back. A
// database that awaits recovery is left to db recover, which builds its
// roles afresh.
func syncProtectedAccess(ctx context.Context, env *Env, old, updated config.Cluster) ([]output.Warning, error) {
	changed := protectionChanged(old, updated)
	if len(changed) == 0 {
		return nil, nil
	}
	recs, _ := backup.LoadRecords()
	rt := runtimeFor(&Env{Req: env.Req, Cluster: updated, Log: env.Log, Report: env.Report}, false)
	var warnings []output.Warning
	for _, name := range changed {
		if recs.IsAwaiting(name) {
			continue
		}
		_, w, err := engine.Apply(ctx, steps.Catalog(), updated, rt, engine.Options{
			Only: []string{steps.RolesStepID(name)}, DB: name, Restarter: steps.Restarter(updated), Report: env.Report,
		})
		if err != nil {
			return warnings, withHint(err, "The protection of "+name+" was not changed: eru_admin's access to it could not be set. Check that its cluster runs (eru status) and retry.")
		}
		warnings = append(warnings, w...)
	}
	return warnings, nil
}
