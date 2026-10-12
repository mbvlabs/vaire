package ui

import (
	"errors"
	"os"
	"strings"

	"charm.land/huh/v2"
)

// ErrAborted is returned when the operator cancels a prompt with Ctrl+C or
// Esc.
var ErrAborted = errors.New("prompt cancelled")

// run shows one form on stderr with the eru theme, with the live area
// suspended.
func run(fields ...huh.Field) error {
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
	return err
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
	if err := run(in); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// Secret asks for a value without echoing it.
func Secret(title string) (string, error) {
	var value string
	if err := run(huh.NewInput().Title(title).EchoMode(huh.EchoModePassword).Value(&value)); err != nil {
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
	if err := run(c); err != nil {
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
	return run(in)
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
	if err := run(sel); err != nil {
		return "", err
	}
	return value, nil
}
