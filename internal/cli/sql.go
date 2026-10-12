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

// dbCredentials returns the user and password for a database role. admin maps
// to the eru_admin role.
func dbCredentials(c *contexts.Context, db, role string) (user, password string, err error) {
	creds, err := contexts.LoadCredentials(c.Name)
	if err != nil {
		return "", "", err
	}
	if creds.Database == nil {
		return "", "", output.NewError(output.CodePrecondition, "this context has no database credentials", output.ExitPrecondition,
			"Re-run eru login with a newer secrets document.")
	}
	if role == "admin" {
		if creds.Database.Admin.Password == "" {
			return "", "", output.NewError(output.CodePrecondition, "this context has no eru_admin password", output.ExitPrecondition,
				"Re-run eru login with a newer secrets document.")
		}
		return creds.Database.Admin.User, creds.Database.Admin.Password, nil
	}
	user, password, ok := creds.Database.DBRole(db, role)
	if !ok {
		return "", "", output.NewError(output.CodePrecondition, "no credentials for "+db+" role "+role, output.ExitPrecondition,
			"Run eru db credentials "+db+" --role "+role+".")
	}
	return user, password, nil
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
	role := stringFlag(cmd, "role")
	if role == "" {
		role = "ro"
	}
	write := flagTrue(cmd, "write")
	if write && role == "ro" {
		return output.NewError(output.CodeUsage, "--write needs a write-capable role", output.ExitUsage,
			"Pass --role app, migrate or admin.")
	}
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
	user, password, err := dbCredentials(s.ctx, db, role)
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
	// One transaction per call; read-only unless --write.
	begin := "BEGIN READ WRITE"
	if !write {
		if _, err := conn.Exec(ctx, "SET default_transaction_read_only = on"); err != nil {
			return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
		}
		begin = "BEGIN READ ONLY"
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
