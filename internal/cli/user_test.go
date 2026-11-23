package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestUserCommands(t *testing.T) {
	for _, args := range [][]string{{"user", "--help"}, {"user", "add", "--help"}, {"user", "list", "--help"}} {
		var out bytes.Buffer

		root := NewRoot()
		root.SetOut(&out)
		root.SetArgs(args)

		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}

		if !strings.Contains(out.String(), "user") {
			t.Fatalf("%v: %s", args, out.String())
		}
	}

	root := NewRoot()
	root.SetArgs([]string{"user", "add", "alice", "--role", "admin"})

	if err := root.Execute(); err == nil {
		t.Fatal("invalid role accepted")
	}
}
