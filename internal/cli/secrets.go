package cli

import (
	"net"
	"path/filepath"
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

// runSecretsExport rebuilds the secrets document from the local context
// (§10.1). It generates a fresh passphrase and never touches the node.
func runSecretsExport(cmd *cobra.Command, _ []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	wgKey, err := contexts.ReadFile(c.Name, contexts.WGKeyFile)
	if err != nil {
		return contextFileError(c.Name, contexts.WGKeyFile, err)
	}
	sshKey, err := contexts.ReadFile(c.Name, contexts.SSHKeyFile)
	if err != nil {
		return contextFileError(c.Name, contexts.SSHKeyFile, err)
	}
	creds, err := contexts.LoadCredentials(c.Name)
	if err != nil {
		return err
	}
	known, err := transport.LoadKnownHosts(contexts.Path(c.Name, contexts.KnownHostsFile))
	if err != nil {
		return err
	}
	doc, err := rebuildDocument(c, string(wgKey), string(sshKey), creds, known)
	if err != nil {
		return err
	}
	secrets.Add(string(wgKey))
	secrets.Add(string(sshKey))
	secrets.Add(creds.SudoPassword)
	for _, v := range doc.SecretValues() {
		secrets.Add(v)
	}

	out := stringFlag(cmd, "out")
	if out == "" {
		out = "eru-" + c.Cluster + ".secrets.age"
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	passphrase, err := secretsdoc.NewPassphrase()
	if err != nil {
		return err
	}
	secrets.Add(passphrase)
	encrypted, err := secretsdoc.Encrypt(doc, passphrase)
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"out": out}, Message: "dry run: would write " + out})
	}
	if err := transport.WriteSecretFile(out, encrypted); err != nil {
		return err
	}
	res := map[string]string{"out": out}
	message := "wrote " + out
	if ppOut := stringFlag(cmd, "passphrase-out"); ppOut == "-" {
		res["passphrase"] = passphrase
		message += "\npassphrase: " + passphrase
	} else if ppOut != "" {
		if abs, err := filepath.Abs(ppOut); err == nil {
			ppOut = abs
		}
		if err := transport.WriteSecretFile(ppOut, []byte(passphrase+"\n")); err != nil {
			return err
		}
		res["passphrase_out"] = ppOut
		message += "\npassphrase written to " + ppOut
	} else {
		message += "\npassphrase (shown once): " + passphrase
	}
	return output.Success(cmd, output.Result{Data: res, Message: message})
}

// rebuildDocument builds a document from the context and credentials.
func rebuildDocument(c *contexts.Context, wgKey, sshKey string, creds contexts.Credentials, known *transport.KnownHosts) (secretsdoc.Document, error) {
	doc := secretsdoc.Document{
		Schema:     secretsdoc.Schema,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		EruVersion: buildinfo.Version,
		Cluster:    secretsdoc.Cluster{Name: c.Cluster, Domain: c.Domain, Stanza: c.Cluster, TLSMode: c.TLSMode},
		Operator: secretsdoc.Operator{
			Name:          c.Operator.Name,
			WGAddress:     c.Operator.WGAddress,
			WGPrivateKey:  wgKey,
			SSHUser:       c.Operator.SSHUser,
			SSHPrivateKey: sshKey,
			SudoPassword:  creds.SudoPassword,
		},
		Database: creds.Database,
		ConnectionNotes: func() string {
			if creds.Database == nil {
				return ""
			}
			return secretsdoc.ConnectionNotes
		}(),
	}
	var primary string
	for _, n := range c.Nodes {
		hostKeys, err := hostKeyLines(known, n.WGAddress)
		if err != nil {
			return doc, err
		}
		doc.Nodes = append(doc.Nodes, secretsdoc.Node{
			Name:          n.Name,
			Role:          n.Role,
			PublicAddress: n.PublicAddress,
			ListenPort:    n.ListenPort,
			WGAddress:     n.WGAddress,
			WGPublicKey:   n.WGPublicKey,
			SSHHostKeys:   hostKeys,
		})
		if n.Role == "primary" || primary == "" {
			primary = n.PublicAddress
		}
	}
	doc.BreakGlass = secretsdoc.BreakGlass(c.Cluster, c.Operator.SSHUser, primary)
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
