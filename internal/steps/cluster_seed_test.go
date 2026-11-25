package steps

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
)

func seedFixture(t *testing.T) (string, map[string]string) {
	t.Helper()

	old := clusterSeedDir
	clusterSeedDir = filepath.Join(t.TempDir(), "runtime")
	t.Cleanup(func() { clusterSeedDir = old })

	return clusterSeedDir, map[string]string{
		SecretAdminPassword: "admin-example", SecretPgbouncerAuth: "auth-example", SecretMonitor: "monitor-example",
	}
}

func TestClusterSeedFirstClusterAndRetry(t *testing.T) {
	dir, values := seedFixture(t)
	if err := SeedClusterCredentials(values); err != nil {
		t.Fatal(err)
	}

	if err := CheckClusterSeed(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, clusterSeedName))
	if err != nil {
		t.Fatal(err)
	}

	for _, password := range values {
		if strings.Contains(string(raw), password) {
			t.Fatal("seed contains plaintext password")
		}
	}

	for role, key := range map[string]string{"vaire_admin": SecretAdminPassword, "pgbouncer_auth": SecretPgbouncerAuth, "vaire_monitor": SecretMonitor} {
		verifier, err := serviceVerifier(context.Background(), config.Cluster{}, config.ClusterSpec{Name: "first"}, engine.Runtime{}, role, key)
		if err != nil || !pg.VerifySCRAM(verifier, values[key]) {
			t.Fatalf("%s verifier unavailable: %v", role, err)
		}
	}

	sql, err := clusterRolesSQL(context.Background(), config.Cluster{}, config.ClusterSpec{Name: "first"}, engine.Runtime{}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}

	seed, err := readClusterSeed()
	if err != nil {
		t.Fatal(err)
	}

	for _, verifier := range []string{seed.Admin, seed.Auth, seed.Monitor} {
		if !strings.Contains(sql, verifier) {
			t.Fatal("first cluster role SQL did not use the seeded verifier")
		}
	}

	if err := SeedClusterCredentials(values); err != nil {
		t.Fatal(err)
	}

	changed := maps.Clone(values)
	changed[SecretAdminPassword] = "different"

	if err := SeedClusterCredentials(changed); err == nil {
		t.Fatal("accepted different bootstrap credentials")
	}

	after, err := os.ReadFile(filepath.Join(dir, clusterSeedName))
	if err != nil || string(after) != string(raw) {
		t.Fatal("retry replaced the seed")
	}
}

func TestClusterSeedFailsClosed(t *testing.T) {
	for _, damage := range []string{"missing", "bad-json", "bad-verifier", "permissions", "symlink", "service-mismatch", "missing-service"} {
		t.Run(damage, func(t *testing.T) {
			dir, values := seedFixture(t)
			if err := SeedClusterCredentials(values); err != nil {
				t.Fatal(err)
			}

			seedPath, authPath := filepath.Join(dir, clusterSeedName), filepath.Join(dir, "pgbouncer_auth.pass")

			switch damage {
			case "missing":
				_ = os.Remove(seedPath)
			case "bad-json":
				_ = os.WriteFile(seedPath, []byte("{"), 0o600)
			case "bad-verifier":
				_ = os.WriteFile(seedPath, []byte(`{"admin":"bad","pgbouncer_auth":"bad","monitor":"bad"}`), 0o600)
			case "permissions":
				_ = os.Chmod(seedPath, 0o644)
			case "symlink":
				_ = os.Remove(seedPath)
				_ = os.Symlink(authPath, seedPath)
			case "service-mismatch":
				_ = os.WriteFile(authPath, []byte("wrong\n"), 0o600)
			case "missing-service":
				_ = os.Remove(authPath)
			}

			if err := CheckClusterSeed(); err == nil {
				t.Fatal("accepted damaged credential state")
			}
		})
	}
}

func TestClusterSeedInterruptedBootstrap(t *testing.T) {
	dir, values := seedFixture(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "pgbouncer_auth.pass")
	if err := os.WriteFile(path, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SeedClusterCredentials(values); err == nil {
		t.Fatal("replaced an existing service password")
	}

	raw, _ := os.ReadFile(path)
	if string(raw) != "different\n" {
		t.Fatal("existing service password was modified")
	}

	if _, err := os.Stat(filepath.Join(dir, clusterSeedName)); !os.IsNotExist(err) {
		t.Fatal("created seed for inconsistent state")
	}
}

func TestRestoreClusterCredentials(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-admin", "wrong-auth", "wrong-monitor", "missing", "invalid", "old-bundle"} {
		t.Run(scenario, func(t *testing.T) {
			dir, values := restoreSeedFixture(t, scenario)
			err := RestoreClusterCredentials(values)
			wantSuccess := scenario == "valid" || scenario == "old-bundle"

			if (err == nil) != wantSuccess {
				t.Fatalf("restore error: %v", err)
			}

			checkRestoreSeed(t, scenario, dir, values)
		})
	}
}

func restoreSeedFixture(t *testing.T, scenario string) (string, map[string]string) {
	t.Helper()

	dir, values := seedFixture(t)
	if err := SeedClusterCredentials(values); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"pgbouncer_auth.pass", "monitor.pass"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	seed := filepath.Join(dir, clusterSeedName)

	switch scenario {
	case "wrong-admin":
		values[SecretAdminPassword] = "wrong"
	case "wrong-auth":
		values[SecretPgbouncerAuth] = "wrong"
	case "wrong-monitor":
		values[SecretMonitor] = "wrong"
	case "missing":
		delete(values, SecretMonitor)
	case "invalid":
		if err := os.WriteFile(seed, []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "old-bundle":
		if err := os.Remove(seed); err != nil {
			t.Fatal(err)
		}
	}

	return dir, values
}

func checkRestoreSeed(t *testing.T, scenario, dir string, values map[string]string) {
	t.Helper()

	if scenario == "valid" {
		if err := CheckClusterSeed(); err != nil {
			t.Fatal(err)
		}

		if err := RestoreClusterCredentials(values); err != nil {
			t.Fatalf("retry: %v", err)
		}

		return
	}

	for _, name := range []string{"pgbouncer_auth.pass", "monitor.pass"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected runtime credential %s: %v", name, err)
		}
	}

	if scenario == "old-bundle" && CheckClusterSeed() == nil {
		t.Fatal("old bundle admitted member create")
	}
}
