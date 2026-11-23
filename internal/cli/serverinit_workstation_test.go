package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestServerInitRejectsLocal(t *testing.T) {
	for _, args := range [][]string{
		{"server", "init", "--local"},
		{"server", "init", "--restore", "--local"},
	} {
		root := NewRoot()
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})

		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "unknown flag: --local") {
			t.Fatalf("%v: expected unknown --local flag, got %v", args, err)
		}
	}
}

func TestServerInitHelpIsWorkstationOnly(t *testing.T) {
	var out bytes.Buffer

	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"server", "init", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out.String(), "--local") {
		t.Fatalf("help advertises --local:\n%s", out.String())
	}
}
