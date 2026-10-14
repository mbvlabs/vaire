package transport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// TunnelMTU is the netstack interface MTU (§7.2).
const TunnelMTU = 1420

// KeepaliveSeconds keeps NAT mappings open while a command runs.
const KeepaliveSeconds = 25

// WireGuardConfig is one operator-to-node tunnel.
type WireGuardConfig struct {
	// PrivateKey is the operator's key in wg base64 form.
	PrivateKey string
	// Address is the operator's tunnel address, for example 10.42.255.1.
	Address netip.Addr
	// PeerPublicKey is the node's key in wg base64 form.
	PeerPublicKey string
	// Endpoint is the node's public address and WireGuard port.
	Endpoint netip.AddrPort
	// AllowedIPs are routed through the tunnel, normally 10.42.0.0/24.
	AllowedIPs []netip.Prefix
	// Logf receives WireGuard diagnostics for --verbose. Nil discards them.
	Logf func(format string, args ...any)
}

// Tunnel is an embedded userspace WireGuard device with a netstack. It needs
// no root, no kernel interface and no system configuration. It lives for one
// command and is closed on exit.
type Tunnel struct {
	dev     *device.Device
	net     *netstack.Net
	onClose []func()
}

// UpWireGuard brings up a tunnel. WireGuard handshakes lazily on the first
// packet, so a bad endpoint shows up as a dial timeout, not here.
func UpWireGuard(cfg WireGuardConfig) (*Tunnel, error) {
	priv, err := keyHex(cfg.PrivateKey)
	if err != nil {
		return nil, wgError("the operator WireGuard private key is invalid", err)
	}
	peer, err := keyHex(cfg.PeerPublicKey)
	if err != nil {
		return nil, wgError("the node WireGuard public key is invalid", err)
	}
	if !cfg.Address.IsValid() || !cfg.Endpoint.IsValid() {
		return nil, wgError("the tunnel address or endpoint is invalid", errors.New("invalid address"))
	}
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{cfg.Address}, nil, TunnelMTU)
	if err != nil {
		return nil, wgError("could not create the userspace tunnel", err)
	}
	logger := &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf}
	if cfg.Logf != nil {
		logger = &device.Logger{Verbosef: device.DiscardLogf, Errorf: cfg.Logf}
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), logger)
	var ipc strings.Builder
	fmt.Fprintf(&ipc, "private_key=%s\n", priv)
	fmt.Fprintf(&ipc, "public_key=%s\n", peer)
	fmt.Fprintf(&ipc, "endpoint=%s\n", cfg.Endpoint)
	fmt.Fprintf(&ipc, "persistent_keepalive_interval=%d\n", KeepaliveSeconds)
	for _, prefix := range cfg.AllowedIPs {
		fmt.Fprintf(&ipc, "allowed_ip=%s\n", prefix)
	}
	if err := dev.IpcSet(ipc.String()); err != nil {
		dev.Close()
		return nil, wgError("could not configure the userspace tunnel", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, wgError("could not start the userspace tunnel", err)
	}
	return &Tunnel{dev: dev, net: tnet}, nil
}

// DialContext dials addr inside the tunnel.
func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.net.DialContext(ctx, network, addr)
}

// OnClose runs fn once the tunnel is torn down, for example to release a
// lock held for the tunnel's lifetime.
func (t *Tunnel) OnClose(fn func()) {
	t.onClose = append(t.onClose, fn)
}

// Close tears the tunnel down.
func (t *Tunnel) Close() {
	if t != nil && t.dev != nil {
		t.dev.Close()
		t.dev = nil
		for _, fn := range t.onClose {
			fn()
		}
		t.onClose = nil
	}
}

func wgError(message string, cause error) *output.CLIError {
	return output.NewError(
		output.CodeTransportFailed,
		message+": "+cause.Error(),
		output.ExitTransport,
		"Check the context's WireGuard keys.",
	)
}

func keyHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", err
	}
	if len(raw) != curve25519.ScalarSize {
		return "", fmt.Errorf("key is %d bytes, want 32", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// GenerateWGKey returns a new WireGuard key pair in wg base64 form.
func GenerateWGKey() (private, public string, err error) {
	var k [curve25519.ScalarSize]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", err
	}
	// Clamp as wg genkey does.
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	private = base64.StdEncoding.EncodeToString(k[:])
	public, err = WGPublicKey(private)
	return private, public, err
}

// WGPublicKey derives the public key from a wg base64 private key.
func WGPublicKey(private string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(private))
	if err != nil || len(raw) != curve25519.ScalarSize {
		return "", errors.New("invalid WireGuard private key")
	}
	pub, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}
