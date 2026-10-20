package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func credentialsFixture(width int) string {
	return Credentials("created database app2 and its roles", "app2 (medium, 1 GiB)",
		[]CredentialRow{{Role: "migrate", User: "app2_migrate", Password: "Abc123"}, {Role: "app", User: "app2_app", Password: "Def456"}},
		[]URLRow{{Label: "app URL", URL: "postgres://app2_app:Def456@db1.example.com:6432/app2?sslmode=verify-full"}}, //nolint:gosec // G101: a test fixture.
		"Store these now: vaire does not keep database passwords. If they are lost, run vaire db credentials <db> --rotate --role <role>.", width)
}

func TestCredentialsURLsBetweenRules(t *testing.T) {
	out := ansi.Strip(credentialsFixture(120))
	if !strings.Contains(out, "╭") {
		t.Fatalf("expected a frame at 120 columns:\n%s", out)
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "postgres://") {
			found = true
			if strings.ContainsAny(line, "│╭╮╰╯") {
				t.Fatalf("URL line carries frame characters: %q", line)
			}
		}
	}
	if !found {
		t.Fatalf("no flush-left URL line:\n%s", out)
	}
}

func TestCredentialsNarrowDropsFrame(t *testing.T) {
	out := ansi.Strip(credentialsFixture(30))
	if strings.ContainsAny(out, "│╭╮╰╯") {
		t.Fatalf("frame drawn at 30 columns:\n%s", out)
	}
	// The note wraps to the width; the table rows and URLs never break.
	_, note, _ := strings.Cut(out, SymWarn+" ")
	for _, line := range strings.Split(SymWarn+" "+note, "\n") {
		if lipgloss.Width(line) > 30 {
			t.Fatalf("note line wider than 30 columns: %q", line)
		}
	}
}

func TestURLFrame(t *testing.T) {
	long := "postgres://app2_migrate:Abc123@db1.example.com:5432/app2?sslmode=verify-full&application_name=x" //nolint:gosec // G101: a test fixture.
	out := ansi.Strip(urlFrame([]URLRow{{Label: "app URL", URL: "postgres://a@h/app2"}, {Label: "migrate URL", URL: long}}, 60))
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "── Connection URLs ─") || !strings.HasPrefix(lines[len(lines)-1], "────") {
		t.Fatalf("expected a top and a bottom rule:\n%s", out)
	}
	want := []string{"app URL:", "postgres://a@h/app2", "", "migrate URL:", long}
	for i, w := range want {
		if lines[i+1] != w {
			t.Errorf("line %d = %q, want %q", i+1, lines[i+1], w)
		}
	}
	for _, line := range lines {
		if strings.HasSuffix(line, " ") || strings.ContainsAny(line, "│╭╮╰╯") {
			t.Errorf("line %q has a trailing space or a side border", line)
		}
		if strings.HasPrefix(line, "─") && lipgloss.Width(line) > 59 {
			t.Errorf("rule wider than the terminal: %d", lipgloss.Width(line))
		}
	}
}
