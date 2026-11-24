//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
)

type silentTunnel struct{}

func (silentTunnel) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestWhoamiSilentSharedTunnelHonorsConnectionTimeout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("VAIRE_TUNNEL", "")

	private, public, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	sshKey, _, err := transport.GenerateSSHKey("test")
	if err != nil {
		t.Fatal(err)
	}

	c := &workspaces.Workspace{
		Name: "prod", Server: "prod", Managed: true,
		Operator: workspaces.Operator{Name: "alice", WGAddress: "10.0.0.2", SSHUser: "vaire"},
		Nodes:    []workspaces.Node{{Name: "node", WGAddress: "10.0.0.1", PublicAddress: "192.0.2.1", WGPublicKey: public, ListenPort: 51820}},
	}
	if err := workspaces.Save(c); err != nil {
		t.Fatal(err)
	}

	if err := workspaces.WriteFile(c.Name, workspaces.WGKeyFile, []byte(private)); err != nil {
		t.Fatal(err)
	}

	if err := workspaces.WriteFile(c.Name, workspaces.SSHKeyFile, sshKey); err != nil {
		t.Fatal(err)
	}

	cfg, err := workspaceTunnelConfig(c, c.Nodes[0])
	if err != nil {
		t.Fatal(err)
	}

	socket, err := tunnelSocketPath(c.Name)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	srv := transport.NewSharedServer(transport.SharedServerConfig{
		Listener: ln, Dialer: silentTunnel{}, Allowed: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		Fingerprint: transport.TunnelFingerprint(cfg), Build: tunnelBuild(), Persist: time.Minute,
		DialTimeout: 10 * time.Second,
	})
	serveCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { srv.Serve(serveCtx); close(done) }()

	t.Cleanup(func() { stop(); <-done })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	root := NewRoot()
	root.SetContext(ctx)

	var out bytes.Buffer

	root.SetOut(&out)
	root.SetArgs([]string{"-w", "prod", "whoami", "--json", "--timeout", "100ms"})

	start := time.Now()
	err = root.Execute()

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected connection deadline, got %v", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("connection took %s despite 100ms timeout", elapsed)
	}

	if out.Len() != 0 || strings.Contains(strings.ToLower(err.Error()), "revoked") {
		t.Fatalf("silence must not claim an identity or revocation: output %q, error %v", out.String(), err)
	}
}
