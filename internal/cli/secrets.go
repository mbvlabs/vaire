package cli

import (
	"net"
	"time"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/secretsdoc"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// runSecretsExport rebuilds the secrets handoff from the local context
// (§10.1). It generates a fresh bundle passphrase and never touches the node.
func runSecretsExport(cmd *cobra.Command, _ []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	doc, err := contextDocument(c)
	if err != nil {
		return err
	}
	if err := requireStructuredHandoff(cmd, "out", "secrets export", ""); err != nil {
		return err
	}
	out := stringFlag(cmd, "out")
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"out": out}, Message: "dry run: would build a new secrets handoff"})
	}
	h, err := newHandoff(cmd, doc, out)
	if err != nil {
		return err
	}
	res := map[string]string{}
	if h.path != "" {
		res["out"] = h.path
	}
	if MustNotPrompt(cmd) {
		// Explicitly requested with --reveal-passphrase.
		res["passphrase"] = h.passphrase
		return output.Success(cmd, output.Result{Data: res, Message: "wrote the transfer bundle to " + h.path})
	}
	if err := h.show(cmd, false, true); err != nil {
		return err
	}
	if h.saved != "" {
		res["saved"] = h.saved
	}
	return output.Success(cmd, output.Result{Data: res, Message: "New secrets handoff shown. The previous bundle still opens with its own passphrase."})
}

// showContextHandoff shows a new handoff for c in a terminal, for commands
// that change the recovery kit (eru backup setup).
func showContextHandoff(cmd *cobra.Command, c *contexts.Context) (string, error) {
	doc, err := contextDocument(c)
	if err != nil {
		return "", err
	}
	h, err := newHandoff(cmd, doc, "")
	if err != nil {
		return "", err
	}
	return h.saved, h.show(cmd, false, true)
}

// contextDocument rebuilds the secrets document from a context.
func contextDocument(c *contexts.Context) (secretsdoc.Document, error) {
	wgKey, err := contexts.ReadFile(c.Name, contexts.WGKeyFile)
	if err != nil {
		return secretsdoc.Document{}, contextFileError(c.Name, contexts.WGKeyFile, err)
	}
	sshKey, err := contexts.ReadFile(c.Name, contexts.SSHKeyFile)
	if err != nil {
		return secretsdoc.Document{}, contextFileError(c.Name, contexts.SSHKeyFile, err)
	}
	creds, err := contexts.LoadCredentials(c.Name)
	if err != nil {
		return secretsdoc.Document{}, err
	}
	known, err := transport.LoadKnownHosts(contexts.Path(c.Name, contexts.KnownHostsFile))
	if err != nil {
		return secretsdoc.Document{}, err
	}
	doc, err := rebuildDocument(c, string(wgKey), string(sshKey), creds, known)
	if err != nil {
		return doc, err
	}
	secrets.Add(string(wgKey))
	secrets.Add(string(sshKey))
	secrets.Add(creds.SudoPassword)
	if creds.Backup != nil {
		for _, v := range creds.Backup.SecretValues() {
			secrets.Add(v)
		}
	}
	return doc, nil
}

// rebuildDocument builds a document from the context and credentials.
func rebuildDocument(c *contexts.Context, wgKey, sshKey string, creds contexts.Credentials, known *transport.KnownHosts) (secretsdoc.Document, error) {
	doc := secretsdoc.Document{
		Schema:     secretsdoc.Schema,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		EruVersion: buildinfo.Version,
		Server:     secretsdoc.Server{Name: c.Server, Domain: c.Domain, TLSMode: c.TLSMode},
		Operator: secretsdoc.Operator{
			Name:          c.Operator.Name,
			WGAddress:     c.Operator.WGAddress,
			WGPrivateKey:  wgKey,
			SSHUser:       c.Operator.SSHUser,
			SSHPrivateKey: sshKey,
			SudoPassword:  creds.SudoPassword,
		},
		Database: creds.Database,
		Backup:   creds.Backup,
	}
	var master string
	for _, n := range c.Nodes {
		hostKeys, err := hostKeyLines(known, n.WGAddress)
		if err != nil {
			return doc, err
		}
		doc.Nodes = append(doc.Nodes, secretsdoc.Node{
			Name:          n.Name,
			Role:          n.Role,
			Hostname:      n.Hostname,
			PublicAddress: n.PublicAddress,
			ListenPort:    n.ListenPort,
			WGAddress:     n.WGAddress,
			WGPublicKey:   n.WGPublicKey,
			SSHHostKeys:   hostKeys,
		})
		if n.Role == contexts.RoleMaster || master == "" {
			master = n.PublicAddress
		}
	}
	doc.BreakGlass = secretsdoc.BreakGlass(c.Server, c.Operator.SSHUser, master)
	return doc, nil
}

// hostKeyLines returns the pinned host keys for a node's WireGuard address.
func hostKeyLines(known *transport.KnownHosts, wgAddress string) ([]string, error) {
	keys := known.Keys(net.JoinHostPort(wgAddress, "22"))
	if len(keys) == 0 {
		return nil, output.NewError(output.CodePrecondition,
			"no pinned host keys for "+wgAddress, output.ExitPrecondition,
			"Re-run eru login with the secrets document that carries the host keys.")
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, string(ssh.MarshalAuthorizedKey(key)))
	}
	return out, nil
}
