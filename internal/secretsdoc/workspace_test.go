package secretsdoc

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/password"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestInstallMasterPasswordHash(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	private, _, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}
	sshPrivate, sshPublic, err := transport.GenerateSSHKey("test")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := password.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{
		Server:   Server{Name: "prod"},
		Operator: Operator{WGPrivateKey: private, SSHPrivateKey: string(sshPrivate)},
		Nodes: []Node{
			{Name: "db1", Role: workspaces.RoleMaster, WGAddress: "10.42.0.1", SSHHostKeys: []string{sshPublic}},
			{Name: "db2", Role: "worker", WGAddress: "10.42.0.2", SSHHostKeys: []string{sshPublic}},
		},
		MasterPassword: hash,
	}
	for _, tc := range []struct{ name, hash string }{
		{"with-password", hash}, {"legacy", ""},
	} {
		doc.MasterPassword = tc.hash
		workspace, err := Install(doc, tc.name, "")
		if err != nil {
			t.Fatal(err)
		}
		if workspace.Name != tc.name || workspace.Server != "prod" || len(workspace.Nodes) != 2 || workspace.Nodes[0].Name != "db1" || workspace.Nodes[1].Name != "db2" {
			t.Fatalf("workspace/node identities were conflated: %+v", workspace)
		}
		creds, err := workspaces.LoadCredentials(tc.name)
		if err != nil || creds.MasterPasswordHash != tc.hash {
			t.Fatalf("installed hash differs: %v", err)
		}
	}
	doc.MasterPassword = strings.Replace(hash, "t=1", "t=0", 1)
	if _, err := Install(doc, "invalid", ""); err == nil {
		t.Fatal("imported a zero-round verifier")
	}
	if workspaces.Exists("invalid") {
		t.Fatal("invalid import left a workspace behind")
	}
}
