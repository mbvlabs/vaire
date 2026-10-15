package cli

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/spf13/cobra"
)

// The context's WireGuard endpoint after a rebuild (Phase 5, D6). A context
// dials the master's public address. After server init --restore on a new
// IP, the machine that ran it has the new address; another machine heals
// itself in acme mode by asking the public resolvers for the master's
// hostname, and anywhere else eru context set-address sets it. WireGuard
// authenticates the peer by its pinned key, so a wrong DNS answer can't
// impersonate the server.

// newEndpoint returns the address the public resolvers give for the
// master's hostname when it differs from the stored one, or "".
func newEndpoint(ctx context.Context, c *contexts.Context, stored string) string {
	if c.TLSMode != "acme" {
		return ""
	}
	host := c.MasterHostname()
	if host == "" {
		return ""
	}
	var answers [][]string
	for _, server := range publicResolvers {
		answers = append(answers, resolveWith(ctx, server, host).A)
	}
	return pickEndpoint(stored, answers...)
}

// pickEndpoint is the address every resolver that answered agrees on, when
// it differs from stored.
func pickEndpoint(stored string, answers ...[]string) string {
	var want string
	for _, a := range answers {
		if len(a) == 0 {
			continue
		}
		if len(a) != 1 || (want != "" && a[0] != want) {
			return ""
		}
		want = a[0]
	}
	if want == "" || want == stored {
		return ""
	}
	return want
}

// setMasterAddress stores addr (and port, when set) as c's master endpoint.
func setMasterAddress(c *contexts.Context, addr string, port int) (string, error) {
	old := ""
	for i := range c.Nodes {
		if c.Nodes[i].Role == contexts.RoleMaster || len(c.Nodes) == 1 {
			old = c.Nodes[i].PublicAddress
			c.Nodes[i].PublicAddress = addr
			if port > 0 {
				c.Nodes[i].ListenPort = port
			}
		}
	}
	return old, contexts.Save(c)
}

// healEndpoint is openSession's retry after the tunnel failed: true when it
// stored a new address and the session should be opened again.
func healEndpoint(ctx context.Context, c *contexts.Context, err error) (bool, output.Warning) {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || (cliErr.Code != output.CodeTransportFailed && cliErr.Code != output.CodeRemoteUnreachable) {
		return false, output.Warning{}
	}
	n, ok := c.Master()
	if !ok {
		return false, output.Warning{}
	}
	addr := newEndpoint(ctx, c, n.PublicAddress)
	if addr == "" {
		return false, output.Warning{}
	}
	stopTunnelDaemonQuietly(c.Name, true)
	old, serr := setMasterAddress(c, addr, 0)
	if serr != nil {
		return false, output.Warning{}
	}
	return true, output.Warning{Code: "context_endpoint_updated",
		Message: c.MasterHostname() + " now resolves to " + addr + " (was " + old + "): context " + c.Name + " was updated; the server was likely rebuilt"}
}

// endpointHealedKey marks a command that already healed its endpoint once,
// so a second failure doesn't loop.
const endpointHealedKey = "eru.endpoint_healed"

func endpointHealed(cmd *cobra.Command) bool { return cmd.Annotations[endpointHealedKey] == "1" }

func markEndpointHealed(cmd *cobra.Command) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[endpointHealedKey] = "1"
}

// parseEndpoint parses ip, ip:port or [ipv6]:port.
func parseEndpoint(v string) (string, int, error) {
	if ip, err := netip.ParseAddr(v); err == nil {
		return ip.Unmap().String(), 0, nil
	}
	host, portStr, err := net.SplitHostPort(v)
	if err != nil {
		return "", 0, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("port must be 1-65535")
	}
	return ip.Unmap().String(), port, nil
}

func runContextSetAddress(cmd *cobra.Command, args []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "name"))
	if err != nil {
		return err
	}
	addr, port, err := parseEndpoint(args[0])
	if err != nil {
		return output.NewError(output.CodeUsage, args[0]+" is not an IP address (ip, ip:port or [ipv6]:port): "+err.Error(), output.ExitUsage, "")
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]any{"context": c.Name, "public_address": addr, "dry_run": true},
			Message: "dry run: would point context " + c.Name + " at " + addr})
	}
	stopTunnelDaemonQuietly(c.Name, true)
	old, err := setMasterAddress(c, addr, port)
	if err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: map[string]any{"context": c.Name, "public_address": addr, "old_address": old},
		Message: "context " + c.Name + " now reaches the server's WireGuard at " + addr + " (was " + old + ")"})
}
