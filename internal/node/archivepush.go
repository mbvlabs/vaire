package node

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"syscall"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/masterbackup"
)

// pgbackrestBin is where Debian's and Ubuntu's pgbackrest package installs it.
const pgbackrestBin = "/usr/bin/pgbackrest"

// stanzaPattern guards the stanza handed to pgBackRest.
var stanzaPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}-[0-9a-f]{6}$`)

// ArchivePush is every cluster's archive_command (Phase 5, D5): PostgreSQL
// runs it as postgres for each WAL segment. On a node a newer master fenced
// it refuses, so the segment stays in pg_wal and nothing reaches the
// stanza; otherwise it replaces itself with pgBackRest's archive-push. It
// reads a local file only, never the bucket, and takes no lock.
func ArchivePush(stanza, walPath string) error {
	if !stanzaPattern.MatchString(stanza) {
		return output.NewError(output.CodeUsage, "archive-push needs --stanza <db>-<6 hex>", output.ExitUsage, "")
	}
	if fenced, err := archiveFenced(); err != nil {
		// Fail closed: PostgreSQL keeps the segment and retries.
		fmt.Fprintln(os.Stderr, "eru: can't tell whether this node is fenced ("+err.Error()+"); not archiving "+walPath+" to "+stanza)
		return output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Check that postgres can stat "+masterbackup.FencedPath+" (/etc/eru is 0755).")
	} else if fenced {
		fmt.Fprintln(os.Stderr, "eru: this node is fenced by a newer master; not archiving "+walPath+" to "+stanza)
		return fencedError()
	}
	bin, err := exec.LookPath("pgbackrest")
	if err != nil {
		// The postmaster's PATH may be minimal; the package installs here.
		if _, serr := os.Stat(pgbackrestBin); serr == nil {
			bin, err = pgbackrestBin, nil
		}
	}
	if err != nil {
		return output.WrapError(output.CodeMissingTool, err, output.ExitMissingTool, "pgBackRest is installed by postgres-packages-v1: run eru apply.")
	}
	return syscall.Exec(bin, []string{"pgbackrest", "--stanza=" + stanza, "archive-push", walPath}, os.Environ()) //nolint:gosec // G204: a validated stanza and PostgreSQL's %p.
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
