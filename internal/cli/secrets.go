package cli

import (
	"fmt"
	"net"
	"time"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/secretsdoc"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// runSecretsExport rebuilds the secrets handoff from the local workspace
// (§10.1). It generates a fresh bundle passphrase and offers a node-verified
// code, with the local master password as an offline fallback.
// The whole document is revealed, so a human master password or a code from
// the authenticator app is required (secretGateUnlock).
func runSecretsExport(cmd *cobra.Command, _ []string) error {
	c, err := workspaces.Resolve(stringFlag(cmd, "workspace"))
	if err != nil {
		return err
	}

	hash, err := masterPasswordHash(c.Name)
	if err != nil {
		return err
	}

	if err := secretGateUnlock(cmd, hash, nil, nil, "vaire secrets export"); err != nil {
		return err
	}

	if err := requireStructuredHandoff(cmd, "out", "secrets export", ""); err != nil {
		return err
	}

	out := stringFlag(cmd, "out")
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"out": out}, Message: "dry run: would build a new secrets handoff"})
	}

	doc, err := secretsExportDocument(cmd, c)
	if err != nil {
		return err
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

	message := "New secrets handoff shown. The previous bundle still opens with its own passphrase."
	if doc.RecoveryCode != "" {
		message = "New recovery kit shown. Store it securely; the previous recovery code is no longer valid."
	}

	return output.Success(cmd, output.Result{Data: res, Message: message})
}

func secretsExportDocument(cmd *cobra.Command, c *workspaces.Workspace) (secretsdoc.Document, error) {
	if !c.Managed {
		return workspaceDocument(c)
	}

	creds, err := workspaces.LoadCredentials(c.Name)
	if err != nil {
		return secretsdoc.Document{}, err
	}

	known, err := transport.LoadKnownHosts(workspaces.Path(c.Name, workspaces.KnownHostsFile))
	if err != nil {
		return secretsdoc.Document{}, err
	}

	doc, err := rebuildDocument(c, "", "", creds, known)
	if err != nil {
		return doc, err
	}

	doc.Operator = secretsdoc.Operator{Name: c.Operator.Name}
	doc.BreakGlass = ""

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return doc, err
	}
	defer s.Close()

	params, err := newRecoveryRequest(c.Operator.Name)
	if err != nil {
		return doc, err
	}

	reply, err := p.call(commandContext(cmd), s.node, "access.recovery-issue", params, remote.CallOptions{Secrets: params.Secrets()})
	if err != nil {
		return doc, err
	}

	var device rpc.Device
	if err := reply.Decode(&device); err != nil {
		return doc, err
	}

	secrets.Add(device.SetupCode)

	setup, err := rpc.DecodeAccessSetup(device.SetupCode)
	if err != nil {
		return doc, err
	}

	if setup.Mode != rpc.AccessSetupRecovery || setup.Role != config.AccessRoleOwner || setup.Server != c.Server ||
		setup.User != c.Operator.Name || setup.Token != params.Token || setup.TemporaryPrivateKey != params.TemporaryPrivateKey {
		return doc, fmt.Errorf("node returned a mismatched recovery code")
	}

	doc.RecoveryCode = device.SetupCode

	return doc, nil
}

// showWorkspaceHandoff shows a new handoff for c in a terminal, for commands
// that change the recovery kit (vaire backup setup).
func showWorkspaceHandoff(cmd *cobra.Command, c *workspaces.Workspace) error {
	doc, err := workspaceDocument(c)
	if err != nil {
		return err
	}

	h, err := newHandoff(cmd, doc, "")
	if err != nil {
		return err
	}

	return h.show(cmd, false, true)
}

// workspaceDocument rebuilds the secrets document from a workspace.
func workspaceDocument(c *workspaces.Workspace) (secretsdoc.Document, error) {
	if err := managedHandoffError(c); err != nil {
		return secretsdoc.Document{}, err
	}

	wgKey, err := workspaces.ReadFile(c.Name, workspaces.WGKeyFile)
	if err != nil {
		return secretsdoc.Document{}, workspaceFileError(c.Name, workspaces.WGKeyFile, err)
	}

	sshKey, err := workspaces.ReadFile(c.Name, workspaces.SSHKeyFile)
	if err != nil {
		return secretsdoc.Document{}, workspaceFileError(c.Name, workspaces.SSHKeyFile, err)
	}

	creds, err := workspaces.LoadCredentials(c.Name)
	if err != nil {
		return secretsdoc.Document{}, err
	}

	known, err := transport.LoadKnownHosts(workspaces.Path(c.Name, workspaces.KnownHostsFile))
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

	if creds.Metrics != nil {
		secrets.Add(creds.Metrics.Password)
	}

	return doc, nil
}

func managedHandoffError(c *workspaces.Workspace) error {
	if c.Managed {
		return output.NewError(output.CodePrecondition, "managed device keys cannot be exported", output.ExitPrecondition,
			"Ask an owner to enroll another device instead.")
	}

	return nil
}

// rebuildDocument builds a document from the workspace and credentials.
func rebuildDocument(c *workspaces.Workspace, wgKey, sshKey string, creds workspaces.Credentials, known *transport.KnownHosts) (secretsdoc.Document, error) {
	doc := secretsdoc.Document{
		Schema:       secretsdoc.Schema,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		VaireVersion: buildinfo.Version,
		Server:       secretsdoc.Server{Name: c.Server, Domain: c.Domain, TLSMode: c.TLSMode},
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
		// Carry the master password hash so vaire login installs it on another
		// machine and the gate follows the document.
		MasterPassword: creds.MasterPasswordHash,
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
		if n.Role == workspaces.RoleMaster || master == "" {
			master = n.PublicAddress
		}
	}

	doc.BreakGlass = secretsdoc.BreakGlass(c.Server, c.Operator.SSHUser, master)
	if n, ok := c.Master(); ok {
		doc.Metrics = metricsDocument(creds.Metrics, n.Hostname, "")
	}

	return doc, nil
}

// hostKeyLines returns the pinned host keys for a node's WireGuard address.
func hostKeyLines(known *transport.KnownHosts, wgAddress string) ([]string, error) {
	keys := known.Keys(net.JoinHostPort(wgAddress, "22"))
	if len(keys) == 0 {
		return nil, output.NewError(output.CodePrecondition,
			"no pinned host keys for "+wgAddress, output.ExitPrecondition,
			"Re-run vaire login with the secrets document that carries the host keys.")
	}

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, string(ssh.MarshalAuthorizedKey(key)))
	}

	return out, nil
}
