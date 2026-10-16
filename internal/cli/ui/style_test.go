package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestErrorBlockWrapsToWidth checks that a long message and hint wrap inside
// the border instead of pushing it past the terminal's width.
func TestErrorBlockWrapsToWidth(t *testing.T) {
	block := ErrorBlock{
		Code:    "precondition_failed",
		Message: strings.Repeat("the database cannot be repacked because the extension is missing ", 3),
		Hint:    strings.Repeat("run eru ext add pg_repack --db app and try again ", 3),
	}
	for _, width := range []int{40, 60, 80} {
		out := block.Render(width)
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			if w := lipgloss.Width(line); w > width {
				t.Fatalf("width %d: line is %d columns wide: %q", width, w, ansi.Strip(line))
			}
		}
		first, last := ansi.Strip(lines[0]), ansi.Strip(lines[len(lines)-1])
		if !strings.HasPrefix(first, "╭") || !strings.HasPrefix(last, "╰") {
			t.Fatalf("width %d: border missing:\n%s", width, ansi.Strip(out))
		}
		plain := strings.Join(strings.Fields(ansi.Strip(out)), " ")
		if !strings.Contains(plain, "precondition_failed") {
			t.Fatalf("width %d: code lost:\n%s", width, ansi.Strip(out))
		}
	}
}

// TestErrorBlockShortStaysUnchanged checks that a box that already fits is
// drawn as is.
func TestErrorBlockShortStaysUnchanged(t *testing.T) {
	block := ErrorBlock{Code: "usage_error", Message: "bad flag"}
	if got, want := block.Render(80), styleErrorBox.Render(styleFail.Render(SymFail+" bad flag")+"  "+styleDim.Render("[usage_error]")); got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

// TestErrorBlockNarrowDropsFrame checks that below the narrowest frame the
// content is printed without a border rather than a broken one.
func TestErrorBlockNarrowDropsFrame(t *testing.T) {
	block := ErrorBlock{Code: "usage_error", Message: strings.Repeat("x", 40)}
	if out := ansi.Strip(block.Render(minPanelWidth - 1)); strings.Contains(out, "╭") {
		t.Fatalf("narrow render kept the frame:\n%s", out)
	}
}
