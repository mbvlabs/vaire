package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

// protectStatusWith is a protect.status result naming dbs, with the protected
// ones in Protected, as the node reports them.
func protectStatusWith(dbs ...node.ProtectedDatabase) (protectStatusData, error) {
	st := protectStatusData{ApprovalFactors: &node.ExposureFactorStatus{TOTP: true}, Protected: []string{}, Databases: dbs}
	for _, d := range dbs {
		if d.Protected {
			st.Protected = append(st.Protected, d.Name)
		}
	}

	return st, nil
}

// A write to any follower is refused with the node's is_follower error, its
// source and a way to write elsewhere or to make the follower independent:
// protected and unprotected alike. The source and a database the status does
// not know pass untouched, and a status that fails keeps its error.
func TestRefuseFollowerWrite(t *testing.T) {
	dbs := []node.ProtectedDatabase{
		{Name: "app"},
		{Name: "app_follower", Follows: "app"},
		{Name: "app_protected_follower", Protected: true, Follows: "app"},
	}

	for _, db := range []string{"app_follower", "app_protected_follower"} {
		_, err := refuseFollowerWrite(func() (protectStatusData, error) {
			return protectStatusWith(dbs...)
		}, db)()
		e := cliErrorOf(t, err)

		details, _ := e.Details.(map[string]any)
		if e.Code != output.CodePrecondition || e.ExitCode != output.ExitPrecondition || reasonOf(e) != "is_follower" {
			t.Errorf("%s: %s %q (exit %d)", db, e.Code, reasonOf(e), e.ExitCode)
		}

		if details["database"] != db || details["source"] != "app" {
			t.Errorf("%s: details %v, want is_follower from app", db, details)
		}

		if !strings.Contains(e.Message, db+" is a follower of app") {
			t.Errorf("%s: message %q", db, e.Message)
		}

		for _, want := range []string{"vaire db unfollow " + db + " --confirm " + db + "/unfollow", "Reads need no --write"} {
			if !strings.Contains(e.Hint, want) {
				t.Errorf("%s: hint %q does not mention %q", db, e.Hint, want)
			}
		}
	}

	for _, db := range []string{"app", "unknown"} {
		st, err := refuseFollowerWrite(func() (protectStatusData, error) {
			return protectStatusWith(dbs...)
		}, db)()
		if err != nil || !hasApprovalFactor(st.ApprovalFactors) || len(st.Databases) != len(dbs) ||
			len(st.Protected) != 1 || st.Protected[0] != "app_protected_follower" {
			t.Errorf("%s: got %+v, %v; want the status unchanged", db, st, err)
		}
	}

	boom := errors.New("node unreachable")

	_, err := refuseFollowerWrite(func() (protectStatusData, error) {
		return protectStatusData{}, boom
	}, "app_follower")()
	if !errors.Is(err, boom) {
		t.Errorf("status error: %v", err)
	}
}

// A protected follower is refused as a follower before protectedWriteCode's
// own checks: no write_requires_human and no code prompt, whatever the
// invocation mode. An unprotected follower is refused the same way, and a
// protected source keeps its existing refusal.
func TestProtectedWriteFollowerRefusedBeforeCode(t *testing.T) {
	status := func() (protectStatusData, error) {
		return protectStatusWith(
			node.ProtectedDatabase{Name: "app", Protected: true},
			node.ProtectedDatabase{Name: "app_follower", Follows: "app"},
			node.ProtectedDatabase{Name: "app_protected_follower", Protected: true, Follows: "app"},
		)
	}

	for _, db := range []string{"app_follower", "app_protected_follower"} {
		for _, args := range [][]string{nil, {"--yes"}, {"--output", "json"}, {"--no-input"}} {
			code, err := protectedWriteCode(commandAt(t, "sql", args...), db, "writing to it",
				refuseFollowerWrite(status, db), func(string) { t.Error("a follower was asked for a code") })
			if code != nil {
				t.Fatalf("%s %v: got a code", db, args)
			}

			e := cliErrorOf(t, err)
			if e.Code != output.CodePrecondition || reasonOf(e) != "is_follower" {
				t.Errorf("%s %v: %s %q, want is_follower", db, args, e.Code, reasonOf(e))
			}
		}
	}

	_, err := protectedWriteCode(commandAt(t, "sql", "--output", "json"), "app", "writing to it",
		refuseFollowerWrite(status, "app"), noNote)
	if e := cliErrorOf(t, err); e.Code != output.CodeDatabaseProtected || reasonOf(e) != "write_requires_human" {
		t.Errorf("the protected source: %s %q", e.Code, reasonOf(e))
	}
}

// A protect.status payload as the node sends it names an unprotected follower
// in databases, with its source in follows: that is the write the live bug
// let through to SET ROLE.
func TestRefuseFollowerWriteFromProtectStatusJSON(t *testing.T) {
	reply := remote.Reply{Data: json.RawMessage(`{"configured":true,"protected_databases":["app"],"databases":[{"name":"app","protected":true},{"name":"app_follower","follows":"app"}]}`)}

	var st protectStatusData
	if err := reply.Decode(&st); err != nil {
		t.Fatal(err)
	}

	_, err := refuseFollowerWrite(func() (protectStatusData, error) { return st, nil }, "app_follower")()

	e := cliErrorOf(t, err)
	if e.Code != output.CodePrecondition || reasonOf(e) != "is_follower" {
		t.Fatalf("the node's protect.status: %s %q", e.Code, reasonOf(e))
	}
}

// A --write session on a follower is refused in sessionLogin, the login vaire
// sql, psql and console share, before it asks for a code or calls
// db.write-access: s.node is nil, so the call would panic. An ordinary write
// still logs in as vaire_admin, and a read never asks protect.status.
func TestSessionLoginWriteRefusesFollower(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	creds := workspaces.Credentials{Database: &workspaces.DatabaseCreds{Admin: workspaces.Role{User: "vaire_admin", Password: "admin-pw"}}}
	if err := workspaces.SaveCredentials("prod", creds); err != nil {
		t.Fatal(err)
	}

	s := &session{ctx: &workspaces.Workspace{Name: "prod"}}
	old := fetchWriteProtectStatus

	t.Cleanup(func() { fetchWriteProtectStatus = old })

	fetchWriteProtectStatus = func(*cobra.Command, *session, *progress) (protectStatusData, error) {
		return protectStatusWith(
			node.ProtectedDatabase{Name: "app"},
			node.ProtectedDatabase{Name: "app_follower", Follows: "app"},
			node.ProtectedDatabase{Name: "app_protected_follower", Protected: true, Follows: "app"},
		)
	}

	for _, c := range []struct {
		db   string
		args []string
	}{
		{"app_follower", nil},
		{"app_follower", []string{"--output", "json"}},
		{"app_protected_follower", nil},
		{"app_protected_follower", []string{"--yes"}},
	} {
		_, _, err := sessionLogin(commandAt(t, "sql", c.args...), s, &progress{}, c.db, true)

		e := cliErrorOf(t, err)
		if e.Code != output.CodePrecondition || reasonOf(e) != "is_follower" {
			t.Errorf("%s %v: %s %q, want is_follower", c.db, c.args, e.Code, reasonOf(e))
		}
	}

	// An ordinary write logs in as vaire_admin, with no write code.
	fetchWriteProtectStatus = func(*cobra.Command, *session, *progress) (protectStatusData, error) {
		return protectStatusWith(node.ProtectedDatabase{Name: "app"})
	}

	user, password, err := sessionLogin(commandAt(t, "sql", "--write"), s, &progress{}, "app", true)
	if err != nil || user != "vaire_admin" || password != "admin-pw" {
		t.Fatalf("ordinary write: %s %s %v", user, password, err)
	}

	// A read never asks the node about protection, even on a follower.
	fetchWriteProtectStatus = func(*cobra.Command, *session, *progress) (protectStatusData, error) {
		t.Error("a read asked protect.status")
		return protectStatusData{}, errors.New("asked")
	}

	user, password, err = sessionLogin(commandAt(t, "sql"), s, nil, "app_follower", false)
	if err != nil || user != "vaire_admin" || password != "admin-pw" {
		t.Fatalf("read login: %s %s %v", user, password, err)
	}
}
