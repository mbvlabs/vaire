package pg

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/mbvlabs/eru/internal/transport"
)

// Protocol request codes a client sends before its startup message.
const (
	sslRequestCode    = 80877103
	gssencRequestCode = 80877104
	maxStartupLen     = 10000
)

// TLSConfig is the client TLS config for p: verify-full against system roots
// with ServerName, or encryption without verification for "require".
func TLSConfig(p Params) *tls.Config { return tlsConfig(p) }

// ServeTLSProxy accepts plaintext PostgreSQL connections on ln and forwards
// each one to remote through dialer, doing the TLS handshake itself with cfg.
// A client on loopback can then use sslmode=disable and still get a verified
// TLS connection on the wire, without having to match the certificate's
// hostname against 127.0.0.1. It returns when ln is closed.
func ServeTLSProxy(ctx context.Context, ln net.Listener, dialer transport.Dialer, remote string, cfg *tls.Config) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_ = proxyTLS(ctx, conn, dialer, remote, cfg)
		}()
	}
}

func proxyTLS(ctx context.Context, client net.Conn, dialer transport.Dialer, remote string, cfg *tls.Config) error {
	// Decline SSL and GSS encryption on the loopback leg until the client
	// sends its startup (or cancel) message.
	var first []byte
	for range 3 {
		msg, err := readStartup(client)
		if err != nil {
			return err
		}
		code := binary.BigEndian.Uint32(msg[4:8])
		if len(msg) == 8 && (code == sslRequestCode || code == gssencRequestCode) {
			if _, err := client.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		}
		first = msg
		break
	}
	if first == nil {
		return errors.New("client sent no startup message")
	}

	upstream, err := dialer.DialContext(ctx, "tcp", remote)
	if err != nil {
		return err
	}
	defer func() { _ = upstream.Close() }()
	req := make([]byte, 8)
	binary.BigEndian.PutUint32(req[0:4], 8)
	binary.BigEndian.PutUint32(req[4:8], sslRequestCode)
	if _, err := upstream.Write(req); err != nil {
		return err
	}
	answer := make([]byte, 1)
	if _, err := io.ReadFull(upstream, answer); err != nil {
		return err
	}
	if answer[0] != 'S' {
		return errors.New("the server refused TLS")
	}
	secure := tls.Client(upstream, cfg)
	if err := secure.HandshakeContext(ctx); err != nil {
		return err
	}
	if _, err := secure.Write(first); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(secure, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, secure); done <- struct{}{} }()
	<-done
	return nil
}

// readStartup reads one length-prefixed message that has no type byte.
func readStartup(r io.Reader) ([]byte, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(head)
	if n < 8 || n > maxStartupLen {
		return nil, fmt.Errorf("bad startup message length %d", n)
	}
	msg := make([]byte, n)
	copy(msg, head)
	if _, err := io.ReadFull(r, msg[4:]); err != nil {
		return nil, err
	}
	return msg, nil
}
