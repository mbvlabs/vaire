package node

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/render"
)

// PgBackRestBin is the binary archive-push runs without --pgbackrest: where
// the Debian and Ubuntu package installs pgBackRest, and what every
// archive_command passes.
const PgBackRestBin = render.PgBackRestBin

// stanzaPattern guards the stanza handed to pgBackRest.
var stanzaPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}-[0-9a-f]{6}$`)

// archiveLog is where a refusal's reason goes: PostgreSQL's archiver
// passes the command's stderr to the cluster's log.
var archiveLog io.Writer = os.Stderr

// ArchivePush is every cluster's archive_command (Phase 5, D5): PostgreSQL
// runs it as postgres for each WAL segment. On a node a newer master fenced
// it refuses, so the segment stays in pg_wal and nothing reaches the
// stanza; otherwise it replaces itself with pgBackRest's archive-push (bin,
// an absolute path to a binary named pgbackrest; PgBackRestBin when empty).
// It reads a local file only, never the bucket, and takes no lock. Every
// refusal writes its reason to stderr, so it shows in the PostgreSQL log.
func ArchivePush(bin, stanza, walPath string) error {
	refuse := func(reason string, err error) error {
		_, _ = fmt.Fprintf(archiveLog, "vaire archive-push: not archiving %q to stanza %q: %s\n", walPath, stanza, reason)
		return err
	}
	if !stanzaPattern.MatchString(stanza) {
		return refuse("--stanza must be <db>-<6 hex>",
			output.NewError(output.CodeUsage, "archive-push needs --stanza <db>-<6 hex>", output.ExitUsage, ""))
	}
	if walPath == "" {
		return refuse("it needs exactly one WAL path (PostgreSQL's %p)",
			output.NewError(output.CodeUsage, "archive-push needs one WAL path", output.ExitUsage, ""))
	}
	if bin == "" {
		bin = PgBackRestBin
	}
	if !filepath.IsAbs(bin) || filepath.Base(bin) != "pgbackrest" {
		return refuse("--pgbackrest must be an absolute path to pgbackrest, not "+fmt.Sprintf("%q", bin),
			output.NewError(output.CodeUsage, "archive-push needs --pgbackrest <absolute path to pgbackrest>", output.ExitUsage, ""))
	}
	if fenced, err := archiveFenced(); err != nil {
		// Fail closed: PostgreSQL keeps the segment and retries.
		return refuse("can't tell whether this node is fenced ("+err.Error()+")",
			output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Check that postgres can stat "+masterbackup.FencedPath+" (/etc/vaire is 0755)."))
	} else if fenced {
		return refuse("this node is fenced by a newer master", fencedError())
	}
	if _, err := os.Stat(bin); err != nil {
		return refuse("pgBackRest is not at "+bin+" ("+err.Error()+")",
			output.WrapError(output.CodeMissingTool, err, output.ExitMissingTool, "pgBackRest is installed by postgres-packages-v1: run vaire apply."))
	}
	err := syscall.Exec(bin, []string{"pgbackrest", "--stanza=" + stanza, "archive-push", walPath}, os.Environ()) //nolint:gosec // G204: a validated stanza and binary, and PostgreSQL's %p.
	return refuse("exec "+bin+" failed ("+err.Error()+")", output.WrapError(output.CodeMissingTool, err, output.ExitMissingTool, ""))
}

// archiveFenced reports whether archive-push must refuse: the fence marker
// exists, or its absence can't be confirmed (an error other than "no such
// file", such as permission denied).
func archiveFenced() (bool, error) {
	_, err := os.Stat(masterbackup.FencedFile())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}
