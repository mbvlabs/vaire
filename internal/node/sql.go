package node

import (
	"context"
	"fmt"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/steps"
)

type sqlParams struct {
	DB    string `json:"db"`
	Role  string `json:"role"`
	Query string `json:"query"`
	Write bool   `json:"write"`
}

// handleSQL runs one statement for vaire sql --local, where there is no tunnel.
// It runs as postgres (peer) inside one transaction with SET LOCAL ROLE to the
// requested database role and, unless write is set, READ ONLY. migrate acts as
// <db>_owner, matching the role's session default. On a protected database
// a write needs a code from the authenticator app (requireWriteCode), and a
// read runs as vaire_admin instead.
func handleSQL(ctx context.Context, env *Env) (Result, error) {
	var p sqlParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if p.DB == "" {
		return Result{}, output.NewError(output.CodeInputRequired, "vaire sql needs --db", output.ExitInput,
			"Pass --db with one of the server's databases.").WithDetails(map[string]any{"flag": "--db"})
	}
	spec, err := clusterOf(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	if p.Write && spec.IsFollower() {
		// A follower takes no writes: a code from the authenticator app can
		// never make it writable (Phase 6). Refuse before any psql run, so a
		// promoted-but-configured follower from an interrupted unfollow never
		// bypasses its protection.
		return Result{}, followerError(spec, "writing to it")
	}
	// Every public SQL path checks the durable activation fence, protected or
	// not, before it opens a connection: a swapped or adopted copy whose
	// safety settlement is incomplete must not be reachable even by a read.
	if err := refuseUnsettledSQL(spec.Name); err != nil {
		return Result{}, err
	}
	local := steps.ClusterPG(spec)
	// A follower's cluster holds its source's database and roles (Phase 6).
	db := spec.DatabaseName()
	role := p.Role
	if role == "" {
		role = "ro"
	}
	user, err := sqlRoleUser(db, role)
	if err != nil {
		return Result{}, err
	}
	if role == "migrate" {
		user = db + "_owner"
	}
	mode := "READ WRITE"
	if !p.Write {
		mode = "READ ONLY"
	}
	statement := fmt.Sprintf("BEGIN %s;\nSET LOCAL ROLE %s;\n%s\n;\nCOMMIT;\n", mode, pg.Quote(user), p.Query)
	if spec.Protected && !p.Write {
		// psql runs every statement in the query, a COMMIT too, and as the
		// superuser what follows would write. On a protected database a
		// read runs as vaire_admin, which cannot write to it.
		local = steps.AdminPG(spec)
		statement = fmt.Sprintf("BEGIN READ ONLY;\n%s\n;\nCOMMIT;\n", p.Query)
	}
	if p.Write {
		if err := local.Exec(ctx, db, statement, nil); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"command_tag": "OK", "row_count": 0}}, nil
	}
	columns, rows, err := local.QueryRows(ctx, db, statement, nil)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"columns": columns, "rows": rows, "row_count": len(rows), "command_tag": "SELECT"}}, nil
}

// refuseUnsettledSQL refuses all SQL on db while a protected unfollow of it
// has not settled the admin access policy. A promoted follower still carries
// the membership and sessions it copied from an unprotected source, so a
// write:false query with its own COMMIT/SET ROLE could write without a code.
// Reads are refused too: the transaction escape needs no write intent. An
// unreadable records file is unknown state and fails closed.
func refuseUnsettledSQL(db string) error {
	recs, err := backup.LoadRecords()
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"the node's activation records could not be read, so no SQL runs on "+db, output.ExitPrecondition,
			"Inspect the node's records file and fix it; vaire never resets it on its own.").
			WithDetails(map[string]any{"reason": "unfollow_state_unknown", "database": db})
	}
	if u, ok := recs.Unfollowing[db]; ok && !u.Settled {
		return output.NewError(output.CodePrecondition,
			db+"'s unfollow has not settled its protection, so no SQL runs on it", output.ExitPrecondition,
			"Finish the unfollow with vaire db unfollow "+db+" --confirm "+db+"/unfollow, then retry.").
			WithDetails(map[string]any{"reason": "unfollow_unsettled", "database": db})
	}
	if f, ok := recs.ActivationFenceFor(db); ok && f.Active() {
		return activationFenceRefusal(f, db)
	}
	return nil
}

// sqlRoleUser maps a --role value to the database user name.
func sqlRoleUser(db, role string) (string, error) {
	switch role {
	case "ro":
		return db + "_ro", nil
	case "app":
		return db + "_app", nil
	case "migrate":
		return db + "_migrate", nil
	case "admin":
		return "vaire_admin", nil
	default:
		return "", output.NewError(output.CodeValidation, "role must be ro, app, migrate or admin", output.ExitConfig, "")
	}
}
