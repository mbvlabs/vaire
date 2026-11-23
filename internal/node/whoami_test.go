package node

import (
	"os/user"
	"testing"
)

func TestLegacySSHAccount(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	if account.Uid == "0" {
		account, err = user.Lookup("nobody")
		if err != nil {
			t.Skip("no unprivileged account available")
		}
	}

	got, err := legacySSHAccount(account.Uid, account.Username)
	if err != nil || got != account.Username {
		t.Fatalf("recognized login = %q, %v; want %q", got, err, account.Username)
	}

	for _, uid := range []string{"", "0", "not-a-uid"} {
		if got, err := legacySSHAccount(uid, account.Username); got != "" || err == nil {
			t.Errorf("unrecognized UID %q: got %q, %v", uid, got, err)
		}
	}

	if got, err := legacySSHAccount(account.Uid, account.Username+"-other"); got != "" || err == nil {
		t.Errorf("different account: got %q, %v", got, err)
	}
}
