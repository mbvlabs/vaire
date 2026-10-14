package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// CredentialRow is one role's login in a credentials block.
type CredentialRow struct {
	Role, User, Password string
}

// URLRow is one connection URL in a credentials block.
type URLRow struct {
	Label, URL string
}

// Credentials renders one-time database credentials: headline, then a framed
// table of roles, users and passwords titled title, then each URL on its own
// unframed line, then note emphasised as a warning. Passwords are base62, so
// a double-click selects one without the frame; URLs carry punctuation, so
// they sit flush left outside the frame where a line selection copies
// nothing else. When the frame would not fit in width columns it is
// dropped, and the note wraps to width.
func Credentials(headline, title string, rows []CredentialRow, urls []URLRow, note string, width int) string {
	width = max(width, 20)
	roleW, userW := len("ROLE"), len("USER")
	for _, r := range rows {
		roleW = max(roleW, lipgloss.Width(r.Role))
		userW = max(userW, lipgloss.Width(r.User))
	}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0)) }
	lines := []string{
		styleTitle.Render(title),
		"",
		styleDim.Render(pad("ROLE", roleW) + "  " + pad("USER", userW) + "  PASSWORD"),
	}
	for _, r := range rows {
		lines = append(lines, styleAccent.Render(pad(r.Role, roleW))+"  "+pad(r.User, userW)+"  "+r.Password)
	}
	table := strings.Join(lines, "\n")
	// Border and padding take two columns on each side.
	if lipgloss.Width(table)+4 <= width {
		table = styleCard.Render(table)
	}

	var b strings.Builder
	if headline != "" {
		b.WriteString(styleOK.Render(SymOK+" "+headline) + "\n")
	}
	b.WriteString(table)
	if len(urls) > 0 {
		b.WriteString("\n")
	}
	for _, u := range urls {
		b.WriteString("\n" + styleDim.Render(u.Label+":") + "\n" + u.URL)
	}
	if note != "" {
		wrapped := lipgloss.NewStyle().Width(max(width-2, 18)).Render(note)
		b.WriteString("\n")
		for i, line := range strings.Split(wrapped, "\n") {
			mark := "  "
			if i == 0 {
				mark = SymWarn + " "
			}
			b.WriteString("\n" + styleWarn.Render(mark+strings.TrimRight(line, " ")))
		}
	}
	return b.String()
}
