package cli

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

func TestSessionRoleValidation(t *testing.T) {
	for _, command := range []string{"sql", "psql", "console"} {
		t.Run(command, func(t *testing.T) {
			if role, err := sessionRole(commandAt(t, command)); err != nil || role != "" {
				t.Fatalf("default: %q, %v", role, err)
			}

			for _, value := range []string{"", "owner", "RO", "bad"} {
				_, err := sessionRole(commandAt(t, command, "--role", value))
				if e := cliErrorOf(t, err); e.Code != output.CodeUsage {
					t.Errorf("role %q: %v", value, err)
				}
			}

			testSessionRolePromptRestrictions(t, command)
		})
	}
}

func testSessionRolePromptRestrictions(t *testing.T, command string) {
	t.Helper()

	for _, value := range []string{"ro", "app", "migrate"} {
		_, err := sessionRole(commandAt(t, command, "--role", value, "--write"))
		if e := cliErrorOf(t, err); e.Code != output.CodeUsage {
			t.Errorf("role %q with --write: %v", value, err)
		}

		_, err = sessionRole(commandAt(t, command, "--role", value, "--no-input"))
		if e := cliErrorOf(t, err); e.Code != output.CodeInputRequired {
			t.Errorf("role %q without prompt: %v", value, err)
		}
	}
}

func TestRoleSessionSetup(t *testing.T) {
	for _, role := range []string{"ro", "app", "migrate"} {
		if got := databaseRoleUser("app", role); got != "app_"+role {
			t.Errorf("%s: login %q", role, got)
		}

		if got := databaseRoleUser("app__restoring", role); got != "app_"+role {
			t.Errorf("%s restore: login %q", role, got)
		}

		setup, begin := sqlSessionSetup("app", role, false)
		if setup != "" || begin != "BEGIN" || loginOptions("app", role, false) != "" {
			t.Errorf("%s: setup %q, begin %q, options %q", role, setup, begin, loginOptions("app", role, false))
		}
	}

	setup, begin := sqlSessionSetup("app", "", false)
	if setup != "SET default_transaction_read_only = on" || begin != "BEGIN READ ONLY" || loginOptions("app", "", false) != "--default_transaction_read_only=on" {
		t.Fatal("admin read behavior changed")
	}

	setup, begin = sqlSessionSetup("app", "", true)
	if setup != `SET ROLE "app_owner"` || begin != "BEGIN READ WRITE" || loginOptions("app", "", true) != "--role=app_owner" {
		t.Fatal("admin write behavior changed")
	}
}
