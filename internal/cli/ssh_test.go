package cli

import (
	"bytes"
	"testing"
)

func TestSudoPasswordWatch(t *testing.T) {
	var out bytes.Buffer
	w := &sudoPasswordWatch{w: &out}
	msg := "sudo: a terminal is required to read the password; either use the -S option\n"
	// Split mid-message so the match has to span writes.
	for _, part := range []string{"some output\n", msg[:10], msg[10:]} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if !w.seen {
		t.Fatal("sudo's no-terminal message was not noticed")
	}
	if out.String() != "some output\n"+msg {
		t.Fatalf("stderr was not passed through: %q", out.String())
	}

	other := &sudoPasswordWatch{w: &bytes.Buffer{}}
	_, _ = other.Write([]byte("cat: /nope: No such file or directory\n"))
	if other.seen {
		t.Fatal("an unrelated error counted as sudo wanting a password")
	}
}
