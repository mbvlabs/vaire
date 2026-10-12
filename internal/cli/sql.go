package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/spf13/cobra"
)

// pgPort is where operator commands connect to PostgreSQL (D12).
const pgPort = 5432

// nodeWGAddr returns the primary node's WireGuard address.
func nodeWGAddr(s *session) string {
	if n, ok := s.ctx.Primary(); ok {
		return n.WGAddress
	}
	return ""
}

// adminCredentials returns the eru_admin user and password. eru keeps no
// application database passwords, so eru sql, psql and console connect as
// eru_admin, read-only unless --write.
func adminCredentials(c *contexts.Context) (user, password string, err error) {
	creds, err := contexts.LoadCredentials(c.Name)
	if err != nil {
		return "", "", err
	}
	if creds.Database == nil || creds.Database.Admin.Password == "" {
		return "", "", output.NewError(output.CodePrecondition, "this context has no eru_admin password", output.ExitPrecondition,
			"Re-run eru login with a newer secrets document.")
	}
	return creds.Database.Admin.User, creds.Database.Admin.Password, nil
}

// sessionOptions is the libpq options value for an eru_admin session on db:
// read-only, or with write acting as the database's owner role so new objects
// get the owner and its default privileges, like a migration.
func sessionOptions(db string, write bool) string {
	if write {
		return "-c role=" + db + "_owner"
	}
	return "-c default_transaction_read_only=on"
}

// sqlQuery resolves the query from the argument, --file, or stdin.
func sqlQuery(cmd *cobra.Command, args []string) (string, error) {
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	if path := stringFlag(cmd, "file"); path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this file.
		if err != nil {
			return "", output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		return string(raw), nil
	}
	if !stdinIsTTY() {
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
		if err != nil {
			return "", output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		return string(raw), nil
	}
	return "", nil
}

func runSQL(cmd *cobra.Command, args []string) error {
	db := stringFlag(cmd, "db")
	write := flagTrue(cmd, "write")
	query, err := sqlQuery(cmd, args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(query) == "" {
		return output.NewError(output.CodeUsage, "no query was given", output.ExitUsage, "Pass the SQL as an argument, or use --file.")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if db == "" {
		return output.NewError(output.CodeInputRequired, "eru sql needs --db", output.ExitInput,
			"Pass --db with one of the cluster's databases.").WithDetails(map[string]any{"flag": "--db"})
	}
	user, password, err := adminCredentials(s.ctx)
	if err != nil {
		return err
	}
	ctx := commandContext(cmd)
	conn, err := pg.Connect(ctx, s.tunnel, pg.Params{
		Host: nodeWGAddr(s), Port: pgPort, ServerName: s.ctx.Domain, TLSMode: s.ctx.TLSMode,
		User: user, Password: password, Database: db,
	})
	if err != nil {
		return output.WrapError(output.CodeRemoteUnreachable, fmt.Errorf("connect to PostgreSQL: %w", err), output.ExitTransport,
			"Check that PostgreSQL is running and the credentials are current.")
	}
	defer conn.Close(ctx)
	warnings := s.warnings
	if s.ctx.TLSMode == "self-signed" {
		warnings = append(warnings, output.Warning{Code: "tls_unverified", Message: "the cluster uses a self-signed certificate; the connection is encrypted but not verified"})
	}
	// One transaction per call; read-only unless --write, which acts as the
	// database's owner role (see sessionOptions).
	begin := "BEGIN READ ONLY"
	setup := "SET default_transaction_read_only = on"
	if write {
		begin = "BEGIN READ WRITE"
		setup = "SET ROLE " + pg.Quote(db+"_owner")
	}
	if _, err := conn.Exec(ctx, setup); err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	if _, err := conn.Exec(ctx, begin); err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	res, err := conn.Query(ctx, query)
	if err != nil {
		_, _ = conn.Exec(ctx, "ROLLBACK")
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "Fix the statement and retry.")
	}
	if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "The transaction did not commit.")
	}
	return output.Success(cmd, output.Result{Data: res, Message: renderTable(res), Warnings: warnings})
}

// renderTable renders a result as a simple text table.
func renderTable(res *pg.Result) string {
	if res == nil || len(res.Columns) == 0 {
		if res != nil && res.CommandTag != "" {
			return res.CommandTag
		}
		return "no rows"
	}
	widths := make([]int, len(res.Columns))
	for i, c := range res.Columns {
		widths[i] = len(c)
	}
	cells := make([][]string, len(res.Rows))
	for r, row := range res.Rows {
		cells[r] = make([]string, len(res.Columns))
		for i := range res.Columns {
			v := ""
			if i < len(row) && row[i] != nil {
				v = *row[i]
			}
			cells[r][i] = v
			if len(v) > widths[i] {
				widths[i] = len(v)
			}
		}
	}
	var b strings.Builder
	for i, c := range res.Columns {
		fmt.Fprintf(&b, "%-*s  ", widths[i], c)
	}
	b.WriteString("\n")
	for _, row := range cells {
		for i, v := range row {
			fmt.Fprintf(&b, "%-*s  ", widths[i], v)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "(%d rows)", len(res.Rows))
	return b.String()
}
