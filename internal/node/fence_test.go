package node

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/rpc"
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

func TestArchivePushRefusesWhenFenced(t *testing.T) {
	if err := ArchivePush("bad stanza", "pg_wal/x"); err == nil {
		t.Fatal("an invalid stanza was accepted")
	}
	fenceForTest(t)
	err := ArchivePush("app-3f9a1c", "pg_wal/000000010000000000000001")
	var cli *output.CLIError
	if !errors.As(err, &cli) || cli.Code != output.CodeNodeFenced {
		t.Fatalf("fenced archive-push: %v", err)
	}
}

func TestArchiveCommandUsesTheWrapper(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Backup.Provider = "r2"
	conf := string(render.BackupConf(cfg, config.ClusterSpec{Name: "app", Stanza: "app-3f9a1c"}))
	if !strings.Contains(conf, "archive_command = '/usr/local/bin/eru node archive-push --stanza=app-3f9a1c %p'") {
		t.Fatalf("40-eru-backup.conf:\n%s", conf)
	}
	cfg.Backup.Provider = ""
	if !strings.Contains(string(render.BackupConf(cfg, config.ClusterSpec{Name: "app", Stanza: "app-3f9a1c"})), "'/bin/true'") {
		t.Fatal("no bucket: archive_command isn't /bin/true")
	}
}
