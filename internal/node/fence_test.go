package node

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func fenceForTest(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(masterbackup.FencedPath)), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+masterbackup.FencedPath, []byte(`{"epoch":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(masterbackup.SetTestRoot(root))
}

func TestFencedRefusals(t *testing.T) {
	table := methods()
	req := func(name string) rpc.Request { return rpc.Request{Method: name} }
	if fencedRefuses(table["apply"], req("apply")) {
		t.Fatal("a node that isn't fenced refuses apply")
	}
	fenceForTest(t)
	for _, name := range []string{"apply", "db.create", "config.set", "backup.now", "restore.start", "db.drop"} {
		if !fencedRefuses(table[name], req(name)) {
			t.Errorf("a fenced node accepts %s", name)
		}
	}
	for _, name := range []string{"status", "doctor.run", "db.list", "logs", "ssh.status", "ssh.confirm", "selfcheck"} {
		if fencedRefuses(table[name], req(name)) {
			t.Errorf("a fenced node refuses the read %s", name)
		}
	}
}

// captureArchiveLog sends the wrapper's stderr lines to a buffer.
func captureArchiveLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := archiveLog
	archiveLog = &buf
	t.Cleanup(func() { archiveLog = old })
	return &buf
}

func TestArchivePushRefusesWhenFenced(t *testing.T) {
	logged := captureArchiveLog(t)
	fenceForTest(t)
	err := ArchivePush("", "app-3f9a1c", "pg_wal/000000010000000000000001")
	var cli *output.CLIError
	if !errors.As(err, &cli) || cli.Code != output.CodeNodeFenced {
		t.Fatalf("fenced archive-push: %v", err)
	}
	if !strings.Contains(logged.String(), "fenced by a newer master") {
		t.Fatalf("no reason on stderr: %q", logged.String())
	}
}

// Every refusal leaves its reason on stderr, which PostgreSQL's archiver
// writes to the cluster's log; a silent refusal shows only as a WAL backlog.
func TestArchivePushLogsEveryRefusal(t *testing.T) {
	const wal = "pg_wal/000000010000000000000001"
	notADir := t.TempDir()
	// /etc/vaire is a file here, so the fence marker's stat fails with
	// ENOTDIR: neither fenced nor provably not, so it must refuse.
	if err := os.MkdirAll(filepath.Join(notADir, "etc"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notADir, "etc", "vaire"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "pgbackrest")
	cases := []struct {
		name, bin, stanza, wal, root, reason string
	}{
		{"bad stanza", "", "bad stanza", wal, "", "--stanza must be"},
		{"no wal path", "", "app-3f9a1c", "", "", "exactly one WAL path"},
		{"relative binary", "pgbackrest", "app-3f9a1c", wal, "", "--pgbackrest must be"},
		{"other binary", "/bin/sh", "app-3f9a1c", wal, "", "--pgbackrest must be"},
		{"fence unknown", "", "app-3f9a1c", wal, notADir, "can't tell whether this node is fenced"},
		{"missing binary", missing, "app-3f9a1c", wal, t.TempDir(), "pgBackRest is not at " + missing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logged := captureArchiveLog(t)
			if c.root != "" {
				t.Cleanup(masterbackup.SetTestRoot(c.root))
			}
			if err := ArchivePush(c.bin, c.stanza, c.wal); err == nil {
				t.Fatal("archive-push accepted")
			}
			line := logged.String()
			if !strings.HasPrefix(line, "vaire archive-push: not archiving") || !strings.Contains(line, c.reason) {
				t.Fatalf("stderr %q lacks %q", line, c.reason)
			}
		})
	}
}

// archiveCommand is the archive_command 40-vaire-backup.conf sets.
func archiveCommand(t *testing.T, conf string) string {
	t.Helper()
	for _, line := range strings.Split(conf, "\n") {
		if v, ok := strings.CutPrefix(line, "archive_command = '"); ok {
			return strings.TrimSuffix(v, "'")
		}
	}
	t.Fatalf("no archive_command in:\n%s", conf)
	return ""
}

func TestArchiveCommandUsesTheWrapper(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Backup.Provider = "r2"
	spec := config.ClusterSpec{Name: "app", Stanza: "app-3f9a1c"}
	cmd := archiveCommand(t, string(render.BackupConf(cfg, spec)))
	if cmd != "/usr/local/bin/vaire node archive-push --pgbackrest=/usr/bin/pgbackrest --stanza=app-3f9a1c %p" {
		t.Fatalf("archive_command: %s", cmd)
	}
	// Regression: pgBackRest's check and backup refuse an archive_command
	// without "pgbackrest" in it (error 068, archive-command-invalid), so a
	// new stanza never got its first full backup.
	if !strings.Contains(cmd, "pgbackrest") {
		t.Fatalf("pgBackRest rejects archive_command %q: it must contain pgbackrest", cmd)
	}
	cfg.Backup.Provider = ""
	if archiveCommand(t, string(render.BackupConf(cfg, spec))) != "/bin/true" {
		t.Fatal("no bucket: archive_command isn't /bin/true")
	}
}
