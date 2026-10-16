package pg

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eru/internal/transport"
)

// Params describe an operator-side connection over the tunnel (D12). Host is
// the WireGuard address to dial; ServerName is the certificate's domain, used
// for verify-full.
type Params struct {
	Host       string
	Port       int
	ServerName string
	TLSMode    string // verify-full | require
	User       string
	Password   string
	Database   string
	// SimpleProtocol uses the simple query protocol, which PgBouncer's
	// admin console needs (eru top).
	SimpleProtocol bool
	// ApplicationName names the session in pg_stat_activity.
	ApplicationName string
}

// Remote is a pgx connection to PostgreSQL over a transport.Dialer.
type Remote struct {
	conn *pgx.Conn
}

// Result is a query result shaped for the agent contract.
type Result struct {
	Columns    []string    `json:"columns"`
	Rows       [][]*string `json:"rows"`
	CommandTag string      `json:"command_tag"`
	RowCount   int         `json:"row_count"`
}

// Connect dials PostgreSQL through dialer. TLSMode "verify-full" verifies the
// server certificate against system roots using ServerName; "require"
// encrypts without verifying.
func Connect(ctx context.Context, dialer transport.Dialer, p Params) (*Remote, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host = p.Host
	cfg.Port = uint16(p.Port) //nolint:gosec // G115: validated 1-65535.
	cfg.User = p.User
	cfg.Password = p.Password
	cfg.Database = p.Database
	cfg.ConnectTimeout = 15 * time.Second
	cfg.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, fmt.Sprintf("%s:%d", p.Host, p.Port))
	}
	cfg.TLSConfig = tlsConfig(p)
	if p.SimpleProtocol {
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	if p.ApplicationName != "" {
		cfg.RuntimeParams["application_name"] = p.ApplicationName
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Remote{conn: conn}, nil
}

func tlsConfig(p Params) *tls.Config {
	if p.TLSMode == "require" {
		return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // G402: self-signed mode cannot verify.
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.ServerName}
}

// Close closes the connection.
func (r *Remote) Close(ctx context.Context) {
	if r != nil && r.conn != nil {
		_ = r.conn.Close(ctx)
	}
}

// Query runs sql and returns its rows. Non-row statements return an empty row
// set with the command tag set.
func (r *Remote) Query(ctx context.Context, sql string, args ...any) (*Result, error) {
	// Text results for every column: RawValues would otherwise hold pgx's
	// binary encoding (int4 1 is "\x00\x00\x00\x01").
	rows, err := r.conn.Query(ctx, sql, append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := &Result{}
	for _, fd := range rows.FieldDescriptions() {
		res.Columns = append(res.Columns, fd.Name)
	}
	for rows.Next() {
		values := rows.RawValues()
		row := make([]*string, len(values))
		for i, v := range values {
			if v == nil {
				continue
			}
			s := string(v)
			row[i] = &s
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.CommandTag = rows.CommandTag().String()
	res.RowCount = len(res.Rows)
	return res, nil
}

// QueryAll runs sql, which may hold several statements separated by
// semicolons, with the simple query protocol and returns one result per
// statement, in order. The extended protocol Query uses refuses more than one
// statement, and it takes no parameters. Every value comes back as text. On an
// error the statements before the failing one keep their results, and the rest
// do not run.
func (r *Remote) QueryAll(ctx context.Context, sql string) ([]*Result, error) {
	mrr := r.conn.PgConn().Exec(ctx, sql)
	var out []*Result
	for mrr.NextResult() {
		res, err := readResult(mrr.ResultReader())
		if err != nil {
			_ = mrr.Close()
			return out, err
		}
		out = append(out, res)
	}
	return out, mrr.Close()
}

// readResult reads one statement's result. pgconn's own Read leaves the
// columns out when there are no rows, which would hide an empty SELECT's
// header.
func readResult(rr *pgconn.ResultReader) (*Result, error) {
	out := &Result{}
	for _, fd := range rr.FieldDescriptions() {
		out.Columns = append(out.Columns, fd.Name)
	}
	for rr.NextRow() {
		values := rr.Values()
		row := make([]*string, len(values))
		for i, v := range values {
			if v == nil {
				continue
			}
			s := string(v)
			row[i] = &s
		}
		out.Rows = append(out.Rows, row)
	}
	tag, err := rr.Close()
	if err != nil {
		return nil, err
	}
	out.CommandTag = tag.String()
	out.RowCount = len(out.Rows)
	return out, nil
}

// Maps returns the rows as column-name -> value maps; NULL is "".
func (r *Result) Maps() []map[string]string {
	if r == nil {
		return nil
	}
	out := make([]map[string]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		m := make(map[string]string, len(r.Columns))
		for i, c := range r.Columns {
			if i < len(row) && row[i] != nil {
				m[c] = *row[i]
			}
		}
		out = append(out, m)
	}
	return out
}

// Exec runs sql and returns its command tag.
func (r *Remote) Exec(ctx context.Context, sql string, args ...any) (string, error) {
	tag, err := r.conn.Exec(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	return tag.String(), nil
}
