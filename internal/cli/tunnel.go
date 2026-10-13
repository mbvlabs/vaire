package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/spf13/cobra"
)

// Tunnel default local ports (§7.2).
const (
	defaultPgbouncerPort = 6543
	defaultPostgresPort  = 6544
	pgbouncerPort        = 6432
)

// listenLocal binds 127.0.0.1 on the first free port at or above start.
func listenLocal(start int) (net.Listener, int, error) {
	for port := start; port < start+50; port++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, fmt.Errorf("no free local port from %d", start)
}

// proxy forwards connections accepted on ln to remote through the tunnel.
func proxy(ctx context.Context, ln net.Listener, dialer transport.Dialer, remote string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			upstream, err := dialer.DialContext(ctx, "tcp", remote)
			if err != nil {
				return
			}
			defer func() { _ = upstream.Close() }()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
			go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
			<-done
		}()
	}
}

func runTunnel(cmd *cobra.Command, _ []string) error {
	target := stringFlag(cmd, "target")
	if target == "" {
		target = "pgbouncer"
	}
	remotePort := pgbouncerPort
	defaultPort := defaultPgbouncerPort
	if target == "postgres" {
		remotePort = pgPort
		defaultPort = defaultPostgresPort
	} else if target != "pgbouncer" {
		return output.NewError(output.CodeUsage, "--target must be pgbouncer or postgres", output.ExitUsage, "")
	}
	localPort, err := cmd.Flags().GetInt("local-port")
	if err != nil {
		return err
	}
	if localPort == 0 {
		localPort = defaultPort
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	ln, port, err := listenLocal(localPort)
	if err != nil {
		return output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Pass --local-port with a free port.")
	}
	defer func() { _ = ln.Close() }()
	ctx, stop := signal.NotifyContext(commandContext(cmd), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	remote := net.JoinHostPort(nodeWGAddr(s), strconv.Itoa(remotePort))
	go proxy(ctx, ln, s.tunnel, remote)

	dsn := fmt.Sprintf("postgres://<user>:<password>@127.0.0.1:%d/<db>", port)
	hint := "clients must set sslmode=require, or keep the hostname " + s.ctx.MasterHostname() + " so verify-full matches"
	opts, _ := output.ParseOptions(cmd)
	if opts.Stream || opts.Mode != output.ModeHuman {
		ready := output.Event{Schema: output.EventSchema, Type: "ready", Detail: fmt.Sprintf("127.0.0.1:%d -> %s", port, remote)}
		raw, _ := json.Marshal(ready)
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	} else if !opts.Quiet {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "tunnel ready: 127.0.0.1:%d -> %s:%d (%s)\nDSN: %s\n%s\nPress Ctrl-C to stop.\n", port, nodeWGAddr(s), remotePort, target, dsn, hint)
	}
	<-ctx.Done()
	_ = os.Stdout.Sync()
	return nil
}
