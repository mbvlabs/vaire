package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
)

func TestWorkstationCopySpoolBoundAndPrivacy(t *testing.T) {
	spool, err := newWorkstationCopySpool()
	if err != nil {
		t.Fatal(err)
	}

	dir := spool.dir

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	for _, check := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0o700}, {filepath.Join(dir, "archive"), 0o600}} {
		info, err := os.Stat(check.path)
		if err != nil || info.Mode().Perm() != check.mode {
			t.Fatalf("permissions for %s: %v, %v", check.path, info, err)
		}
	}

	spool.limit = 4
	if n, err := spool.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("write at limit: %d, %v", n, err)
	}

	if n, err := spool.Write([]byte("5")); n != 0 || !errors.Is(err, errWorkstationCopyArchiveLimit) {
		t.Fatalf("write beyond limit: %d, %v", n, err)
	}

	if err := spool.remove(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workspace remains: %v", err)
	}
}

func TestDumpWorkstationSource(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "pg_dump")

	script := `#!/bin/sh
test -z "$PGHOST" || exit 1
test "$PGSSLMODE" = verify-full || exit 2
test "$PGSSLROOTCERT" = system || exit 3
test "$(cat "$PGPASSFILE")" = 'db.example:5432:app:alice:p\:a\\ss' || exit 4
printf archive
`
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PGHOST", "must-not-inherit")

	spec := backup.SourceSpec{Host: "db.example", Database: "app", User: "alice", Password: `p:a\ss`}

	spool, err := dumpWorkstationSourceWithTool(context.Background(), tool, spec)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = spool.remove() }()

	got, err := io.ReadAll(spool.file)
	if err != nil || string(got) != "archive" {
		t.Fatalf("archive = %q, %v", got, err)
	}

	if _, err := os.Stat(filepath.Join(spool.dir, "source-passfile")); !os.IsNotExist(err) {
		t.Fatalf("source passfile remains: %v", err)
	}
}

func TestDumpWorkstationSourceFailureDoesNotExposeDiagnostics(t *testing.T) {
	tool := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf 'sensitive-source-data\\n' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	spec := backup.SourceSpec{Host: "db.example", Database: "app", User: "alice", Password: "secret"}

	spool, err := dumpWorkstationSourceWithTool(context.Background(), tool, spec)
	if spool != nil || err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("spool = %v, error = %v", spool, err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace not cleaned up: %v, %v", entries, err)
	}
}

func TestInspectWorkstationArchive(t *testing.T) {
	spool, err := newWorkstationCopySpool()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spool.remove() }()

	if _, err := spool.Write([]byte("archive")); err != nil {
		t.Fatal(err)
	}

	tool := filepath.Join(t.TempDir(), "pg_restore")

	script := `#!/bin/sh
case "$1" in
--list)
  printf '%s\n' '; dbname: app' '; Format: CUSTOM' \
    '1; 1262 0 DATABASE - app owner' \
    '2; 3079 0 EXTENSION - pg_stat_statements ' \
    '3; 0 0 COMMENT - "pg_stat_statements" ' \
    '4; 1259 42 TABLE public things owner' \
    '5; 0 42 TABLE DATA public things owner' \
    '6; 0 0 SEQUENCE OWNED BY public things_id_seq owner' \
    '7; 2604 43 DEFAULT public things id owner' \
    '8; 2606 44 CONSTRAINT public things things_pkey owner'
  ;;
--create)
  for arg in "$@"; do case "$arg" in --use-list=*) list="${arg#--use-list=}";; esac; done
  case "$(/bin/cat "$list")" in
    '1;') printf '%s\n' 'CREATE DATABASE "app" WITH TEMPLATE = template0;';;
    '2;') printf '%s\n' 'CREATE EXTENSION IF NOT EXISTS "pg_stat_statements" WITH SCHEMA "public";';;
    '3;') printf '%s\n' 'COMMENT ON EXTENSION "pg_stat_statements" IS '\''stats'\'';';;
    *) exit 1;;
  esac
  ;;
*) exit 1;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	extensions := []backup.SourceExtension{
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		{Name: "pg_stat_statements", Version: "1.11", Schema: "public"},
	}

	selection, err := inspectWorkstationArchiveWithTool(context.Background(), tool, spool, "app", extensions, extensions)
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(selection.Lines, " "); got != "4; 5; 6; 7; 8;" {
		t.Fatalf("selection = %q", got)
	}

	if _, err := os.Stat(filepath.Join(spool.dir, "archive")); err != nil {
		t.Fatalf("archive not retained: %v", err)
	}

	if _, err := os.Stat(filepath.Join(spool.dir, "inspection")); !os.IsNotExist(err) {
		t.Fatalf("inspection workspace not removed: %v", err)
	}

	mismatch := append([]backup.SourceExtension(nil), extensions...)

	mismatch[1].Version = "other"
	if _, err := inspectWorkstationArchiveWithTool(context.Background(), tool, spool, "app", extensions, mismatch); err == nil {
		t.Fatal("accepted an extension version mismatch")
	}

	if _, err := inspectWorkstationArchiveWithTool(context.Background(), tool, spool, "wrong", extensions, extensions); err == nil {
		t.Fatal("accepted a different source database")
	}

	unsupported := append(append([]backup.SourceExtension(nil), extensions...), backup.SourceExtension{Name: "unapproved", Version: "1", Schema: "public"})
	if _, err := inspectWorkstationArchiveWithTool(context.Background(), tool, spool, "app", unsupported, unsupported); err == nil {
		t.Fatal("accepted an unsupported extension")
	}

	if _, err := inspectWorkstationArchiveWithTool(context.Background(), tool, spool, "app", nil, nil); err == nil {
		t.Fatal("accepted an incomplete source extension inventory")
	}
}

func TestWorkstationTOCSerialShapes(t *testing.T) {
	for _, tc := range []struct {
		rest string
		ok   bool
	}{
		{"SEQUENCE OWNED BY public items_id_seq vaire", true},
		{"DEFAULT public items id vaire", true},
		{"CONSTRAINT public items items_pkey vaire", true},
		{"FK CONSTRAINT public items items_parent_fkey vaire", true},
		{"SEQUENCE OWNED BY public items_id_seq", false},
		{"SEQUENCE OWNED BY public items_id_seq extra vaire", false},
		{"DEFAULT public items vaire", false},
		{"DEFAULT public items id extra vaire", false},
		{"CONSTRAINT public items vaire", false},
		{"CONSTRAINT public items items_pkey extra vaire", false},
		{"DEFAULT - items id vaire", false},
		{"DEFAULT ACL public items id vaire", false},
		{"CONSTRAINT public items items_pkey -", false},
		{"FK CONSTRAINT public items vaire", false},
		{"FK CONSTRAINT public items items_parent_fkey extra vaire", false},
	} {
		t.Run(tc.rest, func(t *testing.T) {
			err := workstationAllowedTOC(backup.ArchiveTOC{Entries: []backup.ArchiveEntry{{Rest: tc.rest}}})
			if (err == nil) != tc.ok {
				t.Fatalf("classify %q: %v", tc.rest, err)
			}
		})
	}
}

func TestInspectWorkstationArchiveRejectsInvalidListing(t *testing.T) {
	spool, err := newWorkstationCopySpool()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spool.remove() }()

	if _, err := spool.Write([]byte("archive")); err != nil {
		t.Fatal(err)
	}

	tool := filepath.Join(t.TempDir(), "pg_restore")
	exts := []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}}

	for name, script := range map[string]string{
		"duplicate": "#!/bin/sh\nprintf '%s\\n' '; dbname: app' '; Format: CUSTOM' '1; 1262 0 DATABASE - app owner' '1; 1259 1 TABLE public t owner'\n",
		"failure":   "#!/bin/sh\nprintf '%s\\n' secret-diagnostic >&2\nexit 1\n",
		"unknown":   "#!/bin/sh\nprintf '%s\\n' '; dbname: app' '; Format: CUSTOM' '1; 1262 0 DATABASE - app owner' '2; 0 0 SURPRISE public t owner'\n",
		"oversize":  "#!/bin/sh\n/bin/dd if=/dev/zero bs=1048576 count=65 2>/dev/null\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			selection, err := inspectWorkstationArchiveWithTool(context.Background(), tool, spool, "app", exts, exts)
			if selection != nil || err == nil || strings.Contains(err.Error(), "secret-diagnostic") {
				t.Fatalf("selection = %v, error = %v", selection, err)
			}
		})
	}
}

func TestWorkstationTOCRejectsUnsafeAndUnknownClasses(t *testing.T) {
	for _, desc := range []string{
		"FOREIGN SERVER - remote owner", "USER MAPPING - remote owner",
		"SUBSCRIPTION - outbound owner", "PUBLICATION TABLE public t owner",
		"TABLE ATTACH public t owner", "ACL public t owner",
		"SECURITY LABEL public t owner", "NEW CLASS public t owner", "TABLE MYSTERY public t owner",
	} {
		t.Run(desc, func(t *testing.T) {
			if err := workstationAllowedTOC(backup.ArchiveTOC{Entries: []backup.ArchiveEntry{{Rest: desc}}}); err == nil {
				t.Fatalf("accepted %q", desc)
			}
		})
	}
}

func TestWorkstationTOCIdentifiesAmbiguousEntryWithoutNames(t *testing.T) {
	toc := backup.ArchiveTOC{Entries: []backup.ArchiveEntry{
		{ID: 41, Rest: "TABLE public accepted_table owner"},
		{ID: 42, Rest: "FUNCTION public private_function(integer, text) private_owner"},
	}}

	err := workstationAllowedTOC(toc)
	if err == nil {
		t.Fatal("accepted ambiguous function description")
	}

	if got := err.Error(); got != "archive TOC entry 42: the source archive contains an ambiguous object description (FUNCTION prefix, 4 fields after prefix)" {
		t.Fatalf("unexpected diagnostic: %q", got)
	}
}

func TestWorkstationTOCRoutineSignatures(t *testing.T) {
	for _, tc := range []struct {
		rest     string
		tableOID uint32
		ok       bool
	}{
		{"FUNCTION public f(integer, text) owner", pgProcCatalogOID, true},
		{"FUNCTION public f(timestamp without time zone) owner", pgProcCatalogOID, true},
		{"PROCEDURE public p(integer, text) owner", pgProcCatalogOID, true},
		{"FUNCTION public f(integer, text) owner", 0, false},
		{"FUNCTION MYSTERY public f(integer, text) owner", pgProcCatalogOID, false},
		{"FUNCTION public f(integer,\ttext) owner", pgProcCatalogOID, false},
		{"FUNCTION public f(integer, text) unknown owner", pgProcCatalogOID, false},
	} {
		t.Run(tc.rest, func(t *testing.T) {
			entry := backup.ArchiveEntry{ID: 241, TableOID: tc.tableOID, OID: 16390, Rest: tc.rest}
			err := workstationAllowedTOC(backup.ArchiveTOC{Entries: []backup.ArchiveEntry{entry}})

			if (err == nil) != tc.ok {
				t.Fatalf("classify %q: %v", tc.rest, err)
			}
		})
	}
}

// This opt-in test requires a disposable fixture with two source_serial rows
// and an empty target. It never runs as part of the standard smoke fixture.
func TestWorkstationSerialRealArchive(t *testing.T) {
	if os.Getenv("VAIRE_COPY_SERIAL_SMOKE") != "1" {
		t.Skip("no disposable serial-copy fixture supplied")
	}

	portText := os.Getenv("VAIRE_COPY_SMOKE_PORT")
	if portText == "" {
		t.Skip("no disposable copy fixture supplied")
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid fixture port")
	}

	caFile := os.Getenv("VAIRE_COPY_SMOKE_CA")

	ca, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}

	client := os.Getenv("VAIRE_COPY_SMOKE_CLIENT")
	if client == "" {
		t.Fatal("no fixture client directory supplied")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	spec := backup.SourceSpec{Host: "localhost", Port: port, Database: "source_serial", User: "vaire", TLSMode: backup.SourceTLSVerifyFull, CACert: ca}

	spool, err := dumpWorkstationSourceWithTool(ctx, filepath.Join(client, "pg_dump"), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spool.remove() }()

	exts := []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}}

	selection, err := inspectWorkstationArchiveWithTool(ctx, filepath.Join(client, "pg_restore"), spool, "source_serial", exts, exts)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	toc, err := backup.ReadArchiveTOC(ctx, workstationArchiveExecutor{restore: filepath.Join(client, "pg_restore")}, "serial-smoke", nil, spool.file)
	if err != nil {
		t.Fatal(err)
	}

	checkSerialSelection(t, toc, selection)

	list, err := os.Create(filepath.Join(spool.dir, "selection"))
	if err != nil {
		t.Fatal(err)
	}

	if err := selection.WriteList(list); err != nil {
		_ = list.Close()

		t.Fatal(err)
	}

	if err := list.Close(); err != nil {
		t.Fatal(err)
	}

	passfile := filepath.Join(spool.dir, "target-passfile")
	if err := os.WriteFile(passfile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{"PGHOST=localhost", "PGPORT=" + portText, "PGUSER=target_migrate",
		"PGDATABASE=target", "PGPASSFILE=" + passfile, "PGSSLMODE=verify-full",
		"PGSSLROOTCERT=" + caFile, "PGGSSENCMODE=disable"}

	restore := exec.CommandContext(ctx, filepath.Join(client, "pg_restore"), "--single-transaction",
		"--exit-on-error", "--no-owner", "--no-acl", "--no-tablespaces", "--no-password",
		"--dbname", "target", "--use-list", filepath.Join(spool.dir, "selection"), filepath.Join(spool.dir, "archive"))
	restore.Env = env

	if out, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore failed: %v: %s", err, out)
	}

	psql := exec.CommandContext(ctx, filepath.Join(client, "psql"), "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c",
		"INSERT INTO serial_items DEFAULT VALUES RETURNING id; SELECT count(*), bool_and(id > 0), (SELECT count(*) FROM pg_constraint WHERE conrelid = 'serial_items'::regclass AND contype = 'p') FROM serial_items;")
	psql.Env = env

	out, err := psql.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "3\nINSERT 0 1\n3|t|1") {
		t.Fatalf("serial default, data or primary key: %q (%v)", out, err)
	}
}

func checkSerialSelection(t *testing.T, toc backup.ArchiveTOC, selection *backup.TOCSelection) {
	t.Helper()

	selected := make(map[string]bool)
	for _, line := range selection.Lines {
		selected[line] = true
	}

	required := map[string]bool{"SEQUENCE OWNED BY ": false, "DEFAULT ": false, "CONSTRAINT ": false}
	for _, entry := range toc.Entries {
		for kind := range required {
			if strings.HasPrefix(entry.Rest, kind) && selected[strconv.Itoa(entry.ID)+";"] {
				required[kind] = true
			}
		}
	}

	for kind, found := range required {
		if !found {
			t.Fatalf("real archive selection missing %s", kind)
		}
	}
}
