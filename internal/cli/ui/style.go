// Package ui is eru's terminal presentation for humans: styled headers,
// panels, spinners, progress bars and huh prompts. It is used only when
// stderr is a terminal and the output mode is human; every other mode keeps
// plain lines.
package ui

import (
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// The palette uses the 16 ANSI colors, so it follows the terminal's own
// light or dark theme.
var (
	colorAccent = lipgloss.Color("5") // magenta
	colorOK     = lipgloss.Color("2")
	colorFail   = lipgloss.Color("1")
	colorWarn   = lipgloss.Color("3")
	colorInfo   = lipgloss.Color("6")
	colorDim    = lipgloss.Color("8")
)

var (
	styleHeader  = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleRule    = lipgloss.NewStyle().Foreground(colorDim)
	styleTitle   = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(colorDim)
	styleOK      = lipgloss.NewStyle().Foreground(colorOK).Bold(true)
	styleFail    = lipgloss.NewStyle().Foreground(colorFail).Bold(true)
	styleWarn    = lipgloss.NewStyle().Foreground(colorWarn).Bold(true)
	styleWarnMsg = lipgloss.NewStyle().Foreground(colorWarn)
	styleInfo    = lipgloss.NewStyle().Foreground(colorInfo)
	styleSpinner = lipgloss.NewStyle().Foreground(colorAccent)
	styleCode    = lipgloss.NewStyle().Foreground(colorInfo).Bold(true)

	stylePanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorInfo).
			Padding(0, 1)
	styleErrorBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorFail).
			Padding(0, 1)
	styleCard = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorOK).
			Padding(0, 1)
)

// Symbols for finished tasks.
const (
	SymOK   = "✓"
	SymFail = "✗"
	SymWarn = "!"
	SymSkip = "–"
)

// Theme is the huh theme for every eru prompt.
var Theme = huh.ThemeFunc(func(isDark bool) *huh.Styles {
	s := huh.ThemeBase16(isDark)
	s.Focused.Title = s.Focused.Title.Foreground(colorAccent).Bold(true)
	s.Focused.NoteTitle = s.Focused.NoteTitle.Foreground(colorAccent).Bold(true)
	s.Focused.Base = s.Focused.Base.BorderForeground(colorAccent)
	s.Focused.SelectSelector = s.Focused.SelectSelector.Foreground(colorAccent)
	s.Focused.SelectedOption = s.Focused.SelectedOption.Foreground(colorOK)
	s.Focused.TextInput.Prompt = s.Focused.TextInput.Prompt.Foreground(colorAccent)
	s.Focused.FocusedButton = s.Focused.FocusedButton.Background(colorAccent)
	return s
})

// Header renders a section header with a rule under it.
func Header(title string, width int) string {
	rule := strings.Repeat("─", max(min(width, 72)-len(title)-3, 4))
	return styleHeader.Render("▌ "+title) + " " + styleRule.Render(rule)
}

// Panel renders body in a rounded box with a title line.
func Panel(title, body string) string {
	content := body
	if title != "" {
		content = styleTitle.Render(title) + "\n" + body
	}
	return stylePanel.Render(content)
}

// Warn renders one warning line.
func Warn(msg string) string {
	return styleWarn.Render(SymWarn+" warning") + " " + styleWarnMsg.Render(msg)
}

// Note renders an informational line.
func Note(msg string) string {
	return styleDim.Render("│ ") + msg
}

// Code renders a command or value the operator types or copies.
func Code(s string) string { return styleCode.Render(s) }

// Dim renders secondary text.
func Dim(s string) string { return styleDim.Render(s) }

// ErrorBlock is a failure shown to a human.
type ErrorBlock struct {
	Code    string
	Message string
	Step    string
	Tail    []string
	Hint    string
}

// Render draws the error box: the message, the failed step, its last output
// lines and the hint.
func (e ErrorBlock) Render(width int) string {
	var b strings.Builder
	b.WriteString(styleFail.Render(SymFail+" "+e.Message) + "  " + styleDim.Render("["+e.Code+"]"))
	if e.Step != "" {
		b.WriteString("\n" + styleDim.Render("step ") + styleTitle.Render(e.Step))
	}
	if len(e.Tail) > 0 {
		b.WriteString("\n\n" + styleDim.Render("last output:"))
		inner := max(width-8, 20)
		for _, line := range e.Tail {
			b.WriteString("\n" + styleDim.Render("  "+truncate(line, inner)))
		}
	}
	if e.Hint != "" {
		b.WriteString("\n\n" + styleInfo.Render("→ ") + e.Hint)
	}
	return styleErrorBox.Render(b.String())
}

// Card renders the final summary card: a title and key/value rows.
func Card(title string, rows [][2]string, footer []string) string {
	width := 0
	for _, r := range rows {
		width = max(width, lipgloss.Width(r[0]))
	}
	var b strings.Builder
	b.WriteString(styleOK.Render(SymOK + " " + title))
	for _, r := range rows {
		if r[0] == "" && r[1] == "" {
			b.WriteString("\n")
			continue
		}
		key := r[0] + strings.Repeat(" ", width-lipgloss.Width(r[0]))
		b.WriteString("\n" + styleDim.Render(key) + "  " + r[1])
	}
	if len(footer) > 0 {
		b.WriteString("\n")
		for _, f := range footer {
			b.WriteString("\n" + f)
		}
	}
	return styleCard.Render(b.String())
}
