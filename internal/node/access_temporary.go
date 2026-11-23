package node

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/rpc"
)

var temporaryAccessUnitDir = "/etc/systemd/system"

var temporaryAccessCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}

	return string(out), nil
}

func temporaryIPv4(value string) (string, error) {
	a, err := netip.ParseAddr(value)
	if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() {
		return "", fmt.Errorf("invalid temporary access IPv4 address %q", value)
	}

	return a.String(), nil
}

// prepareTemporaryAccess installs the socket and expiry timer. Callers must
// serialize preparation and registry/peer changes with the node lock.
//
//nolint:gocognit // The ordered host operations and rollback are kept together.
func prepareTemporaryAccess(ctx context.Context, nodeAddress string) (retErr error) {
	nodeAddress, err := temporaryIPv4(nodeAddress)
	if err != nil {
		return err
	}

	units := map[string]string{
		"vaire-access-enroll.socket": fmt.Sprintf(`[Unit]
Description=Vaire temporary access enrollment
After=wg-quick@wg0.service
Requires=wg-quick@wg0.service

[Socket]
ListenStream=%s:%d
Accept=yes
MaxConnections=32
MaxConnectionsPerSource=4

[Install]
WantedBy=sockets.target
`, nodeAddress, rpc.AccessEnrollPort),
		"vaire-access-enroll@.service": `[Unit]
Description=Vaire temporary access enrollment connection

[Service]
ExecStart=/usr/local/bin/vaire node access-enroll
StandardInput=socket
StandardOutput=socket
StandardError=journal
RuntimeMaxSec=30s
`,
		"vaire-access-expire.service": `[Unit]
Description=Expire Vaire temporary access peers

[Service]
Type=oneshot
ExecStart=/usr/local/bin/vaire node access-expire
TimeoutStartSec=45s
`,
		"vaire-access-expire.timer": `[Unit]
Description=Expire Vaire temporary access peers every minute

[Timer]
OnBootSec=1min
OnUnitActiveSec=1min

[Install]
WantedBy=timers.target
`,
	}
	socketChanged := false

	var (
		oldSocket []byte
		hadSocket bool
	)

	defer func() {
		if retErr != nil && socketChanged {
			path := filepath.Join(temporaryAccessUnitDir, "vaire-access-enroll.socket")

			var err error
			if hadSocket {
				err = os.WriteFile(path, oldSocket, 0o644) //nolint:gosec // Systemd unit files are intentionally world-readable.
			} else {
				err = os.Remove(path)
			}

			if err != nil {
				retErr = fmt.Errorf("%w; restore socket unit: %w", retErr, err)
			}
		}
	}()

	for name, contents := range units {
		path := filepath.Join(temporaryAccessUnitDir, name)

		old, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", name, err)
		}

		if string(old) == contents {
			continue
		}

		if name == "vaire-access-enroll.socket" {
			socketChanged, oldSocket, hadSocket = true, old, err == nil
		}

		if err := os.WriteFile(filepath.Join(temporaryAccessUnitDir, name), []byte(contents), 0o644); err != nil { //nolint:gosec // Systemd unit files are intentionally world-readable.
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	// Reload even on retry after a partially completed unit update.
	if _, err := temporaryAccessCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}

	if _, err := temporaryAccessCommand(ctx, "ufw", "allow", "in", "on", "wg0", "to", "any", "port", strconv.Itoa(rpc.AccessEnrollPort), "proto", "tcp"); err != nil {
		return err
	}
	// Restart the socket so a changed node address is actually used.
	if _, err := temporaryAccessCommand(ctx, "systemctl", "enable", "--now", "vaire-access-expire.timer"); err != nil {
		return err
	}

	if _, err := temporaryAccessCommand(ctx, "systemctl", "enable", "--now", "vaire-access-enroll.socket"); err != nil {
		return err
	}

	if socketChanged && hadSocket {
		if _, err := temporaryAccessCommand(ctx, "systemctl", "restart", "vaire-access-enroll.socket"); err != nil {
			return err
		}
	}

	return nil
}

func temporaryPeerInputs(nodeAddress, address, publicKey string) (string, string, error) {
	nodeAddress, err := temporaryIPv4(nodeAddress)
	if err != nil {
		return "", "", err
	}

	address, err = temporaryIPv4(address)
	if err != nil || address == nodeAddress {
		return "", "", fmt.Errorf("invalid temporary peer address %q", address)
	}

	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != publicKey {
		return "", "", fmt.Errorf("invalid temporary peer public key")
	}

	return nodeAddress, address, nil
}

// Each rule drops a different mismatch. Together they admit only TCP packets
// to the node's enrollment socket in raw PREROUTING, before UFW's filter
// rules, including established traffic. A separate security-table FORWARD
// drop handles DNAT and policy routing. UFW reloads flush neither table.
type temporaryGuard struct {
	table, chain string
	rule         []string
}

func temporaryDropRules(nodeAddress, address string) []temporaryGuard {
	base := []string{"-i", "wg0", "-s", address + "/32"}
	rule := func(extra ...string) []string {
		r := append([]string{}, base...)
		r = append(r, extra...)

		return append(r, "-j", "DROP")
	}

	return []temporaryGuard{
		{"security", "FORWARD", rule()},
		{"raw", "PREROUTING", rule("!", "-d", nodeAddress+"/32")},
		{"raw", "PREROUTING", rule("!", "-p", "tcp")},
		{"raw", "PREROUTING", rule("-p", "tcp", "!", "--dport", strconv.Itoa(rpc.AccessEnrollPort))},
	}
}

func temporaryGuardArgs(g temporaryGuard, action string) []string {
	return append([]string{"-w", "-t", g.table, action, g.chain}, g.rule...)
}

//nolint:gocognit // Peer validation and fail-closed guard repair must stay ordered.
func addTemporaryAccessPeer(ctx context.Context, nodeAddress, address, publicKey string) error {
	nodeAddress, address, err := temporaryPeerInputs(nodeAddress, address, publicKey)
	if err != nil {
		return err
	}
	// Never replace another live peer or steal an existing allowed IP. The
	// registry also reserves permanent and revoked identities under the lock.
	out, err := temporaryAccessCommand(ctx, "wg", "show", "wg0", "allowed-ips")
	if err != nil {
		return err
	}

	exists := false

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		if fields[0] == publicKey {
			if len(fields) != 2 || fields[1] != address+"/32" {
				return fmt.Errorf("WireGuard peer has unexpected allowed IPs")
			}

			exists = true

			continue
		}

		for _, item := range fields[1:] {
			prefix, e := netip.ParsePrefix(strings.TrimSuffix(item, ","))
			if e == nil && prefix.Contains(netip.MustParseAddr(address)) {
				return fmt.Errorf("WireGuard address already in use")
			}
		}
	}

	guards := temporaryDropRules(nodeAddress, address)

	missing := make([]temporaryGuard, 0, len(guards))
	for _, guard := range guards {
		if _, err := temporaryAccessCommand(ctx, "iptables", temporaryGuardArgs(guard, "-C")...); err != nil {
			missing = append(missing, guard)
		}
	}

	if exists && len(missing) > 0 {
		// Do not leave an active peer unrestricted if repair fails.
		if _, err := temporaryAccessCommand(ctx, "wg", "set", "wg0", "peer", publicKey, "remove"); err != nil {
			return fmt.Errorf("remove unguarded temporary peer: %w", err)
		}

		exists = false
	}

	for _, guard := range missing {
		if _, err := temporaryAccessCommand(ctx, "iptables", temporaryGuardArgs(guard, "-I")...); err != nil {
			return err
		}
	}

	if exists {
		return nil
	}

	_, err = temporaryAccessCommand(ctx, "wg", "set", "wg0", "peer", publicKey, "allowed-ips", address+"/32")

	return err
}

func removeTemporaryAccessPeer(ctx context.Context, nodeAddress, address, publicKey string) error {
	nodeAddress, address, err := temporaryPeerInputs(nodeAddress, address, publicKey)
	if err != nil {
		return err
	}

	out, err := temporaryAccessCommand(ctx, "wg", "show", "wg0", "allowed-ips")
	if err != nil {
		return err
	}

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == publicKey &&
			(len(fields) != 2 || fields[1] != address+"/32") {
			return fmt.Errorf("WireGuard peer has unexpected allowed IPs")
		}
	}
	// Keep every drop rule in place unless peer removal has succeeded.
	if _, err := temporaryAccessCommand(ctx, "wg", "set", "wg0", "peer", publicKey, "remove"); err != nil {
		return err
	}

	for _, guard := range temporaryDropRules(nodeAddress, address) {
		if _, err := temporaryAccessCommand(ctx, "iptables", temporaryGuardArgs(guard, "-C")...); err == nil {
			if _, err := temporaryAccessCommand(ctx, "iptables", temporaryGuardArgs(guard, "-D")...); err != nil {
				return err
			}
		} else if _, err := temporaryAccessCommand(ctx, "iptables", "-w", "-t", guard.table, "-S", guard.chain); err != nil {
			// Distinguish an absent rule from a broken/unavailable table.
			return err
		}
	}

	return nil
}
