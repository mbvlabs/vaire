package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/vaire/internal/backup"
)

func TestWorkstationCopyPlaintextSourceSmoke(t *testing.T) {
	port := os.Getenv("VAIRE_COPY_SMOKE_PLAINTEXT_PORT")
	if port == "" {
		t.Skip("no disposable plaintext source supplied")
	}

	source := "postgres://vaire@127.0.0.1:" + port + "/source_serial"

	spec, err := workstationSourceSpec(copySourceInput{URL: source})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := workstationSourceInventory(t.Context(), spec); err == nil {
		t.Fatal("default TLS mode silently fell back to plaintext")
	}

	spec, err = workstationSourceSpec(copySourceInput{URL: source + "?sslmode=disable"})
	if err != nil {
		t.Fatal(err)
	}

	extensions, err := workstationSourceInventory(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}

	client := os.Getenv("VAIRE_COPY_SMOKE_CLIENT")

	spool, err := dumpWorkstationSourceWithTool(t.Context(), filepath.Join(client, "pg_dump"), spec)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = spool.remove() }()

	if !slices.ContainsFunc(extensions, func(ext backup.SourceExtension) bool { return ext.Name == "plpgsql" }) {
		t.Fatal("missing source extension inventory")
	}

	plan := []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}}
	if _, err := inspectWorkstationArchiveWithTool(t.Context(), filepath.Join(client, "pg_restore"), spool, spec.Database, extensions, plan); err != nil {
		t.Fatal(err)
	}
}

// This opt-in test uses a disposable local fixture with an untrusted server
// certificate. Loopback substitutes for the authenticated destination tunnel.
func TestWorkstationCopyRealArchiveSmoke(t *testing.T) {
	portText := os.Getenv("VAIRE_COPY_SMOKE_PORT")
	if portText == "" {
		t.Skip("no disposable copy fixture supplied")
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	caFile := os.Getenv("VAIRE_COPY_SMOKE_CA")

	ca, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}

	client := os.Getenv("VAIRE_COPY_SMOKE_CLIENT")
	if client == "" {
		t.Fatal("no client binary directory supplied")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	spec := backup.SourceSpec{Host: "localhost", Port: port, Database: "source", User: "vaire", TLSMode: backup.SourceTLSVerifyFull, CACert: ca}

	exts, err := workstationSourceInventory(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}

	targetPlan := []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}}
	if _, err := backup.MatchSourceExtensions(exts, targetPlan); err != nil {
		t.Fatal(err)
	}

	extSpec := spec
	extSpec.Database = "source_ext"

	sourceWithExtension, err := workstationSourceInventory(ctx, extSpec)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := backup.MatchSourceExtensions(sourceWithExtension, targetPlan); err == nil {
		t.Fatal("accepted unverified source pg_stat_statements")
	}
	// An ordinary serial primary key must pass archive inspection.
	serialSpec := spec
	serialSpec.Database = "source_serial"

	serialSpool, err := dumpWorkstationSourceWithTool(ctx, filepath.Join(client, "pg_dump"), serialSpec)
	if err != nil {
		t.Fatal(err)
	}

	_, err = inspectWorkstationArchiveWithTool(ctx, filepath.Join(client, "pg_restore"), serialSpool, "source_serial", exts, targetPlan)
	_ = serialSpool.remove()

	if err != nil {
		t.Fatalf("ordinary serial fixture refused before create: %v", err)
	}

	spool, err := dumpWorkstationSourceWithTool(ctx, filepath.Join(client, "pg_dump"), spec)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = spool.remove() }()

	if _, err := inspectWorkstationArchiveWithTool(ctx, filepath.Join(client, "pg_restore"), spool, "wrong_database", exts, targetPlan); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong database result: %v", err)
	}

	selection, err := inspectWorkstationArchiveWithTool(ctx, filepath.Join(client, "pg_restore"), spool, "source", exts, targetPlan)
	if err != nil {
		listing, _ := exec.CommandContext(ctx, filepath.Join(client, "pg_restore"), "--list", filepath.Join(spool.dir, "archive")).Output()
		t.Fatalf("%v\n%s", err, listing)
	}

	list, err := os.Create(filepath.Join(spool.dir, "selection"))
	if err != nil {
		t.Fatal(err)
	}

	if err := selection.WriteList(list); err != nil {
		t.Fatal(err)
	}

	if err := list.Close(); err != nil {
		t.Fatal(err)
	}

	passfile := filepath.Join(spool.dir, "target-passfile")
	if err := os.WriteFile(passfile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	child := workstationRestoreCommand(ctx, filepath.Join(client, "pg_restore"), spool.dir, "localhost", port, "target", "target_migrate", passfile)
	checkOldWorkstationTrustPolicy(t, ctx, client, child.Env)

	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("restore: %v: %s", err, out)
	}

	checkWorkstationSmokeData(t, ctx, port, ca)
}

func checkOldWorkstationTrustPolicy(t *testing.T, ctx context.Context, client string, env []string) {
	t.Helper()

	probe := exec.CommandContext(ctx, filepath.Join(client, "psql"), "-X", "-w", "-Atqc", "SELECT 1")

	probe.Env = append(append([]string(nil), env...), "PGSSLMODE=verify-full", "PGSSLROOTCERT=system")

	if out, err := probe.CombinedOutput(); err == nil || !strings.Contains(string(out), "certificate verify failed") {
		t.Fatalf("old libpq policy did not reproduce the CA failure: %v: %s", err, out)
	}
}

func checkWorkstationSmokeData(t *testing.T, ctx context.Context, port int, ca []byte) {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("invalid fixture CA")
	}

	cfg, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}

	cfg.Host, cfg.Port, cfg.Database, cfg.User = "127.0.0.1", uint16(port), "target", "target_migrate"
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "localhost", RootCAs: pool}
	cfg.Fallbacks = nil
	cfg.RuntimeParams = map[string]string{}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkstationCopyConn(ctx, conn)

	var sum int
	if err := conn.QueryRow(ctx, "select public.add_two(19, 23)").Scan(&sum); err != nil || sum != 42 {
		t.Fatalf("restored multi-argument function: sum=%d, err=%v", sum, err)
	}

	var login, current, database string
	if err := conn.QueryRow(ctx, "select session_user, current_user, current_database()").Scan(&login, &current, &database); err != nil {
		t.Fatal(err)
	}

	if login != "target_migrate" || current != "target_owner" || database != "target" {
		t.Fatalf("unexpected identity: %s %s %s", login, current, database)
	}

	var count, total, next int
	if err := conn.QueryRow(ctx, "select count(*), sum(value), nextval('items_id_seq') from items").Scan(&count, &total, &next); err != nil {
		t.Fatal(err)
	}

	if count != 2 || total != 42 || next != 3 {
		t.Fatalf("data or sequence mismatch: count=%d sum=%d next=%d", count, total, next)
	}

	var index, function, extension, owner string

	err = conn.QueryRow(ctx, `select to_regclass('public.items_value_idx')::text, to_regprocedure('public.add_one(integer)')::text, (select extversion from pg_extension where extname='pg_stat_statements'), (select pg_get_userbyid(relowner) from pg_class where oid='public.items'::regclass)`).Scan(&index, &function, &extension, &owner)
	if err != nil || index != "items_value_idx" || function != "add_one(integer)" || extension != "1.12" || owner != "target_owner" {
		t.Fatalf("objects: index=%q function=%q extension=%q owner=%q: %v", index, function, extension, owner, err)
	}

	_, err = conn.Exec(ctx, "insert into items values (999, -1)")

	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pgErr.Code != "23503" || pgErr.ConstraintName != "items_value_fkey" {
		t.Fatalf("restored foreign key did not reject an orphan row: %v", err)
	}
}
