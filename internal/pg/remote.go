package pg

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
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
	rows, err := r.conn.Query(ctx, sql, args...)
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

// Exec runs sql and returns its command tag.
func (r *Remote) Exec(ctx context.Context, sql string, args ...any) (string, error) {
	tag, err := r.conn.Exec(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	return tag.String(), nil
}
