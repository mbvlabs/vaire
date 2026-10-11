// Package transport is how the operator reaches a node: the embedded userspace
// WireGuard tunnel, a direct dialer for first contact and --local, the
// in-process SSH client, and host-key pinning (§7.2, §7.3).
package transport

import (
	"context"
	"net"
	"time"
)

// Dialer opens a stream connection. The WireGuard tunnel and the direct
// network both implement it, so the SSH client does not care which it uses.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// DefaultTimeout bounds a dial or SSH handshake when the caller sets none.
const DefaultTimeout = 15 * time.Second

// Direct dials through the host network stack: first contact over public SSH,
// and the operator running on the node itself.
type Direct struct {
	Timeout time.Duration
}

// DialContext dials addr with the host network stack.
func (d Direct) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, addr)
}
