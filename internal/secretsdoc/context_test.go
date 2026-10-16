package secretsdoc

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/password"
	"github.com/mbvlabs/eru/internal/transport"
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
		Server:         Server{Name: "prod"},
		Operator:       Operator{WGPrivateKey: private, SSHPrivateKey: string(sshPrivate)},
		Nodes:          []Node{{Name: "db1", Role: contexts.RoleMaster, WGAddress: "10.42.0.1", SSHHostKeys: []string{sshPublic}}},
		MasterPassword: hash,
	}
	for _, tc := range []struct{ name, hash string }{
		{"with-password", hash}, {"legacy", ""},
	} {
		doc.MasterPassword = tc.hash
		if _, err := Install(doc, tc.name, ""); err != nil {
			t.Fatal(err)
		}
		creds, err := contexts.LoadCredentials(tc.name)
		if err != nil || creds.MasterPasswordHash != tc.hash {
			t.Fatalf("installed hash differs: %v", err)
		}
	}
	doc.MasterPassword = strings.Replace(hash, "t=1", "t=0", 1)
	if _, err := Install(doc, "invalid", ""); err == nil {
		t.Fatal("imported a zero-round verifier")
	}
	if contexts.Exists("invalid") {
		t.Fatal("invalid import left a context behind")
	}
}
