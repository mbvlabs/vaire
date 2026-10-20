package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestQRSquare(t *testing.T) {
	out, err := QR("otpauth://totp/vaire:prod?algorithm=SHA1&digits=6&issuer=vaire&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(ansi.Strip(out), "\n"), "\n")
	width := utf8.RuneCountInString(lines[0])
	// Two modules per line, so the lines are about half the width.
	if want := (width + 1) / 2; len(lines) != want {
		t.Fatalf("%d lines for width %d, want %d", len(lines), width, want)
	}
	for i, line := range lines {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("line %d is %d wide, want %d", i, utf8.RuneCountInString(line), width)
		}
	}
	// The quiet zone is light.
	if strings.TrimSpace(lines[0]) != "" {
		t.Fatalf("first line is not blank: %q", lines[0])
	}
}
