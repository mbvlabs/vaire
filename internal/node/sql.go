package node

import (
	"context"
	"fmt"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/pg"
)

type sqlParams struct {
	DB    string `json:"db"`
	Role  string `json:"role"`
	Query string `json:"query"`
	Write bool   `json:"write"`
}

// handleSQL runs one statement for eru sql --local, where there is no tunnel.
// It runs as postgres (peer) inside one transaction with SET LOCAL ROLE to the
// requested database role and, unless write is set, READ ONLY. migrate acts as
// <db>_owner, matching the role's session default.
func handleSQL(ctx context.Context, env *Env) (Result, error) {
	var p sqlParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if p.DB == "" {
		return Result{}, output.NewError(output.CodeInputRequired, "eru sql needs --db", output.ExitInput,
			"Pass --db with one of the server's databases.").WithDetails(map[string]any{"flag": "--db"})
	}
	if _, ok := env.Cluster.ClusterByName(p.DB); !ok {
		return Result{}, output.NewError(output.CodeValidation, "database "+p.DB+" is not in cluster.toml", output.ExitConfig,
			"Run eru db list to see the databases.")
	}
	role := p.Role
	if role == "" {
		role = "ro"
	}
	user, err := sqlRoleUser(p.DB, role)
	if err != nil {
		return Result{}, err
	}
	rt := runtimeFor(env, false)
	if role == "migrate" {
		user = p.DB + "_owner"
	}
	mode := "READ WRITE"
	if !p.Write {
		mode = "READ ONLY"
	}
	statement := fmt.Sprintf("BEGIN %s;\nSET LOCAL ROLE %s;\n%s\n;\nCOMMIT;\n", mode, pg.Quote(user), p.Query)
	if p.Write {
		if err := rt.PG.Exec(ctx, p.DB, statement, nil); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"command_tag": "OK", "row_count": 0}}, nil
	}
	columns, rows, err := rt.PG.QueryRows(ctx, p.DB, statement, nil)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"columns": columns, "rows": rows, "row_count": len(rows), "command_tag": "SELECT"}}, nil
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
		return "eru_admin", nil
	default:
		return "", output.NewError(output.CodeValidation, "role must be ro, app, migrate or admin", output.ExitConfig, "")
	}
}
