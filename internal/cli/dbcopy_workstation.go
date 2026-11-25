package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/localenv"
)

// This limit applies only to the compressed local spool, not to the space
// required by the restored database or its indexes on the destination.
const workstationCopyArchiveLimit int64 = 10 << 30

var errWorkstationCopyArchiveLimit = errors.New("the compressed copy archive exceeds the 10 GiB local spool limit")

// dumpWorkstationSource only exports. Its result must not be used for
// provisioning until its TOC and extension policy have been inspected.
func dumpWorkstationSource(ctx context.Context, spec backup.SourceSpec) (*workstationCopySpool, error) {
	tool, err := localenv.ClientPath("pg_dump")
	if err != nil {
		return nil, errors.New("the managed PostgreSQL dump tool is unavailable")
	}

	return dumpWorkstationSourceWithTool(ctx, tool, spec)
}

func dumpWorkstationSourceWithTool(ctx context.Context, tool string, spec backup.SourceSpec) (_ *workstationCopySpool, err error) {
	src, err := backup.NewSource(spec)
	if err != nil {
		return nil, err
	}

	spool, err := newWorkstationCopySpool()
	if err != nil {
		return nil, err
	}

	keep := false
	defer func() {
		if !keep {
			if cleanupErr := spool.remove(); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()

	env, err := workstationSourceEnvironment(spool, src, spec.Password)
	if err != nil {
		return nil, err
	}

	dumpCtx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	// tool is resolved from the managed bundle; NewSource validated the
	// connection fields, which are passed as separate arguments, not shell text.
	cmd := exec.CommandContext(dumpCtx, tool, "--format=custom", "--no-acl", "--no-password", //nolint:gosec // G204: fixed managed tool and validated source arguments.
		"--host", src.Host, "--port", strconv.Itoa(src.Port), "--username", src.User, "--dbname", src.Database)
	cmd.Env = env
	cmd.Stdout = spool
	cmd.Stderr = io.Discard // Source diagnostics can contain data and credentials.

	runErr := cmd.Run()
	if runErr != nil {
		if errors.Is(runErr, errWorkstationCopyArchiveLimit) {
			return nil, errWorkstationCopyArchiveLimit
		}

		return nil, errors.New("source export failed or was interrupted; no destination was created")
	}

	if spool.size == 0 {
		return nil, errors.New("source export produced an empty archive")
	}

	if err = spool.file.Sync(); err != nil {
		return nil, errors.New("the source archive could not be written to disk")
	}

	if err = os.Remove(filepath.Join(spool.dir, "source-passfile")); err != nil {
		return nil, errors.New("the temporary source passfile could not be removed")
	}

	if len(src.CACert()) > 0 {
		if err = os.Remove(filepath.Join(spool.dir, "source-ca")); err != nil {
			return nil, errors.New("the temporary source CA file could not be removed")
		}
	}

	if _, err = spool.file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("the source archive could not be read")
	}

	keep = true

	return spool, nil
}

func workstationSourceEnvironment(spool *workstationCopySpool, src backup.Source, password string) ([]string, error) {
	passfile := filepath.Join(spool.dir, "source-passfile")
	fields := []string{src.Host, strconv.Itoa(src.Port), src.Database, src.User, password}

	for i := range fields {
		fields[i] = strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(fields[i])
	}

	if err := os.WriteFile(passfile, []byte(strings.Join(fields, ":")+"\n"), 0o600); err != nil {
		return nil, errors.New("could not create the private source passfile")
	}

	env := []string{
		"HOME=" + spool.dir,
		"LC_ALL=C",
		"PGPASSFILE=" + passfile,
		"PGCONNECT_TIMEOUT=15",
		"PGGSSENCMODE=disable",
		"PGSSLMODE=" + src.TLSMode,
		"PGCLIENTENCODING=UTF8",
	}
	if ca := src.CACert(); len(ca) > 0 {
		caFile := filepath.Join(spool.dir, "source-ca")
		if err := os.WriteFile(caFile, ca, 0o600); err != nil {
			return nil, errors.New("could not create the private source CA file")
		}

		env = append(env, "PGSSLROOTCERT="+caFile)
	} else if src.TLSMode == backup.SourceTLSVerifyFull {
		env = append(env, "PGSSLROOTCERT=system")
	} else if src.TLSMode == backup.SourceTLSVerifyCA {
		// libpq's "system" root mode requires verify-full; do not silently
		// substitute an unverified connection.
		return nil, errors.New("verify-ca requires an explicit source CA")
	}

	return env, nil
}

// workstationArchiveExecutor adapts the existing TOC parser and selector to
// the installed workstation pg_restore. It never runs node commands.
type workstationArchiveExecutor struct{ restore string }

func (e workstationArchiveExecutor) Exec(ctx context.Context, _ string, command engine.Command, _ engine.Reporter) error {
	if filepath.Base(command.Name) != "pg_restore" || len(command.Env) != 0 {
		return errors.New("invalid archive inspection command")
	}
	// Only the existing TOC parser and selector construct these arguments;
	// e.restore is the managed bundle client, never a shell command.
	child := exec.CommandContext(ctx, e.restore, command.Args...) //nolint:gosec // G204: managed tool and internally constructed inspection arguments.
	child.Env = []string{"LC_ALL=C"}
	child.Stdin = command.Stdin
	child.Stdout = command.Stdout
	child.Stderr = io.Discard

	child.WaitDelay = 3 * time.Second
	if err := child.Run(); err != nil {
		return errors.New("archive inspection command failed")
	}

	return nil
}

// workstationAllowedTOC accepts only known PostgreSQL 18 archive descriptions.
// Credential-bearing and outbound-connection descriptions are deliberately
// not present. Descriptions precede unquoted names in pg_restore output, so
// check longer descriptions first and reject known disallowed prefixes before
// considering the permitted forms.
func workstationAllowedTOC(toc backup.ArchiveTOC) error {
	disallowed := []string{
		"TABLE ATTACH", // handled below only if explicitly admitted in future
		"DEFAULT ACL", "ACL", "SECURITY LABEL", "USER MAPPING",
		"FOREIGN DATA WRAPPER", "FOREIGN SERVER", "FOREIGN TABLE",
		"PUBLICATION", "PUBLICATION TABLE", "PUBLICATION TABLES IN SCHEMA",
		"SUBSCRIPTION", "EVENT TRIGGER",
	}

	allowed := []string{
		"DATABASE PROPERTIES", "DATABASE", "EXTENSION", "COMMENT",
		"ENCODING", "STDSTRINGS", "SEARCHPATH",
		"SCHEMA", "TABLE DATA", "TABLE", "SEQUENCE OWNED BY", "SEQUENCE SET", "SEQUENCE",
		"INDEX ATTACH", "INDEX", "FK CONSTRAINT", "CHECK CONSTRAINT", "CONSTRAINT",
		"DOMAIN CONSTRAINT", "DOMAIN", "DEFAULT", "TRIGGER", "FUNCTION", "PROCEDURE",
		"TYPE", "VIEW", "MATERIALIZED VIEW DATA", "MATERIALIZED VIEW", "RULE",
		"ROW SECURITY", "POLICY", "BLOB COMMENTS", "BLOBS", "BLOB",
	}
	for _, entry := range toc.Entries {
		if err := workstationAllowedTOCEntry(entry.Rest, disallowed, allowed); err != nil {
			return err
		}
	}

	return nil
}

func workstationAllowedTOCEntry(rest string, disallowed, allowed []string) error {
	for _, kind := range disallowed {
		if strings.HasPrefix(rest, kind+" ") {
			return errors.New("the source archive contains an unsupported object class")
		}
	}

	for _, kind := range allowed {
		if after, ok := strings.CutPrefix(rest, kind+" "); ok {
			// pg_restore does not quote TOC names. Limit accepted names
			// to unambiguous whitespace-free fields rather than guess
			// whether an extra word is an unknown longer description.
			fields := strings.Fields(after)
			valid := len(fields) == 3

			switch kind {
			case "EXTENSION", "ENCODING", "STDSTRINGS", "SEARCHPATH":
				valid = len(fields) == 2 && fields[0] == "-"
			case "SEQUENCE OWNED BY":
				// namespace, sequence, owner
				valid = len(fields) == 3 && fields[0] != "-" && fields[1] != "-" && fields[2] != "-"
			case "DEFAULT", "CONSTRAINT":
				// namespace, table, column or constraint, owner
				valid = len(fields) == 4 && fields[0] != "-" && fields[1] != "-" && fields[2] != "-" && fields[3] != "-"
			case "COMMENT":
				valid = len(fields) == 3 ||
					(len(fields) == 2 && fields[0] == "-") ||
					(len(fields) == 4 && fields[0] == "-" && fields[1] == "DATABASE")
			}

			if !valid || strings.ContainsAny(rest, "\t\r\n") {
				return errors.New("the source archive contains an ambiguous object description")
			}

			return nil
		}
	}

	return errors.New("the source archive contains an unknown object class")
}

// inspectWorkstationArchive must complete before db.create. sourceExtensions
// must be the complete source catalog and targetExtensions the independently
// verified preprovisioning plan, both including builtin plpgsql where present.
// The returned selection contains only numeric IDs, suitable for --use-list.
func inspectWorkstationArchive(ctx context.Context, spool *workstationCopySpool, database string, sourceExtensions, targetExtensions []backup.SourceExtension) (*backup.TOCSelection, error) {
	restore, err := localenv.ClientPath("pg_restore")
	if err != nil {
		return nil, errors.New("the managed PostgreSQL restore tool is unavailable")
	}

	return inspectWorkstationArchiveWithTool(ctx, restore, spool, database, sourceExtensions, targetExtensions)
}

func inspectWorkstationArchiveWithTool(ctx context.Context, restore string, spool *workstationCopySpool, database string, sourceExtensions, targetExtensions []backup.SourceExtension) (selection *backup.TOCSelection, err error) {
	if spool == nil || spool.file == nil || spool.size == 0 {
		return nil, errors.New("the source archive is empty")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	if err := workstationCopyExtensions(sourceExtensions, targetExtensions); err != nil {
		return nil, err
	}

	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("the source archive could not be read")
	}

	executor := workstationArchiveExecutor{restore: restore}

	toc, err := backup.ReadArchiveTOC(ctx, executor, "workstation-copy-list", nil, spool.file)
	if err != nil {
		return nil, err
	}

	if err := workstationAllowedTOC(toc); err != nil {
		return nil, err
	}

	// The existing selector expects a verified private CopyWorkspace and its
	// fixed archive filename. Rename within this same private filesystem; do
	// not make a second unbounded archive copy.
	root := filepath.Join(spool.dir, "inspection")

	ws, err := backup.NewCopyWorkspace(root, "copy", backup.CurrentWorkspaceOwner(), nil)
	if err != nil {
		return nil, errors.New("the private archive inspection workspace could not be created")
	}

	defer func() {
		if closeErr := ws.Close(); closeErr != nil {
			err = errors.Join(err, errors.New("the archive inspection workspace could not be closed"))
		}

		if removeErr := os.RemoveAll(root); removeErr != nil {
			err = errors.Join(err, errors.New("the archive inspection workspace could not be removed"))
		}
	}()

	archivePath := filepath.Join(spool.dir, "archive")
	if err := os.Rename(archivePath, ws.FilePath("source.dump")); err != nil {
		return nil, errors.New("the source archive could not be prepared for inspection")
	}

	defer func() {
		if moveErr := os.Rename(ws.FilePath("source.dump"), archivePath); moveErr != nil {
			err = errors.Join(err, errors.New("the source archive could not be retained after inspection"))
			selection = nil
		}
	}()

	selection, err = backup.SelectArchiveTOC(ctx, executor, "workstation-copy-select", nil, ws,
		backup.SourceArchive{Size: spool.size, TOC: toc},
		backup.SelectionOptions{SourceDatabase: database, Extensions: sourceExtensions})

	return selection, err
}

func workstationCopyExtensions(sourceExtensions, targetExtensions []backup.SourceExtension) error {
	foundBuiltin := false

	for _, ext := range sourceExtensions {
		if ext.Name != "plpgsql" && ext.Name != "pg_stat_statements" {
			return errors.New("the source uses an unsupported extension")
		}

		if ext.Name == "plpgsql" && ext.Schema != "pg_catalog" {
			return errors.New("the source builtin extension is in an unexpected schema")
		}

		foundBuiltin = foundBuiltin || ext.Name == "plpgsql"
	}

	if !foundBuiltin {
		return errors.New("the source extension inventory is incomplete")
	}

	if _, err := backup.MatchSourceExtensions(sourceExtensions, targetExtensions); err != nil {
		return err
	}

	return nil
}

// workstationCopySpool bounds writes while keeping the archive private. The
// caller must close and remove it on every exit, including cancellation.
type workstationCopySpool struct {
	dir   string
	file  *os.File
	size  int64
	limit int64
}

func newWorkstationCopySpool() (*workstationCopySpool, error) {
	dir, err := os.MkdirTemp("", "vaire-db-copy-")
	if err != nil {
		return nil, errors.New("could not create the private copy workspace")
	}

	file, err := os.OpenFile(filepath.Join(dir, "archive"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, errors.New("could not create the private copy archive")
	}

	return &workstationCopySpool{dir: dir, file: file, limit: workstationCopyArchiveLimit}, nil
}

func (s *workstationCopySpool) Write(p []byte) (int, error) {
	if int64(len(p)) > s.limit-s.size {
		return 0, errWorkstationCopyArchiveLimit
	}

	n, err := s.file.Write(p)
	s.size += int64(n)

	return n, err
}

func (s *workstationCopySpool) remove() error {
	closeErr := s.file.Close()

	removeErr := os.RemoveAll(s.dir)
	if closeErr != nil || removeErr != nil {
		return fmt.Errorf("could not remove the private copy workspace")
	}

	return nil
}
