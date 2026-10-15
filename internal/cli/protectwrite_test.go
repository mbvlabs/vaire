package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
)

func statusOf(protected ...string) func() (protectStatusData, error) {
	return func() (protectStatusData, error) {
		return protectStatusData{Configured: true, Protected: protected}, nil
	}
}

func noNote(string) {}

// A write to a protected database without a person at a terminal is
// refused: no terminal (the tests have none), --yes, or structured output.
// An agent gets read-only access.
func TestProtectedWriteRefusedWithoutHuman(t *testing.T) {
	for _, path := range []string{"sql", "console", "psql", "maint kill"} {
		for _, args := range [][]string{nil, {"--yes"}, {"--output", "json"}, {"--no-input"}} {
			code, err := protectedWriteCode(commandAt(t, path, args...), "app", "writing to it", statusOf("app"), noNote)
			if code != nil {
				t.Fatalf("%s %v: got a code", path, args)
			}
			e := cliErrorOf(t, err)
			if e.Code != output.CodeDatabaseProtected || reasonOf(e) != "write_requires_human" || e.ExitCode != output.ExitUnsafe {
				t.Errorf("%s %v: %s %q (exit %d)", path, args, e.Code, reasonOf(e), e.ExitCode)
			}
			if !strings.Contains(e.Hint, "without --write") {
				t.Errorf("%s %v: the hint does not say reads need no code: %q", path, args, e.Hint)
			}
		}
	}
}

// An unprotected database needs no code, whoever runs the command.
func TestProtectedWriteUnprotectedNeedsNoCode(t *testing.T) {
	for _, args := range [][]string{nil, {"--output", "json"}} {
		code, err := protectedWriteCode(commandAt(t, "sql", args...), "other", "writing to it", statusOf("app"), noNote)
		if err != nil || code != nil {
			t.Fatalf("%v: %v %v", args, code, err)
		}
	}
	// A status that can't be read fails the write; it never skips the check.
	boom := errors.New("node unreachable")
	_, err := protectedWriteCode(commandAt(t, "sql"), "app", "writing to it",
		func() (protectStatusData, error) { return protectStatusData{}, boom }, noNote)
	if !errors.Is(err, boom) {
		t.Fatalf("status error: %v", err)
	}
}

// Without --write, sql, psql and console log in as eru_admin and never ask
// the node about protection: reads need no code.
func TestSessionLoginReadNeedsNoCode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	creds := contexts.Credentials{Database: &contexts.DatabaseCreds{Admin: contexts.Role{User: "eru_admin", Password: "admin-pw"}}}
	if err := contexts.SaveCredentials("prod", creds); err != nil {
		t.Fatal(err)
	}
	// No node and no progress: a read that reached for either would panic.
	s := &session{ctx: &contexts.Context{Name: "prod"}}
	user, password, err := sessionLogin(commandAt(t, "sql", "--output", "json"), s, nil, "app", false)
	if err != nil || user != "eru_admin" || password != "admin-pw" {
		t.Fatalf("read login: %s %s %v", user, password, err)
	}
}
