package secretsdoc

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/password"
	"github.com/mbvlabs/vaire/internal/transport"
)

// BreakGlass is the break-glass text stored in the document (§8.4).
func BreakGlass(server, adminUser, publicAddress string) string {
	return strings.Join([]string{
		"# Break-glass access for server " + server,
		"",
		"Use this when WireGuard is unreachable and vaire cannot reach the server from your machine.",
		"",
		"1. Open the provider's web console for " + publicAddress + " and log in as root.",
		"2. Run `vaire node unlock-ssh --minutes 30`. Public SSH (22/tcp) opens for 30 minutes and only the admin user `" + adminUser + "` may log in, with its password (`operator.sudo_password` in this document).",
		"3. From your machine: `ssh " + adminUser + "@" + publicAddress + "`. `sudo` takes the same password.",
		"4. Fix WireGuard (`sudo systemctl status wg-quick@wg0`, `sudo wg show`, `sudo ufw status`), then check from your machine with `vaire plan`.",
		"5. Close public SSH early with `sudo vaire node ssh-revert --lockdown-only`, or let the timer close it.",
		"",
		"Without console access, rebuild the server from backups with `vaire server init --restore`.",
		"",
	}, "\n")
}

// Install creates the local context name from doc (§10.1 vaire login). It
// refuses to replace an existing context. lockdownConfirmedAt is set when the
// caller already confirmed the SSH lockdown over WireGuard.
func Install(doc Document, name, lockdownConfirmedAt string) (*contexts.Context, error) {
	if err := contexts.ValidateName(name); err != nil {
		return nil, err
	}
	if contexts.Exists(name) {
		return nil, output.NewError(
			output.CodePrecondition,
			"context "+name+" already exists",
			output.ExitPrecondition,
			"Pick another name with --name, or remove it first with vaire context remove "+name+".",
		).WithDetails(map[string]any{"reason": "context_exists", "context": name})
	}
	if len(doc.Nodes) == 0 {
		return nil, secretsError("the secrets document lists no node", nil)
	}
	if doc.MasterPassword != "" && !password.Valid(doc.MasterPassword) {
		return nil, secretsError("the master password verifier in the document is invalid or has unsupported costs", nil)
	}
	ctx := &contexts.Context{
		Name:      name,
		Server:    doc.Server.Name,
		Domain:    doc.Server.Domain,
		TLSMode:   doc.Server.TLSMode,
		Databases: map[string]string{},
		Operator: contexts.Operator{
			Name:      doc.Operator.Name,
			WGAddress: doc.Operator.WGAddress,
			SSHUser:   doc.Operator.SSHUser,
		},
		LockdownConfirmedAt: lockdownConfirmedAt,
		CreatedAt:           time.Now().UTC().Format(time.RFC3339),
		VaireVersion:        buildinfo.Version,
	}
	if _, err := transport.WGPublicKey(doc.Operator.WGPrivateKey); err != nil {
		return nil, secretsError("the operator WireGuard key in the document is invalid", err)
	}
	if _, err := transport.KeyAuth([]byte(doc.Operator.SSHPrivateKey)); err != nil {
		return nil, secretsError("the operator SSH key in the document is invalid", err)
	}
	known, err := transport.LoadKnownHosts(contexts.Path(name, contexts.KnownHostsFile))
	if err != nil {
		return nil, err
	}
	for _, n := range doc.Nodes {
		keys, err := transport.ParseHostKeys(n.SSHHostKeys)
		if err != nil || len(keys) == 0 {
			return nil, secretsError("the host keys of node "+n.Name+" are invalid", err)
		}
		known.Pin(net.JoinHostPort(n.WGAddress, "22"), keys...)
		ctx.Nodes = append(ctx.Nodes, contexts.Node{
			Name:          n.Name,
			Role:          n.Role,
			Hostname:      n.Hostname,
			PublicAddress: n.PublicAddress,
			WGAddress:     n.WGAddress,
			WGPublicKey:   n.WGPublicKey,
			ListenPort:    n.ListenPort,
		})
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(contexts.Dir(name))
		}
	}()
	if err := contexts.WriteFile(name, contexts.WGKeyFile, []byte(doc.Operator.WGPrivateKey+"\n")); err != nil {
		return nil, err
	}
	if err := contexts.WriteFile(name, contexts.SSHKeyFile, []byte(doc.Operator.SSHPrivateKey)); err != nil {
		return nil, err
	}
	if err := known.Save(); err != nil {
		return nil, err
	}
	if err := contexts.SaveCredentials(name, contexts.Credentials{SudoPassword: doc.Operator.SudoPassword, Database: doc.Database, Backup: doc.Backup, Metrics: doc.Metrics.creds(), MasterPasswordHash: doc.MasterPassword}); err != nil {
		return nil, err
	}
	if err := contexts.Save(ctx); err != nil {
		return nil, err
	}
	ok = true
	if current, _ := contexts.Current(); current == "" || !contexts.Exists(current) {
		if err := contexts.SetCurrent(name); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// Summary is the public part of a document, safe to print.
type Summary struct {
	Server   string        `json:"server"`
	Domain   string        `json:"domain,omitempty"`
	Operator string        `json:"operator"`
	SSHUser  string        `json:"ssh_user"`
	Nodes    []NodeSummary `json:"nodes"`
	Created  string        `json:"created_at"`
	Version  string        `json:"vaire_version"`
}

// NodeSummary is one node's public identity.
type NodeSummary struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	Hostname      string `json:"hostname,omitempty"`
	PublicAddress string `json:"public_address"`
	Endpoint      string `json:"wireguard_endpoint"`
	WGAddress     string `json:"wg_address"`
}

// Summarize returns doc without secrets.
func Summarize(doc Document) Summary {
	s := Summary{
		Server:   doc.Server.Name,
		Domain:   doc.Server.Domain,
		Operator: doc.Operator.Name,
		SSHUser:  doc.Operator.SSHUser,
		Created:  doc.CreatedAt,
		Version:  doc.VaireVersion,
	}
	for _, n := range doc.Nodes {
		s.Nodes = append(s.Nodes, NodeSummary{
			Name:          n.Name,
			Role:          n.Role,
			Hostname:      n.Hostname,
			PublicAddress: n.PublicAddress,
			Endpoint:      net.JoinHostPort(n.PublicAddress, strconv.Itoa(n.ListenPort)),
			WGAddress:     n.WGAddress,
		})
	}
	return s
}

// String is a one-line description.
func (s Summary) String() string {
	return fmt.Sprintf("server %s, operator %s, %d node(s)", s.Server, s.Operator, len(s.Nodes))
}
