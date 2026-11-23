package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestWhoamiRequiresNodeConnection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// A saved profile alone must never be enough to answer whoami.
	if err := workspaces.Save(&workspaces.Workspace{
		Name: "prod", Server: "prod", Operator: workspaces.Operator{Name: "claimed-owner"},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"-w", "prod", "whoami", "--json"})

	if err := root.Execute(); err == nil {
		t.Fatalf("whoami returned success without a node connection: %s", &out)
	}

	if out.Len() != 0 {
		t.Fatalf("unexpected identity output: %s", &out)
	}
}

func TestWhoamiShowsVerifiedDeviceOnlyForThatReply(t *testing.T) {
	identity := rpc.WhoamiReply{
		Server: "prod", Node: "db1", Operator: "alice", Device: "work-laptop", Role: "member",
		IdentityKind: rpc.IdentityVerifiedDevice,
	}

	got := whoamiMessage("prod", identity)
	if !strings.Contains(got, "alice (member)") || !strings.Contains(got, "work-laptop") || !strings.Contains(got, "verified by db1") {
		t.Fatalf("verified device not shown: %s", got)
	}

	identity.IdentityKind = rpc.IdentityLegacyShared

	got = whoamiMessage("prod", identity)
	if strings.Contains(got, "alice") || strings.Contains(got, "work-laptop") || !strings.Contains(got, "not individually verified") {
		t.Fatalf("legacy login claims a device identity: %s", got)
	}
}
