package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/engine"
)

// R2-1: Adopt rewrites a temporary cluster's registration in several files.
// A run stopped after postgresql.conf, before start.conf, leaves the same
// deterministic cluster the next attempt adopts again; Adopt is idempotent, so
// a resumed operation finishes the partial registration instead of dropping the
// copy. The real registration and adoption files are written here; only their
// owner lookup and the writer's owner argument are neutralised for the test.
func TestAdoptFinishesAPartialRegistration(t *testing.T) {
	oldConf, oldIDs, oldWrite := pgConfRoot, resolveClusterIDs, writeClusterFile
	pgConfRoot = t.TempDir()
	// Tests write as their own user; production chowns to postgres.
	resolveClusterIDs = func(_, _ string) (int, int, error) { return -1, -1, nil }
	t.Cleanup(func() { pgConfRoot, resolveClusterIDs, writeClusterFile = oldConf, oldIDs, oldWrite })
	writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
		return engine.WriteFileAtomicOwned(path, data, perm, "", "")
	}

	source := filepath.Join(pgConfRoot, "app")
	if err := os.MkdirAll(filepath.Join(source, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "postgresql.conf"), []byte(strings.Join([]string{
		"data_directory = '/var/lib/postgresql/18/app'",
		"hba_file = '/etc/postgresql/18/app/pg_hba.conf'",
		"port = 5433",
		"cluster_name = '18/app'",
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "conf.d", UserOverrides), []byte("# operator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "app_follower")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Materialised{Name: "app_follower", Source: "app", Data: data}
	if err := m.register(); err != nil {
		t.Fatal(err)
	}
	confDir := m.ConfDir()
	start, _ := os.ReadFile(filepath.Join(confDir, "start.conf")) //nolint:gosec // G304: the test's own registration.
	if string(start) != "manual\n" {
		t.Fatalf("registration start.conf %q", start)
	}

	for _, file := range []string{"postgresql.conf", "start.conf"} {
		t.Run(file, func(t *testing.T) {
			if err := m.register(); err != nil {
				t.Fatal(err)
			}
			writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
				if filepath.Base(path) == file {
					return errors.New("the process died writing " + file)
				}
				return engine.WriteFileAtomicOwned(path, data, perm, "", "")
			}
			if err := m.Adopt(context.Background(), 5439); err == nil {
				t.Fatalf("the interrupted adoption wrote %s without failing", file)
			}
			start, _ := os.ReadFile(filepath.Join(confDir, "start.conf")) //nolint:gosec // G304: the test's own registration.
			if string(start) != "manual\n" {
				t.Fatalf("the interrupted adoption already finished start.conf: %q", start)
			}
			writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
				return engine.WriteFileAtomicOwned(path, data, perm, "", "")
			}
			if err := m.Adopt(context.Background(), 5439); err != nil {
				t.Fatalf("the resumed adoption failed: %v", err)
			}
			conf, err := os.ReadFile(filepath.Join(confDir, "postgresql.conf")) //nolint:gosec // G304: the test's own registration.
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(conf), "port = 5439") || !strings.Contains(string(conf), "data_directory = '"+data+"'") {
				t.Fatalf("the resumed adoption did not finish postgresql.conf:\n%s", conf)
			}
			start, _ = os.ReadFile(filepath.Join(confDir, "start.conf")) //nolint:gosec // G304: the test's own registration.
			if string(start) != "auto\n" {
				t.Fatalf("the resumed adoption left start.conf %q", start)
			}
			if _, err := os.Stat(filepath.Join(confDir, "conf.d", MaterialiseOverrides)); !os.IsNotExist(err) {
				t.Fatalf("the temporary override survived adoption: %v", err)
			}
			if _, err := os.Stat(filepath.Join(confDir, "conf.d", UserOverrides)); err != nil {
				t.Fatalf("the operator's override was removed: %v", err)
			}

			// A crash after a complete adoption, before the caller's marker,
			// runs the same adoption again on an already adopted registration.
			if err := m.Adopt(context.Background(), 5439); err != nil {
				t.Fatalf("a repeated adoption failed: %v", err)
			}
		})
	}
}

// R2-1: the registered data directory is the ownership evidence a resumed
// adoption checks; a registration for another data directory is not ours.
func TestClusterDataDirectory(t *testing.T) {
	oldConf := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldConf })
	dir := filepath.Join(pgConfRoot, "app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := ClusterDataDirectory("app"); err != nil || got != "" {
		t.Fatalf("unregistered cluster: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte("data_directory = '/var/lib/postgresql/18/app'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ClusterDataDirectory("app"); err != nil || got != "/var/lib/postgresql/18/app" {
		t.Fatalf("registered cluster: %q %v", got, err)
	}
}

// R2-1: a registration that made the configuration directory but died before
// postgresql.conf is a partial directory, not a cluster. The operation-owned
// cleanup removes it with the data.
func TestDropTemporaryClusterRemovesAPartialRegistration(t *testing.T) {
	oldConf := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldConf })
	data := filepath.Join(t.TempDir(), "app_follower")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(pgConfRoot, "app_follower")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "start.conf"), []byte("manual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := ClusterConfigDirectory("app_follower"); err != nil || !present {
		t.Fatalf("a partial registration has no configuration directory: %v %v", present, err)
	}
	if ClusterRegistered("app_follower") {
		t.Fatal("a partial registration counts as registered")
	}

	if err := DropTemporaryCluster(engine.Shell{}, "", nil, "app_follower", data); err != nil {
		t.Fatalf("the partial registration was not removed: %v", err)
	}
	for _, path := range []string{conf, data} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived the cleanup: %v", path, err)
		}
	}
	if present, err := ClusterConfigDirectory("app_follower"); err != nil || present {
		t.Fatalf("the removed registration still has a directory: %v %v", present, err)
	}
}
