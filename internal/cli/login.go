package cli

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/secretsdoc"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

type loginData struct {
	Context  string             `json:"context"`
	Current  bool               `json:"current"`
	Path     string             `json:"path"`
	Document secretsdoc.Summary `json:"document"`
}

// runLogin decrypts a transfer bundle (armored or binary age) and creates a
// local context (§10.1). The bundle comes from a file, from stdin with -, or
// is pasted at a prompt when no argument is given. A file, stdin or paste may
// hold the whole copied handoff; only its armored block is used. The bundle is
// read and checked before the passphrase is asked for.
func runLogin(cmd *cobra.Command, args []string) error {
	raw, err := loginBundle(cmd, args)
	if err != nil {
		return err
	}
	passphrase, err := loginPassphrase(cmd)
	if err != nil {
		return err
	}
	secrets.Add(passphrase)
	// age's scrypt work factor makes this take about a second.
	stop := waitOn(cmd, "decrypting the transfer bundle")
	doc, err := secretsdoc.Decrypt(raw, passphrase)
	stop()
	if err != nil {
		return err
	}
	for _, v := range doc.SecretValues() {
		secrets.Add(v)
	}
	name := stringFlag(cmd, "name")
	if name == "" {
		name = doc.Server.Name
	}
	ctx, err := secretsdoc.Install(doc, name, "")
	if err != nil {
		return err
	}
	current, _ := contexts.Current()
	summary := secretsdoc.Summarize(doc)
	message := fmt.Sprintf("Created context %s for %s.", ctx.Name, summary)
	if current == ctx.Name {
		message += " It is the current context."
	} else {
		message += " Switch to it with eru context use " + ctx.Name + "."
	}
	return output.Success(cmd, output.Result{
		Data:    loginData{Context: ctx.Name, Current: current == ctx.Name, Path: contexts.Dir(ctx.Name), Document: summary},
		Message: message,
		Next:    []string{"eru plan"},
	})
}

// loginBundle reads the bundle from the argument, stdin (-), or a paste.
func loginBundle(cmd *cobra.Command, args []string) ([]byte, error) {
	src := ""
	if len(args) == 1 {
		src = args[0]
	}
	switch src {
	case "-":
		if flagTrue(cmd, "passphrase-stdin") {
			return nil, output.NewError(output.CodeUsage, "the bundle and the passphrase can't both come from stdin", output.ExitUsage,
				"Pass the passphrase with --passphrase-file, or the bundle as a file.")
		}
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4<<20))
		if err != nil {
			return nil, output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		return extractBundle(raw, "on stdin")
	case "":
		if err := RequireFlag(cmd, false, "<bundle-file>", "login"); err != nil {
			return nil, err
		}
		restore := ui.Suspend()
		defer restore()
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Paste the transfer bundle (from "+secretsdoc.ArmorHeader+" to "+secretsdoc.ArmorFooter+"), or the whole copied handoff:")
		raw, err := readBundle(cmd.InOrStdin())
		if err != nil {
			return nil, output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		if !secretsdoc.IsArmored(raw) {
			return nil, output.NewError(output.CodeUsage, "that is not a transfer bundle", output.ExitUsage,
				"Paste the whole block, including the BEGIN and END lines.")
		}
		return raw, nil
	default:
		raw, err := os.ReadFile(src) //nolint:gosec // G304: the operator chose this bundle file.
		if err != nil {
			return nil, output.WrapError(output.CodeUsage, err, output.ExitUsage, "Pass the path of a transfer bundle, - for stdin, or nothing to paste it.")
		}
		return extractBundle(raw, "in "+src)
	}
}

// ageBinaryPrefix starts a binary (unarmored) age file.
const ageBinaryPrefix = "age-encryption.org/"

// extractBundle returns raw unchanged when it is a binary age file, and
// otherwise the armored block inside it, so a file or stdin may hold the
// bundle alone or the whole copied handoff. where names the source for the
// error, such as "in bundle.txt" or "on stdin".
func extractBundle(raw []byte, where string) ([]byte, error) {
	if bytes.HasPrefix(raw, []byte(ageBinaryPrefix)) {
		return raw, nil
	}
	block, err := readBundle(bytes.NewReader(raw))
	if err != nil {
		return nil, output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
	}
	if !secretsdoc.IsArmored(block) {
		return nil, output.NewError(output.CodeUsage, "no transfer bundle found "+where, output.ExitUsage,
			"Pass the bundle file eru printed, or the whole copied handoff with its "+secretsdoc.ArmorHeader+" block. Or run eru login with no argument and paste it.")
	}
	return block, nil
}

func loginPassphrase(cmd *cobra.Command) (string, error) {
	if path := stringFlag(cmd, "passphrase-file"); path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this passphrase file.
		if err != nil {
			return "", output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		line, err := readFirstLine(strings.NewReader(string(raw)))
		return strings.TrimSpace(line), err
	}
	if flagTrue(cmd, "passphrase-stdin") {
		line, err := readFirstLine(cmd.InOrStdin())
		return strings.TrimSpace(line), err
	}
	if err := RequireFlag(cmd, false, "--passphrase-stdin", "login"); err != nil {
		return "", err
	}
	return readSecretTTY(cmd, "Passphrase")
}

type contextRow struct {
	Name    string `json:"name"`
	Server  string `json:"server"`
	Current bool   `json:"current"`
	Nodes   int    `json:"nodes"`
}

func runContextList(cmd *cobra.Command, _ []string) error {
	names, err := contexts.List()
	if err != nil {
		return err
	}
	current, _ := contexts.Current()
	rows := make([]contextRow, 0, len(names))
	lines := make([]string, 0, len(names))
	for _, name := range names {
		c, err := contexts.Load(name)
		if err != nil {
			return err
		}
		rows = append(rows, contextRow{Name: name, Server: c.Server, Current: name == current, Nodes: len(c.Nodes)})
		mark := "  "
		if name == current {
			mark = "* "
		}
		lines = append(lines, mark+name+" (server "+c.Server+")")
	}
	message := strings.Join(lines, "\n")
	if len(rows) == 0 {
		message = "No contexts. Create one with eru login and the transfer bundle."
	}
	return output.Success(cmd, output.Result{Data: rows, Message: message})
}

type contextShow struct {
	*contexts.Context
	Current              bool              `json:"current"`
	Path                 string            `json:"path"`
	OperatorWGPublicKey  string            `json:"operator_wg_public_key"`
	OperatorSSHPublicKey string            `json:"operator_ssh_public_key"`
	HostKeyFingerprints  map[string]string `json:"host_key_fingerprints"`
}

func runContextShow(cmd *cobra.Command, args []string) error {
	flag := stringFlag(cmd, "context")
	if len(args) == 1 {
		flag = args[0]
	}
	c, err := contexts.Resolve(flag)
	if err != nil {
		return err
	}
	warnings := contexts.FixModes(c.Name)
	current, _ := contexts.Current()
	show := contextShow{Context: c, Current: current == c.Name, Path: contexts.Dir(c.Name), HostKeyFingerprints: map[string]string{}}
	if raw, err := contexts.ReadFile(c.Name, contexts.WGKeyFile); err == nil {
		secrets.Add(strings.TrimSpace(string(raw)))
		show.OperatorWGPublicKey, _ = transport.WGPublicKey(string(raw))
	}
	if raw, err := contexts.ReadFile(c.Name, contexts.SSHKeyFile); err == nil {
		show.OperatorSSHPublicKey, _ = transport.AuthorizedKey(raw, "")
	}
	if known, err := transport.LoadKnownHosts(contexts.Path(c.Name, contexts.KnownHostsFile)); err == nil {
		for _, n := range c.Nodes {
			addr := net.JoinHostPort(n.WGAddress, "22")
			for _, key := range known.Keys(addr) {
				show.HostKeyFingerprints[key.Type()+" "+transport.HostID(addr)] = ssh.FingerprintSHA256(key)
			}
		}
	}
	lines := []string{
		"context   " + c.Name,
		"server    " + c.Server,
		"operator  " + c.Operator.Name + " (" + c.Operator.WGAddress + ", ssh user " + c.Operator.SSHUser + ")",
	}
	for _, n := range c.Nodes {
		lines = append(lines, fmt.Sprintf("node      %s %s %s: public %s:%d, wireguard %s", n.Name, n.Role, n.Hostname, n.PublicAddress, n.ListenPort, n.WGAddress))
	}
	for _, db := range slices.Sorted(maps.Keys(c.Databases)) {
		lines = append(lines, "database  "+db+" on "+c.Databases[db])
	}
	return output.Success(cmd, output.Result{Data: show, Message: strings.Join(lines, "\n"), Warnings: warnings})
}

func runContextUse(cmd *cobra.Command, args []string) error {
	if err := contexts.ValidateName(args[0]); err != nil {
		return err
	}
	if err := contexts.SetCurrent(args[0]); err != nil {
		return err
	}
	return output.OK(cmd, map[string]string{"current": args[0]}, "Current context is now "+args[0]+".")
}

func runContextRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	if err := contexts.ValidateName(name); err != nil {
		return err
	}
	if !contexts.Exists(name) {
		_, err := contexts.Load(name)
		return err
	}
	if err := Confirm(cmd, name); err != nil {
		return err
	}
	// The tunnel daemon would keep the removed context's tunnel up.
	stopTunnelDaemonQuietly(name, true)
	if err := contexts.Remove(name); err != nil {
		return err
	}
	return output.OK(cmd, map[string]any{"removed": name}, "Removed context "+name+". Its transfer bundle still opens the server.")
}
