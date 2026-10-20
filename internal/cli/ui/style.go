// Package ui is vaire's terminal presentation for humans: styled headers,
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
// light or dark theme. Primary text keeps the terminal's default foreground
// and the accent only marks and borders, so it stays readable on both.
var (
	colorAccent = lipgloss.Color("6") // cyan
	colorOK     = lipgloss.Color("2")
	colorFail   = lipgloss.Color("1")
	colorWarn   = lipgloss.Color("3")
	colorInfo   = lipgloss.Color("6")
	colorDim    = lipgloss.Color("8")
)

var (
	styleHeader  = lipgloss.NewStyle().Bold(true)
	styleAccent  = lipgloss.NewStyle().Foreground(colorAccent)
	styleRule    = lipgloss.NewStyle().Foreground(colorDim)
	styleTitle   = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(colorDim)
	styleOK      = lipgloss.NewStyle().Foreground(colorOK).Bold(true)
	styleFail    = lipgloss.NewStyle().Foreground(colorFail).Bold(true)
	styleWarn    = lipgloss.NewStyle().Foreground(colorWarn).Bold(true)
	styleWarnMsg = lipgloss.NewStyle().Foreground(colorWarn)
	styleInfo    = lipgloss.NewStyle().Foreground(colorInfo)
	styleSpinner = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
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

// Theme is the huh theme for every vaire prompt. It is built on ThemeBase16, so
// it follows the terminal's 16 colors, with the accent, the selected option
// and the focused button picked per background for contrast.
var Theme = huh.ThemeFunc(func(isDark bool) *huh.Styles {
	s := huh.ThemeBase16(isDark)
	lightDark := lipgloss.LightDark(isDark)
	// Magenta reads better on a light background, bright magenta on a dark
	// one. The focused button uses a foreground that contrasts with the
	// accent, so the label is never light on light.
	accent := lightDark(lipgloss.Color("5"), lipgloss.Color("13"))
	onAccent := lightDark(lipgloss.Color("15"), lipgloss.Color("0"))
	selected := lightDark(lipgloss.Color("2"), lipgloss.Color("10"))
	s.Focused.Title = s.Focused.Title.Foreground(accent).Bold(true)
	s.Focused.NoteTitle = s.Focused.NoteTitle.Foreground(accent).Bold(true)
	s.Focused.Base = s.Focused.Base.BorderForeground(accent)
	s.Focused.SelectSelector = s.Focused.SelectSelector.Foreground(accent)
	s.Focused.NextIndicator = s.Focused.NextIndicator.Foreground(accent)
	s.Focused.PrevIndicator = s.Focused.PrevIndicator.Foreground(accent)
	s.Focused.SelectedOption = s.Focused.SelectedOption.Foreground(selected)
	s.Focused.SelectedPrefix = s.Focused.SelectedPrefix.Foreground(selected)
	s.Focused.TextInput.Prompt = s.Focused.TextInput.Prompt.Foreground(accent)
	s.Focused.FocusedButton = s.Focused.FocusedButton.Foreground(onAccent).Background(accent).Bold(true)
	s.Focused.Next = s.Focused.FocusedButton
	return s
})

// Header renders a section header with a rule under it. The title keeps the
// terminal's default foreground; the accent only marks it.
func Header(title string, width int) string {
	rule := strings.Repeat("─", max(min(width, 72)-len(title)-3, 4))
	return styleAccent.Render("▌") + " " + styleHeader.Render(title) + " " + styleRule.Render(rule)
}

// Panel renders body in a rounded box with a title line.
func Panel(title, body string) string {
	content := body
	if title != "" {
		content = styleTitle.Render(title) + "\n" + body
	}
	return stylePanel.Render(content)
}

// PanelFit is Panel within width columns: content too wide for the frame
// wraps inside it, and below minPanelWidth the frame is left out, so a
// narrow terminal never breaks the border.
func PanelFit(title, body string, width int) string {
	content := body
	if title != "" {
		content = styleTitle.Render(title) + "\n" + body
	}
	return fitBox(stylePanel, content, width)
}

// fitBox renders content in the bordered style within width columns: content
// too wide for the frame wraps inside it, and below minPanelWidth the frame is
// left out, so a narrow terminal never breaks the border. A width of zero or
// less means unknown and draws the box as is.
func fitBox(style lipgloss.Style, content string, width int) string {
	framed := style.Render(content)
	if width <= 0 || lipgloss.Width(framed) <= width {
		return framed
	}
	if width >= minPanelWidth {
		// Whether Width counts the border differs between lipgloss
		// versions; take the first that fits.
		for _, w := range []int{width, width - 2} {
			if out := style.Width(w).Render(content); lipgloss.Width(out) <= width {
				return out
			}
		}
	}
	return content
}

// minPanelWidth is the narrowest terminal PanelFit draws a frame in.
const minPanelWidth = 30

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

// Render draws the error box within width columns: the message, the failed
// step, its last output lines and the hint. Long lines wrap inside the border.
// The step is only shown when the message does not already name it.
func (e ErrorBlock) Render(width int) string {
	var b strings.Builder
	b.WriteString(styleFail.Render(SymFail+" "+e.Message) + "  " + styleDim.Render("["+e.Code+"]"))
	if e.Step != "" && !strings.Contains(e.Message, e.Step) {
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
	return fitBox(styleErrorBox, b.String(), width)
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
