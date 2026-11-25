package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/localenv"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/spf13/cobra"
)

func workstationSourceSpec(input copySourceInput) (backup.SourceSpec, error) {
	if _, err := input.secrets(); err != nil {
		return backup.SourceSpec{}, err
	}

	var spec backup.SourceSpec

	if input.URL != "" {
		src, err := backup.ParseSourceURL(input.URL)
		if err != nil {
			return spec, err
		}

		u, err := url.Parse(strings.TrimRight(input.URL, "\r\n"))
		if err != nil {
			return spec, errors.New("invalid source URL")
		}

		pw, _ := u.User.Password()
		spec = backup.SourceSpec{Host: src.Host, Port: src.Port, Database: src.Database, User: src.User, Password: pw, TLSMode: src.TLSMode}
	} else {
		f := input.Fields
		spec = backup.SourceSpec{Host: f.Host, Port: f.Port, Database: f.Database, User: f.User, Password: f.Password, TLSMode: f.TLSMode, CACert: []byte(f.CACertPEM)}
	}

	if input.CA != "" {
		if len(spec.CACert) != 0 {
			return backup.SourceSpec{}, errors.New("the source CA was supplied twice")
		}

		spec.CACert = []byte(input.CA)
	}

	src, err := backup.NewSource(spec)
	if err != nil {
		return backup.SourceSpec{}, err
	}

	spec.Port, spec.TLSMode = src.Port, src.TLSMode

	return spec, nil
}

func workstationSourceInventory(ctx context.Context, spec backup.SourceSpec) ([]backup.SourceExtension, error) {
	extensions, _, err := workstationSourceInventoryAndSize(ctx, spec)
	return extensions, err
}

func workstationSourceInventoryAndSize(ctx context.Context, spec backup.SourceSpec) ([]backup.SourceExtension, int64, error) {
	if spec.Port < 1 || spec.Port > 65535 {
		return nil, 0, errors.New("invalid source port")
	}

	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, 0, errors.New("could not configure source connection")
	}

	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = spec.Host, uint16(spec.Port), spec.User, spec.Password, spec.Database
	cfg.ConnectTimeout = 15 * time.Second
	cfg.Fallbacks = nil
	cfg.RuntimeParams = map[string]string{"default_transaction_read_only": "on"}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: spec.Host}
	if len(spec.CACert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(spec.CACert) {
			return nil, 0, errors.New("invalid source CA")
		}

		tlsConfig.RootCAs = pool
	}

	switch spec.TLSMode {
	case backup.SourceTLSVerifyFull:
	case backup.SourceTLSVerifyCA:
		if tlsConfig.RootCAs == nil {
			return nil, 0, errors.New("verify-ca requires an explicit source CA")
		}

		tlsConfig.InsecureSkipVerify = true // The callback verifies the certificate chain without checking the hostname.
		tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			opts := x509.VerifyOptions{Roots: tlsConfig.RootCAs, Intermediates: x509.NewCertPool()}
			for _, cert := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(cert)
			}

			_, err := cs.PeerCertificates[0].Verify(opts)

			return err
		}
	case backup.SourceTLSRequire:
		tlsConfig.InsecureSkipVerify = true // Explicit source require mode.
	default:
		return nil, 0, errors.New("unsupported source TLS mode")
	}

	cfg.TLSConfig = tlsConfig

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, 0, errors.New("source inventory connection failed")
	}
	defer closeWorkstationCopyConn(ctx, conn)

	extensions, err := copyExtensionInventory(ctx, conn)
	if err != nil {
		return nil, 0, err
	}

	var size int64
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.pg_database_size(pg_catalog.current_database())").Scan(&size); err != nil || size <= 0 {
		return nil, 0, errors.New("could not estimate the source database footprint")
	}

	return extensions, size, nil
}

func closeWorkstationCopyConn(ctx context.Context, conn *pgx.Conn) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	_ = conn.Close(closeCtx)
}

func copyExtensionInventory(ctx context.Context, conn *pgx.Conn) ([]backup.SourceExtension, error) {
	rows, err := conn.Query(ctx, `SELECT e.extname, e.extversion, n.nspname FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace ORDER BY e.extname`)
	if err != nil {
		return nil, errors.New("could not inspect database extensions")
	}
	defer rows.Close()

	var result []backup.SourceExtension

	for rows.Next() {
		var ext backup.SourceExtension
		if err := rows.Scan(&ext.Name, &ext.Version, &ext.Schema); err != nil {
			return nil, errors.New("could not inspect database extensions")
		}

		result = append(result, ext)
	}

	if rows.Err() != nil {
		return nil, errors.New("could not inspect database extensions")
	}

	return result, nil
}

func runWorkstationCopy(cmd *cobra.Command, p *progress, s *session, cfg config.Cluster, dest copyDestination, input *copySourceInput) error {
	ctx, cancel := context.WithTimeout(commandContext(cmd), 6*time.Hour)
	defer cancel()

	db := dest.spec()
	db.Extensions = []string{"pg_stat_statements"}

	planned, warnings, err := previewDBCreate(db, dest.AllowOvercommit, func(method string, params map[string]any, opts remote.CallOptions) (remote.Reply, error) {
		opts.OnEvent = p.event
		return p.call(ctx, s.node, method, params, opts)
	})
	if err != nil {
		return err
	}

	warnings = append(s.warnings, warnings...)
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]any{"database": planned}, Message: "dry run: would copy into a new database " + dest.Name + "; source compatibility and reachability were not checked", Warnings: warnings})
	}

	spec, extensions, sourceBytes, sourceWarning, err := prepareWorkstationSource(ctx, cmd, p, dest, input)
	if err != nil {
		return err
	}

	if sourceWarning != nil {
		warnings = append(warnings, *sourceWarning)
	}
	// This independently known plan intentionally excludes all non-builtin
	// source extensions until pre-create target metadata is available.
	target := []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}}

	spool, err := dumpWorkstationSource(ctx, spec)
	if err != nil {
		return err
	}

	cleaned := false
	defer func() {
		if !cleaned {
			_ = spool.remove()
		}
	}()

	selection, err := inspectWorkstationArchive(ctx, spool, spec.Database, extensions, target)
	if err != nil {
		return err
	}

	secret, values, err := newDatabaseSecret(cfg, dest.Name, steps.DatabaseRoles)
	if err != nil {
		return err
	}

	if err := addCreateServiceSecrets(cmd, s, p, values); err != nil {
		return err
	}

	params := dest.params()
	params["statement_timeout"] = db.StatementTimeout
	params["extensions"] = db.Extensions
	params["copy_source_bytes"] = sourceBytes

	reply, err := p.call(ctx, s.node, "db.create", params, remote.CallOptions{Secrets: values, OnEvent: p.event})
	if err != nil {
		if copySpaceRefused(err) {
			return err
		}

		return workstationCopyFailure(cmd, dest.Name, secret, "database creation failed or its outcome is unknown")
	}

	created, endpoint, err := copyCreatedDestination(reply, dest.Name, s.ctx)
	if err != nil {
		return workstationCopyFailure(cmd, dest.Name, secret, "the created database placement could not be verified")
	}

	if err := restoreWorkstationCopy(ctx, s, endpoint, dest.Name, secret.Roles["migrate"].User, secret.Roles["migrate"].Password, spool, selection, extensions); err != nil {
		return workstationCopyFailure(cmd, dest.Name, secret, "restore failed or its commit outcome is unknown")
	}

	cleaned = true

	if err := spool.remove(); err != nil {
		return workstationCopyFailure(cmd, dest.Name, secret, "restore completed but the private copy workspace could not be removed")
	}

	headline := "copied into " + dest.Name + " with its own roles"
	message, human := credentialsMessage(cmd, headline, headline, dest.Name, secret)

	return output.Success(cmd, output.Result{Data: map[string]any{"database": secret, "cluster": created}, Message: message, Human: human, Warnings: append(warnings, reply.Warnings...)})
}

// These refusals happen before the node writes topology.
func copySpaceRefused(err error) bool {
	cliErr, ok := errors.AsType[*output.CLIError](err)
	if !ok {
		return false
	}

	details, _ := cliErr.Details.(map[string]any)

	switch details["reason"] {
	case "insufficient_copy_space", "copy_space_unavailable", "invalid_copy_size":
		return true
	default:
		return false
	}
}

func prepareWorkstationSource(ctx context.Context, cmd *cobra.Command, p *progress, dest copyDestination, input *copySourceInput) (backup.SourceSpec, []backup.SourceExtension, int64, *output.Warning, error) {
	if input == nil {
		return backup.SourceSpec{}, nil, 0, nil, copyInputRequired("source", "Pass a newline-terminated PostgreSQL URL on stdin with --source-url-stdin.")
	}

	if (MustNotPrompt(cmd) || flagTrue(cmd, "yes")) && len(confirmTokens(cmd)) == 0 {
		if err := Confirm(cmd, dest.Name); err != nil {
			return backup.SourceSpec{}, nil, 0, nil, err
		}
	}

	spec, err := workstationSourceSpec(*input)
	if err != nil {
		return backup.SourceSpec{}, nil, 0, nil, err
	}

	validatedSource, err := backup.NewSource(spec)
	if err != nil {
		return backup.SourceSpec{}, nil, 0, nil, err
	}

	var sourceWarning *output.Warning
	if warning, ok := validatedSource.TLSWarning(); ok {
		sourceWarning = &warning
		p.warn(warning.Message)
	}

	extensions, sourceBytes, err := workstationSourceInventoryAndSize(ctx, spec)
	if err != nil {
		return backup.SourceSpec{}, nil, 0, nil, err
	}
	// Do not infer destination extension metadata from the workstation's
	// installed control files. Until a destination pre-create inventory is
	// available, only the PostgreSQL built-in is known independently.
	target := []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}}
	if _, err := backup.MatchSourceExtensions(extensions, target); err != nil {
		return backup.SourceSpec{}, nil, 0, nil, err
	}

	p.panel("Copy summary", []string{"Source: " + net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port)) + " (database " + spec.Database + ")", "Destination: new database " + dest.Name, "The workstation exports a snapshot and restores it over WireGuard. Trust the source: dumps contain executable SQL.", "Destination space is checked using a source footprint estimate and reserves; it is not a quota or a guarantee.", "The destination identity check and restore use separate TLS-verified connections.", "The source is not modified apart from normal read/export load and locks. Credentials are shown once."})

	if err := Confirm(cmd, dest.Name); err != nil {
		return backup.SourceSpec{}, nil, 0, nil, err
	}

	return spec, extensions, sourceBytes, sourceWarning, nil
}

func workstationCopyFailure(cmd *cobra.Command, name string, secret databaseSecret, message string) error {
	note := "The database may exist and these credentials may be in use. Inspect it before deciding what to do; vaire will not drop it or replay the import."
	printCopyCredentials(cmd, note, message, name, secret)

	return output.NewError(output.CodeError, message, output.ExitGeneric, note).WithDetails(map[string]any{"database": secret, "credentials_note": note + " " + shownOnceNote})
}

func restoreWorkstationCopy(ctx context.Context, s *session, endpoint dbEndpoint, database, user, password string, spool *workstationCopySpool, selection *backup.TOCSelection, extensions []backup.SourceExtension) error {
	// A separate, TLS-verified connection checks migrate's login and effective
	// owner role. pg_restore makes another connection with the same parameters.
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return err
	}

	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = endpoint.WGAddress, pgPort, user, password, database
	cfg.ConnectTimeout, cfg.Fallbacks = 15*time.Second, nil
	cfg.RuntimeParams = map[string]string{}
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.Hostname}
	cfg.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return s.dialer.DialContext(ctx, network, endpoint.addr(pgPort))
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return errors.New("verified destination connection failed")
	}

	var sessionUser, currentUser, currentDB string

	err = conn.QueryRow(ctx, "SELECT session_user, current_user, current_database()").Scan(&sessionUser, &currentUser, &currentDB)
	if err == nil && (sessionUser != user || currentUser != ownerRole(database) || currentDB != database) {
		err = errors.New("unexpected destination identity")
	}

	if err == nil {
		var actual []backup.SourceExtension

		actual, err = copyExtensionInventory(ctx, conn)
		if err == nil {
			_, err = backup.MatchSourceExtensions(extensions, actual)
		}
	}

	closeWorkstationCopyConn(ctx, conn)

	if err != nil {
		return err
	}

	restore, err := localenv.ClientPath("pg_restore")
	if err != nil {
		return err
	}

	list, err := os.OpenFile(filepath.Join(spool.dir, "selection"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	err = selection.WriteList(list)
	if closeErr := list.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return err
	}

	passfile := filepath.Join(spool.dir, "target-passfile")

	fields := []string{endpoint.Hostname, "*", database, user, password}
	for i := range fields {
		fields[i] = strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(fields[i])
	}

	if err := os.WriteFile(passfile, []byte(strings.Join(fields, ":")+"\n"), 0o600); err != nil {
		return err
	}

	ln, port, err := listenLocal(ctx, defaultPostgresPort)
	if err != nil {
		return err
	}

	proxyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = ln.Close() }()

	go proxy(proxyCtx, ln, s.dialer, endpoint.addr(pgPort))

	child := workstationRestoreCommand(ctx, restore, spool.dir, endpoint.Hostname, port, database, user, passfile)
	child.Stdout, child.Stderr = io.Discard, io.Discard

	child.WaitDelay = 3 * time.Second
	if err := child.Run(); err != nil {
		return fmt.Errorf("restore failed or was interrupted")
	}

	return nil
}

func workstationRestoreCommand(ctx context.Context, restore, dir, hostname string, port int, database, user, passfile string) *exec.Cmd {
	child := exec.CommandContext(ctx, restore, "--exit-on-error", "--single-transaction", "--no-owner", "--no-acl", "--no-tablespaces", "--no-password", "--dbname", database, "--use-list", filepath.Join(dir, "selection"), filepath.Join(dir, "archive")) //nolint:gosec // restore is the managed client path.
	child.Env = []string{"HOME=" + dir, "LC_ALL=C", "PGHOST=" + hostname, "PGHOSTADDR=127.0.0.1", "PGPORT=" + strconv.Itoa(port), "PGUSER=" + user, "PGDATABASE=" + database, "PGPASSFILE=" + passfile, "PGSSLMODE=verify-full", "PGSSLROOTCERT=system", "PGGSSENCMODE=disable", "PGCONNECT_TIMEOUT=15"}

	return child
}
