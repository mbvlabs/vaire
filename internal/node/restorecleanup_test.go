package node

import (
	"strconv"
	"testing"
)

// The effective restore port is pg_conftool's short output; the labelled
// default never equals the number.
func TestRestoreCleanupRegistrationUsesShortConftoolPort(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)
	args := fakePgConftoolSettings(t, map[string]fakePgConftoolSetting{
		"data_directory": {value: f.dataPath},
		"port":           {value: strconv.Itoa(f.port)},
	})
	if err := restoreCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err != nil {
		t.Fatalf("registration with an exact effective port: %v", err)
	}
	if got, want := args(), "-s 18 "+f.name+" show port"; got != want {
		t.Fatalf("pg_conftool args = %q, want %q", got, want)
	}
}

func TestRestoreCleanupRegistrationRefusesMismatchedPort(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)
	fakePgConftoolSettings(t, map[string]fakePgConftoolSetting{
		"data_directory": {value: f.dataPath},
		"port":           {value: strconv.Itoa(f.port + 1)},
	})
	if err := restoreCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err == nil ||
		err.Error() != "effective restore port is unverifiable" {
		t.Fatalf("mismatched effective port: err = %v", err)
	}
}

func TestRestoreCleanupRegistrationRefusesUnreadablePort(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)
	fakePgConftoolSettings(t, map[string]fakePgConftoolSetting{
		"data_directory": {value: f.dataPath},
		"port":           {value: strconv.Itoa(f.port), status: 1},
	})
	if err := restoreCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err == nil ||
		err.Error() != "effective restore port is unverifiable" {
		t.Fatalf("failed pg_conftool: err = %v", err)
	}
}

// The follower ownership guard runs before postgresql-common is asked anything.
func TestRestoreCleanupRegistrationVerifiesFollowerBeforeConftool(t *testing.T) {
	f := newFollowCleanupRegistrationFixture(t)
	f.writeRegistration(t, "18/other")
	args := fakePgConftoolSettings(t, map[string]fakePgConftoolSetting{
		"data_directory": {value: f.dataPath},
		"port":           {value: strconv.Itoa(f.port)},
	})
	if err := restoreCleanupRegistration(f.name, f.source, f.dataRoot, f.port-1, f.port); err == nil {
		t.Fatal("a foreign follower registration was accepted")
	}
	if got := args(); got != "" {
		t.Fatalf("pg_conftool ran before the ownership check: %q", got)
	}
}
