package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
)

// ErrAborted is returned when the operator cancels a prompt with Ctrl+C or
// Esc.
var ErrAborted = errors.New("prompt cancelled")

// run shows one form on stderr with the eru theme, with the live area
// suspended. huh erases the form when it ends, so once it is answered run
// prints the question and answer() as one line that stays in the
// scrollback.
func run(title string, answer func() string, fields ...huh.Field) error {
	resume := Suspend()
	defer resume()
	err := huh.NewForm(huh.NewGroup(fields...)).
		WithTheme(Theme).
		WithOutput(os.Stderr).
		WithShowHelp(false).
		Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrAborted
	}
	if err == nil {
		printAnswered(Writer(os.Stderr), title, answer())
	}
	return err
}

// secretMask stands for a secret answer in the scrollback; it never shows
// the length.
const secretMask = "••••••" //nolint:gosec // G101: a mask, not a credential.

// answeredLine is a finished prompt: the question, then the answer dimmed.
func answeredLine(title, answer string) string {
	line := styleAccent.Render("?") + " " + title
	if answer != "" {
		line += " " + styleDim.Render(answer)
	}
	return line
}

// printAnswered writes the finished prompt line to w and tells the live
// area that something was printed, so the waiting line keeps its pause.
func printAnswered(w io.Writer, title, answer string) {
	_, _ = fmt.Fprintln(w, answeredLine(title, answer))
	activeMu.Lock()
	l := active
	activeMu.Unlock()
	if l != nil {
		l.mu.Lock()
		l.lastOut = time.Now()
		l.mu.Unlock()
	}
}

// RunForm runs a multi-group form (the init interview) with the eru theme.
func RunForm(groups ...*huh.Group) error {
	resume := Suspend()
	defer resume()
	err := huh.NewForm(groups...).WithTheme(Theme).WithOutput(os.Stderr).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrAborted
	}
	return err
}

// Input asks for one line. def is pre-filled.
func Input(title, description, def string) (string, error) {
	value := def
	in := huh.NewInput().Title(title).Value(&value)
	if description != "" {
		in = in.Description(description)
	}
	if err := run(title, func() string { return strings.TrimSpace(value) }, in); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// Secret asks for a value without echoing it.
func Secret(title string) (string, error) {
	var value string
	if err := run(title, func() string { return secretMask }, huh.NewInput().Title(title).EchoMode(huh.EchoModePassword).Value(&value)); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// YesNo asks a yes/no question, defaulting to no.
func YesNo(title, description string) (bool, error) {
	return YesNoDefault(title, description, false)
}

// YesNoDefault asks a yes/no question with def preselected.
func YesNoDefault(title, description string, def bool) (bool, error) {
	ok := def
	c := huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(&ok)
	if description != "" {
		c = c.Description(description)
	}
	answer := func() string {
		if ok {
			return "Yes"
		}
		return "No"
	}
	if err := run(title, answer, c); err != nil {
		return false, err
	}
	return ok, nil
}

// TypeWord asks the operator to type word exactly. Anything else is refused
// inline until it matches or the prompt is cancelled.
func TypeWord(title, description, word string) error {
	var value string
	in := huh.NewInput().
		Title(title).
		Placeholder(word).
		Value(&value).
		Validate(func(s string) error {
			if strings.TrimSpace(s) != word {
				return errors.New("type " + word + " exactly")
			}
			return nil
		})
	if description != "" {
		in = in.Description(description)
	}
	return run(title, func() string { return strings.TrimSpace(value) }, in)
}

// Choose asks the operator to pick one of options (label, value pairs).
func Choose(title, description string, options [][2]string) (string, error) {
	var value string
	opts := make([]huh.Option[string], 0, len(options))
	for _, o := range options {
		opts = append(opts, huh.NewOption(o[0], o[1]))
	}
	sel := huh.NewSelect[string]().Title(title).Options(opts...).Value(&value)
	if description != "" {
		sel = sel.Description(description)
	}
	answer := func() string {
		for _, o := range options {
			if o[1] == value {
				return o[0]
			}
		}
		return value
	}
	if err := run(title, answer, sel); err != nil {
		return "", err
	}
	return value, nil
}
