package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/spf13/cobra"
)

// pgPort is where operator commands connect: the session pooler, which
// routes by the database name (RFC 6.3, 8.1).
const pgPort = 5432

// dbEndpoint is where operator connections to one database go: its node's
// WireGuard address, and the hostname that node's certificate carries.
type dbEndpoint struct {
	WGAddress string
	Hostname  string
}

// databaseEndpoint looks database db up in the context's placement cache
// (RFC 6.3). A database the cache doesn't know refreshes the cache from the
// master once; one still unknown (a restore copy, a typo) goes to the master,
// whose session pooler refuses a database it has no entry for. With db
// empty, it is the master.
func databaseEndpoint(cmd *cobra.Command, s *session, p *progress, db string) dbEndpoint {
	n, ok := s.ctx.DatabaseNode(db)
	if !ok && db != "" {
		s.warnings = append(s.warnings, syncContextFromNode(cmd, s, p)...)
		n, _ = s.ctx.DatabaseNode(db)
	}
	return dbEndpoint{WGAddress: n.WGAddress, Hostname: n.Hostname}
}

// addr is the endpoint's address on port.
func (e dbEndpoint) addr(port int) string {
	return net.JoinHostPort(e.WGAddress, strconv.Itoa(port))
}

// adminCredentials returns the eru_admin user and password. eru keeps no
// application database passwords, so eru sql, psql and console connect as
// eru_admin, read-only unless --write. A write to a protected database
// connects as a short-lived role instead (sessionLogin).
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
// get the owner and its default privileges, like a migration. It uses the
// --name=value form: usql rewrites the URL into a key=value conninfo without
// quoting, so a "-c name=value" value would be cut at the space.
func sessionOptions(db string, write bool) string {
	if write {
		return "--role=" + ownerRole(db)
	}
	return "--default_transaction_read_only=on"
}

// ownerRole is db's owner role. A restore's or drill's copy
// (<db>__restoring) holds the live database's roles, so it is <db>_owner.
func ownerRole(db string) string { return backup.LiveDatabase(db) + "_owner" }

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
			"Pass --db with one of the server's databases.").WithDetails(map[string]any{"flag": "--db"})
	}
	user, password, err := sessionLogin(cmd, s, p, db, write)
	if err != nil {
		return err
	}
	ctx := commandContext(cmd)
	at := databaseEndpoint(cmd, s, p, db)
	// Connecting through the tunnel and running the statement send no
	// events; the waiting line shows them. It ends before the result prints.
	stopWait := p.wait("connecting to " + db)
	defer stopWait()
	conn, err := pg.Connect(ctx, s.dialer, pg.Params{
		Host: at.WGAddress, Port: pgPort, ServerName: at.Hostname, TLSMode: s.ctx.TLSMode,
		User: user, Password: password, Database: db,
	})
	if err != nil {
		return output.WrapError(output.CodeRemoteUnreachable, fmt.Errorf("connect to PostgreSQL: %w", err), output.ExitTransport,
			"Check that the database exists (eru db list), that the session pooler runs (eru logs pgbouncer-session), and that the credentials are current.")
	}
	defer conn.Close(ctx)
	p.setPhase("running the query on " + db)
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
		setup = "SET ROLE " + pg.Quote(ownerRole(db))
	}
	if _, err := conn.Exec(ctx, setup); err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	if _, err := conn.Exec(ctx, begin); err != nil {
		if rerr := readOnlyCopyError(db, err); rerr != nil {
			return rerr
		}
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	res, err := conn.Query(ctx, query)
	if err != nil {
		_, _ = conn.Exec(ctx, "ROLLBACK")
		if rerr := readOnlyCopyError(db, err); rerr != nil {
			return rerr
		}
		hint := "Fix the statement and retry."
		if write && backup.LiveDatabase(db) != db {
			hint += " " + db + " is a copy: one restored from a backup is read-only (in recovery) until eru restore promote " + backup.LiveDatabase(db) + "; one restored from a dump takes writes."
		}
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, hint)
	}
	if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "The transaction did not commit.")
	}
	stopWait()
	return output.Success(cmd, output.Result{Data: res, Message: renderTable(res), Warnings: warnings})
}

// sqlstateRecovery is feature_not_supported, which PostgreSQL returns for
// a read-write transaction on a cluster in recovery.
const sqlstateRecovery = "0A000"

// readOnlyCopyError is err as a precondition error when db is a restore's
// copy (<db>__restoring) that refused a write because it is still in
// recovery: a copy restored from a backup is read-only until promoted. It is
// nil for any other error or database.
func readOnlyCopyError(db string, err error) error {
	live := backup.LiveDatabase(db)
	if live == db || backup.RestoringName(live) != db {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateRecovery {
		if !strings.Contains(err.Error(), "during recovery") {
			return nil
		}
	}
	return output.WrapError(output.CodePrecondition, fmt.Errorf("%s is read-only: it was restored from a backup and stays in recovery until promoted: %w", db, err),
		output.ExitPrecondition, "Inspect it read-only (eru sql --db "+db+" without --write), or promote it with eru restore promote "+live+" --confirm "+live+"/restore and write to "+live+".").
		WithDetails(map[string]any{"reason": "restore_copy_read_only", "sqlstate": sqlstateRecovery})
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
