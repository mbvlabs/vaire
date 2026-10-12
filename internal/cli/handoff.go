package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/secretsdoc"
	"github.com/mbvlabs/eru/internal/transport"
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
}

// newHandoff encrypts doc into an armored bundle with a new passphrase and
// writes it to out when out is set. Nothing else touches the disk.
func newHandoff(doc secretsdoc.Document, out string) (handoff, error) {
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
	h.bundle, err = secretsdoc.EncryptArmored(doc, passphrase)
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

// show prints the handoff on the terminal: every secret, then the bundle and
// its passphrase. Then it waits for 'stored' and offers to clear the screen.
// It does nothing when eru must not prompt.
func (h handoff) show(cmd *cobra.Command, skipStored bool) error {
	if MustNotPrompt(cmd) {
		return nil
	}
	rich := richOutput(cmd)
	resume := ui.Suspend()
	defer resume()
	w := cmd.ErrOrStderr()
	header := func(title string) {
		if rich {
			_, _ = fmt.Fprintln(w, "\n"+ui.Header(title, 72))
			return
		}
		_, _ = fmt.Fprintln(w, "\n== "+title+" ==")
	}
	label := func(s string) {
		if rich {
			s = ui.Dim(s)
		}
		_, _ = fmt.Fprintln(w, s)
	}

	header("Secrets handoff")
	_, _ = fmt.Fprintln(w, "This is the cluster's master record. Copy every value into your password manager:")
	_, _ = fmt.Fprintln(w, "eru does not keep it, and this is the only time the plain secrets are shown.")
	for _, g := range h.doc.Groups() {
		header(g.Title)
		for _, f := range g.Fields {
			label(f.Label + ":")
			_, _ = fmt.Fprintln(w, strings.TrimRight(f.Value, "\n"))
		}
	}

	header("Transfer bundle")
	if h.path != "" {
		_, _ = fmt.Fprintln(w, "The bundle was written to "+h.path+". Store it with its passphrase.")
	} else {
		_, _ = fmt.Fprintln(w, "Store this block and its passphrase too. On another machine run eru login and paste the block.")
		_, _ = fmt.Fprint(w, string(h.bundle))
	}
	label("Bundle passphrase:")
	_, _ = fmt.Fprintln(w, h.passphrase)
	_, _ = fmt.Fprintln(w)

	if skipStored {
		return nil
	}
	if err := typeWord(cmd, "Type 'stored' once you've saved both parts in your password manager", "", "stored"); err != nil {
		return output.NewError(output.CodeInputRequired, "the secrets handoff was not confirmed as stored", output.ExitInput,
			"Re-run and type stored; the handoff is shown again with a new bundle passphrase.").
			WithDetails(map[string]any{"flag": "--confirm-secrets-stored"})
	}
	if clear, err := yesNoDefaultYes(cmd, "Clear the screen now?"); err == nil && clear {
		_, _ = fmt.Fprint(w, clearScreen)
	}
	return nil
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

// readBundle reads a pasted armored bundle from r up to its footer line.
func readBundle(r io.Reader) ([]byte, error) {
	var b strings.Builder
	sc := bufio.NewScanner(io.LimitReader(r, 4<<20))
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" && b.Len() == 0 {
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
