package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestWorkspaceIdentitySwitchKeepsCanonicalWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := workspaces.Save(&workspaces.Workspace{Name: "thelab", Server: "thelab", Operator: workspaces.Operator{Name: "owner"}}); err != nil {
		t.Fatal(err)
	}

	alice := &workspaces.Workspace{Name: "thelab", Server: "thelab", Operator: workspaces.Operator{Name: "alice"}}
	if err := workspaces.SaveIdentity("dev-alice", alice); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()

		var out bytes.Buffer

		cmd := NewRoot()
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{"-w", "thelab"}, args...))

		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}

		return out.String()
	}

	run("workspace", "operator", "use", "alice")

	if list := run("workspace", "operator", "list"); !strings.Contains(list, "*\talice") {
		t.Fatalf("Alice was not selected: %s", list)
	}

	if c, err := workspaces.Load("thelab"); err != nil || c.Name != "thelab" || c.Operator.Name != "alice" {
		t.Fatalf("not active as Alice in the canonical workspace: %+v %v", c, err)
	}

	run("workspace", "operator", "use", "owner")

	if c, err := workspaces.Load("thelab"); err != nil || c.Operator.Name != "owner" {
		t.Fatalf("couldn't switch back to owner: %+v %v", c, err)
	}
}

func TestWorkspaceOperatorSelectorUsesDeviceLabel(t *testing.T) {
	rows := []workspaceIdentityRow{
		{id: "dev-1", Operator: "alice", Device: "laptop"},
		{id: "dev-2", Operator: "alice", Device: "desktop"},
	}

	if _, err := selectWorkspaceIdentity(rows, "alice", ""); err == nil {
		t.Fatal("accepted an ambiguous operator")
	}

	if row, err := selectWorkspaceIdentity(rows, "alice", "laptop"); err != nil || row.id != "dev-1" {
		t.Fatalf("wrong device: %+v, %v", row, err)
	}
}
