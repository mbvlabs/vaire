package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
)

// followCleanupRegistrationFixture is a valid owned follower registration for
// followCleanupRegistration to verify before it asks postgresql-common.
type followCleanupRegistrationFixture struct {
	dataRoot, dataPath string
	confRoot, confPath string
	name, source       string
	port               int
}

func newFollowCleanupRegistrationFixture(t *testing.T) *followCleanupRegistrationFixture {
	t.Helper()
	root := t.TempDir()
	f := &followCleanupRegistrationFixture{
		dataRoot: filepath.Join(root, "data"),
		confRoot: filepath.Join(root, "postgresql", "18"),
		name:     "app_follower",
		source:   "app",
		port:     5434,
	}
	f.dataPath = filepath.Join(f.dataRoot, f.name)

	confDir := filepath.Join(f.confRoot, f.name)
	for _, dir := range []string{f.dataPath, confDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Chmod(confDir, 0o755); err != nil {
		t.Fatal(err)
	}

	f.confPath = filepath.Join(confDir, "postgresql.conf")
	f.writeRegistration(t, "18/"+f.name)
	t.Cleanup(backup.PGConfRootForTest(f.confRoot))

	return f
}

func (f *followCleanupRegistrationFixture) writeRegistration(t *testing.T, clusterName string) {
	t.Helper()

	raw := fmt.Sprintf("data_directory = '%s'\ncluster_name = '%s'\nport = %d\n", f.dataPath, clusterName, f.port)
	if err := os.WriteFile(f.confPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakePgConftoolSetting is one setting's short output and exit status.
type fakePgConftoolSetting struct {
	value  string
	status int
}

// fakePgConftoolSettings puts a pg_conftool on PATH that records its arguments
// and behaves like postgresql-common's: -s prints only the setting's value, the
// default prints the labelled pair. A setting's nonzero status fails after
// printing.
func fakePgConftoolSettings(t *testing.T, settings map[string]fakePgConftoolSetting) func() string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "pg_conftool.args")

	var cases strings.Builder
	for setting, s := range settings {
		fmt.Fprintf(&cases, "%s) value='%s' status=%d ;;\n", setting, s.value, s.status)
	}

	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" > '%s'
if [ "$1" = "-s" ]; then
	setting="$5"
else
	setting="$4"
fi
value=''
status=0
case "$setting" in
%s
esac
if [ "$1" = "-s" ]; then
	printf '%%s\n' "$value"
else
	printf "%%s = '%%s'\n" "$setting" "$value"
fi
exit "$status"
`, marker, cases.String())
	if err := os.WriteFile(filepath.Join(dir, "pg_conftool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)

	return func() string {
		raw, err := os.ReadFile(marker)
		if err != nil {
			return ""
		}

		return strings.TrimSpace(string(raw))
	}
}

// fakePgConftool is fakePgConftoolSettings for the data directory alone.
func fakePgConftool(t *testing.T, value string, status int) func() string {
	t.Helper()

	return fakePgConftoolSettings(t, map[string]fakePgConftoolSetting{
		"data_directory": {value: value, status: status},
	})
}

// QF06: the effective data directory is pg_conftool's short output; the
// labelled default never equals the path.
func TestFollowCleanupRegistrationUsesShortConftoolOutput(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)

	args := fakePgConftool(t, f.dataPath, 0)
	if err := followCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err != nil {
		t.Fatalf("registration with an exact effective data directory: %v", err)
	}

	if got, want := args(), "-s 18 "+f.name+" show data_directory"; got != want {
		t.Fatalf("pg_conftool args = %q, want %q", got, want)
	}
}

func TestFollowCleanupRegistrationRefusesMismatchedDataDirectory(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)
	fakePgConftool(t, f.dataPath+"-other", 0)

	if err := followCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err == nil ||
		err.Error() != "effective follower data directory is unverifiable" {
		t.Fatalf("mismatched effective data directory: err = %v", err)
	}
}

func TestFollowCleanupRegistrationRefusesUnreadableDataDirectory(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)
	fakePgConftool(t, f.dataPath, 1)

	if err := followCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err == nil ||
		err.Error() != "effective follower data directory is unverifiable" {
		t.Fatalf("failed pg_conftool: err = %v", err)
	}
}

// The ownership guard runs before postgresql-common is asked anything.
func TestFollowCleanupRegistrationVerifiesOwnershipBeforeConftool(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)
	f.writeRegistration(t, "18/other")

	args := fakePgConftool(t, f.dataPath, 0)
	if err := followCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err == nil {
		t.Fatal("a foreign registration was accepted")
	}

	if got := args(); got != "" {
		t.Fatalf("pg_conftool ran before the ownership check: %q", got)
	}
}
