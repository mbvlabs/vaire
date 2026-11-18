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
// isolateAdoptRegistration neutralises the owner lookup and the writer's owner
// argument, so the test writes the real registration and adoption files as its
// own user.
func isolateAdoptRegistration(t *testing.T) {
	t.Helper()

	oldReservation, oldRunning := legacyReservationDir, materialisedDataRunning
	legacyReservationDir = t.TempDir()
	materialisedDataRunning = func(context.Context, string) bool { return false }

	t.Cleanup(func() { legacyReservationDir, materialisedDataRunning = oldReservation, oldRunning })

	oldConf, oldIDs, oldWrite := pgConfRoot, resolveClusterIDs, writeClusterFile
	pgConfRoot = t.TempDir()
	// Tests write as their own user; production chowns to postgres.
	resolveClusterIDs = func(_, _ string) (int, int, error) { return -1, -1, nil }
	writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
		return engine.WriteFileAtomicOwned(path, data, perm, "", "")
	}

	t.Cleanup(func() { pgConfRoot, resolveClusterIDs, writeClusterFile = oldConf, oldIDs, oldWrite })
}

// writeAdoptSource writes the source registration a partial adoption copies.
func writeAdoptSource(t *testing.T) {
	t.Helper()

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
}

// requireAdoptionInterrupted requires an adoption that dies writing file to
// leave the registration unfinished but unchanged.
func requireAdoptionInterrupted(t *testing.T, m *Materialised, confDir, file string) {
	t.Helper()

	writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
		if filepath.Base(path) == file {
			return errors.New("the process died writing " + file)
		}

		return engine.WriteFileAtomicOwned(path, data, perm, "", "")
	}
	defer func() {
		writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
			return engine.WriteFileAtomicOwned(path, data, perm, "", "")
		}
	}()

	if err := m.Adopt(context.Background(), 5439); err == nil {
		t.Fatalf("the interrupted adoption wrote %s without failing", file)
	}

	start, _ := os.ReadFile(filepath.Join(confDir, "start.conf"))
	if string(start) != "manual\n" {
		t.Fatalf("the interrupted adoption already finished start.conf: %q", start)
	}
}

// requireAdoptedRegistration requires a completed adoption's registration.
func requireAdoptedRegistration(t *testing.T, confDir, data string) {
	t.Helper()

	conf, err := os.ReadFile(filepath.Join(confDir, "postgresql.conf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(conf), "port = 5439") || !strings.Contains(string(conf), "data_directory = '"+data+"'") {
		t.Fatalf("the resumed adoption did not finish postgresql.conf:\n%s", conf)
	}

	start, err := os.ReadFile(filepath.Join(confDir, "start.conf"))
	if err != nil || string(start) != "auto\n" {
		t.Fatalf("the resumed adoption left start.conf %q (%v)", start, err)
	}

	if _, err := os.Stat(filepath.Join(confDir, "conf.d", MaterialiseOverrides)); !os.IsNotExist(err) {
		t.Fatalf("the temporary override survived adoption: %v", err)
	}

	if _, err := os.Stat(filepath.Join(confDir, "conf.d", UserOverrides)); err != nil {
		t.Fatalf("the operator's override was removed: %v", err)
	}
}

func TestAdoptFinishesAPartialRegistration(t *testing.T) {
	isolateAdoptRegistration(t)
	writeAdoptSource(t)

	data := filepath.Join(t.TempDir(), "app_follower")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	m := &Materialised{Name: "app_follower", Source: "app", Data: data}
	if err := m.register(); err != nil {
		t.Fatal(err)
	}

	confDir := m.ConfDir()

	start, _ := os.ReadFile(filepath.Join(confDir, "start.conf"))
	if string(start) != "manual\n" {
		t.Fatalf("registration start.conf %q", start)
	}

	for _, file := range []string{"postgresql.conf", "start.conf"} {
		t.Run(file, func(t *testing.T) {
			if err := m.register(); err != nil {
				t.Fatal(err)
			}

			requireAdoptionInterrupted(t, m, confDir, file)

			if err := m.Adopt(context.Background(), 5439); err != nil {
				t.Fatalf("the resumed adoption failed: %v", err)
			}

			requireAdoptedRegistration(t, confDir, data)
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

	for _, tt := range []struct {
		name, conf, want string
		wantErr          bool
	}{
		{"plain", "data_directory = '/var/lib/postgresql/18/app'\n", "/var/lib/postgresql/18/app", false},
		{"postgresql-common comment", "data_directory = '/var/lib/postgresql/18/app'\t\t# use data in another directory\n", "/var/lib/postgresql/18/app", false},
		{"quoted hash", "data_directory = '/srv/pg#data/app' # outside comment\n", "/srv/pg#data/app", false},
		{"doubled quote", "data_directory = '/srv/it''s/app' # comment\n", "/srv/it's/app", false},
		{"escaped quote", "data_directory = '/srv/it\\'s/app' # comment\n", "/srv/it's/app", false},
		{"bare", "data_directory = /srv/pg/app # comment\n", "/srv/pg/app", false},
		{"optional equals and case", " DATA_DIRECTORY '/srv/pg/app' # comment\n", "/srv/pg/app", false},
		{"commented only", "# data_directory = '/wrong'\ninclude_dir = 'conf.d'\n", "", false},
		{"last assignment", "data_directory = '/old'\ndata_directory = '/srv/pg/app' # current\n", "/srv/pg/app", false},
		{"unterminated", "data_directory = '/srv/pg/app\n", "", true},
		{"trailing text", "data_directory = '/srv/pg/app' unexpected\n", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte(tt.conf), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := ClusterDataDirectory("app")
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("registered cluster: got %q, %v; want %q, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// R2-1: a registration that made the configuration directory but died before
// postgresql.conf is a partial directory, not a cluster. The operation-owned
// cleanup removes it with the data.
func TestDropTemporaryClusterRemovesAPartialRegistration(t *testing.T) {
	oldReservation := legacyReservationDir
	legacyReservationDir = t.TempDir()
	t.Cleanup(func() { legacyReservationDir = oldReservation })

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
