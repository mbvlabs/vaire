package cli

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/secretsdoc"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/spf13/cobra"
)

// clearScreen clears the screen and, where the terminal supports it, the
// scrollback.
const clearScreen = "\x1b[H\x1b[2J\x1b[3J"

// handoff is a new transfer bundle for a document (§10.1).
type handoff struct {
	doc        secretsdoc.Document
	bundle     []byte
	passphrase string
	// path is where the bundle was written; empty when it is only printed.
	path string
	// saved is where the save action wrote the whole record; empty until it
	// runs.
	saved string
}

// newHandoff encrypts doc into an armored bundle with a new passphrase and
// writes it to out when out is set. Nothing else touches the disk.
func newHandoff(cmd *cobra.Command, doc secretsdoc.Document, out string) (handoff, error) {
	h := handoff{doc: doc}
	for _, v := range doc.SecretValues() {
		secrets.Add(v)
	}
	passphrase, err := secretsdoc.NewPassphrase()
	if err != nil {
		return h, err
	}
	secrets.Add(passphrase)
	h.passphrase = passphrase
	// age's scrypt work factor makes this take about a second.
	stop := waitOn(cmd, "encrypting the transfer bundle")
	h.bundle, err = secretsdoc.EncryptArmored(doc, passphrase)
	stop()
	if err != nil {
		return h, err
	}
	if out != "" {
		if abs, err := filepath.Abs(out); err == nil {
			out = abs
		}
		if err := transport.WriteSecretFile(out, h.bundle); err != nil {
			return h, err
		}
		h.path = out
	}
	return h, nil
}

// requireStructuredHandoff checks the flags a run without a terminal needs
// before anything changes: the bundle goes to --secrets-out (or --out), the
// passphrase is only revealed on request, and storing is confirmed up front.
func requireStructuredHandoff(cmd *cobra.Command, outFlag, action string, confirmFlag string) error {
	if !MustNotPrompt(cmd) {
		return nil
	}
	if err := RequireFlag(cmd, stringFlag(cmd, outFlag) != "", "--"+outFlag, action); err != nil {
		return err
	}
	if err := RequireFlag(cmd, flagTrue(cmd, "reveal-passphrase"), "--reveal-passphrase", action); err != nil {
		return err
	}
	if confirmFlag != "" {
		return RequireFlag(cmd, flagTrue(cmd, confirmFlag), "--"+confirmFlag, action)
	}
	return nil
}

// show prints a short handoff: what the record is and the bundle
// passphrase. The operator then copies the whole record to the clipboard,
// prints it when the terminal blocks the copy, or, with canSave, saves it
// to a file, and confirms with 'stored'. It does nothing when vaire must not
// prompt.
func (h *handoff) show(cmd *cobra.Command, skipStored, canSave bool) error {
	if MustNotPrompt(cmd) {
		return nil
	}
	rich := richOutput(cmd)
	resume := ui.Suspend()
	defer resume()
	w := cmd.ErrOrStderr()
	note := func(s string) {
		if rich {
			s = ui.Note(s)
		}
		_, _ = fmt.Fprintln(w, s)
	}

	if rich {
		_, _ = fmt.Fprintln(w, "\n"+ui.Header("Secrets handoff", 72))
	} else {
		_, _ = fmt.Fprintln(w, "\n== Secrets handoff ==")
	}
	_, _ = fmt.Fprintln(w, "The cluster's master record: the operator's keys, the sudo password and the database passwords.")
	_, _ = fmt.Fprintln(w, "vaire keeps no copy and doesn't show it again. Store it in your password manager.")
	_, _ = fmt.Fprintln(w)
	label := "Bundle passphrase"
	if rich {
		label = ui.Dim(label)
	}
	_, _ = fmt.Fprintln(w, label+"  "+h.passphrase)
	if h.path != "" {
		note("The transfer bundle was saved to " + h.path + ". Keep the passphrase apart from it.")
	}
	_, _ = fmt.Fprintln(w)

	for {
		options := [][2]string{{"Copy everything to the clipboard", "copy"}}
		if canSave && h.saved == "" {
			options = append(options, [2]string{"Save everything to a file", "save"})
		}
		options = append(options, [2]string{"Show everything on screen", "print"}, [2]string{"Done", "done"})
		choice, err := chooseOption(cmd, "Secrets handoff", options)
		if err != nil {
			return err
		}
		if choice == "done" {
			break
		}
		switch choice {
		case "copy":
			_, _ = fmt.Fprint(w, osc52(h.record()))
			note("Copied to your clipboard: every value, the transfer bundle and its passphrase. Paste it into your password manager. Nothing pasted? Pick Show everything on screen.")
		case "print":
			// For terminals that block OSC 52, such as tmux without
			// set-clipboard on.
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprint(w, h.record())
			_, _ = fmt.Fprintln(w)
		case "save":
			if err := h.save(cmd); err != nil {
				note(err.Error())
				continue
			}
			note("Saved everything to " + h.saved + " (plain text, readable only by you): every value, the transfer bundle and its passphrase.")
		}
	}

	if skipStored {
		return nil
	}
	if err := typeWord(cmd, "Type 'stored' once the record is in your password manager", "", "stored"); err != nil {
		return output.NewError(output.CodeInputRequired, "the secrets handoff was not confirmed as stored", output.ExitInput,
			"Re-run and type stored; the handoff is shown again with a new bundle passphrase.").
			WithDetails(map[string]any{"flag": "--confirm-secrets-stored"})
	}
	if clear, err := yesNoDefaultYes(cmd, "Clear the screen now?"); err == nil && clear {
		_, _ = fmt.Fprint(w, clearScreen)
	}
	return nil
}

// save asks for a path, relative to the current directory, and writes the
// whole record there (0600): the same text the copy action puts on the
// clipboard. It never overwrites an existing file.
func (h *handoff) save(cmd *cobra.Command) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path, err := promptLine(cmd, "Save everything to (relative to "+cwd+")", "./"+h.doc.Server.Name+"-secrets.txt")
	if err != nil {
		return err
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, rest)
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	if _, err := os.Stat(path); err == nil {
		return errors.New(path + " already exists; pick another path")
	}
	if err := transport.WriteSecretFile(path, []byte(h.record())); err != nil {
		return err
	}
	h.saved = path
	return nil
}

// record is the plain-text handoff the copy action puts on the clipboard:
// every value grouped like password-manager entries, then the bundle and its
// passphrase.
func (h handoff) record() string {
	var b strings.Builder
	b.WriteString("vaire secrets handoff for server " + h.doc.Server.Name + "\n")
	for _, g := range h.doc.Groups() {
		b.WriteString("\n## " + g.Title + "\n")
		for _, f := range g.Fields {
			v := strings.TrimRight(f.Value, "\n")
			if strings.Contains(v, "\n") {
				b.WriteString(f.Label + ":\n" + v + "\n")
				continue
			}
			b.WriteString(f.Label + ": " + v + "\n")
		}
	}
	b.WriteString("\n## Transfer bundle\n")
	b.WriteString("On another machine run vaire login and paste this block.\n")
	b.WriteString("Passphrase: " + h.passphrase + "\n")
	b.Write(h.bundle)
	return b.String()
}

// osc52 wraps s in the OSC 52 sequence that asks the terminal to put it on the
// system clipboard. It works over SSH, so it also reaches the operator's
// clipboard from a server console.
func osc52(s string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\a"
}

// chooseOption asks the operator to pick one of options (label, value
// pairs): a select in a rich terminal, a numbered list otherwise.
func chooseOption(cmd *cobra.Command, title string, options [][2]string) (string, error) {
	if richOutput(cmd) {
		v, err := ui.Choose(title, "", options)
		return v, promptErr(err)
	}
	w := cmd.ErrOrStderr()
	for i, o := range options {
		_, _ = fmt.Fprintf(w, "  %d) %s\n", i+1, o[0])
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	for {
		if _, err := fmt.Fprint(w, title+" [1-"+strconv.Itoa(len(options))+"]: "); err != nil {
			return "", err
		}
		line, err := reader.ReadString('\n')
		if n, convErr := strconv.Atoi(strings.TrimSpace(line)); convErr == nil && n >= 1 && n <= len(options) {
			return options[n-1][1], nil
		}
		if err != nil {
			return "", output.NewError(output.CodeInputRequired, "no option was picked", output.ExitInput, "")
		}
	}
}

// yesNoDefaultYes asks a [Y/n] question in a terminal.
func yesNoDefaultYes(cmd *cobra.Command, question string) (bool, error) {
	if richOutput(cmd) {
		ok, err := ui.YesNoDefault(question, "", true)
		if errors.Is(err, ui.ErrAborted) {
			return false, nil
		}
		return ok, err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s [Y/n] ", question); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "n", "no":
		return false, nil
	default:
		return true, nil
	}
}

// readBundle reads a pasted armored bundle from r, from its header line up
// to its footer line.
func readBundle(r io.Reader) ([]byte, error) {
	var b strings.Builder
	sc := bufio.NewScanner(io.LimitReader(r, 4<<20))
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if b.Len() == 0 && line != secretsdoc.ArmorHeader {
			// Skip whatever precedes the block, such as the copied record.
			continue
		}
		b.WriteString(line + "\n")
		if line == secretsdoc.ArmorFooter {
			return []byte(b.String()), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
