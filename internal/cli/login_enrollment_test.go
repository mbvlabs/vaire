package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secretsdoc"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

func TestEnrollmentRedeemSendsBearer(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	got := make(chan rpc.AccessRedeemParams, 1)

	go func() {
		var p rpc.AccessRedeemParams

		_ = json.NewDecoder(server).Decode(&p)
		got <- p

		_, _ = server.Write([]byte("{\"ok\":true}\n"))
	}()

	want := rpc.AccessRedeemParams{ID: "invite", Token: "private-token", SSHPublicKey: "ssh", WGPublicKey: "wg"}
	if err := sendEnrollment(client, want, nil); err != nil {
		t.Fatal(err)
	}

	if p := <-got; p != want {
		t.Fatalf("redeem parameters: %+v", p)
	}

	if err := sendEnrollment(client, rpc.AccessRedeemParams{}, nil); err == nil {
		t.Fatal("accepted empty bearer token")
	}
}

func TestRedeemVerifiesBeforeTemporaryReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := redeemAndVerify(ctx, func(ctx context.Context, sent func()) error {
		sent()
		<-ctx.Done()

		return ctx.Err()
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	if ctx.Err() != nil {
		t.Fatal("waited for temporary peer timeout")
	}
}

func TestRedeemRetriesVerificationAfterProvisioning(t *testing.T) {
	ready := make(chan struct{})
	attempts := 0

	err := redeemAndVerify(context.Background(), func(_ context.Context, sent func()) error {
		sent()
		<-ready

		return nil
	}, func() error {
		attempts++
		if attempts == 1 {
			close(ready)
			return errors.New("not provisioned yet")
		}

		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("verification attempts %d, error %v", attempts, err)
	}
}

func TestEnrollmentRejectsMissingBearerBeforeSavingKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	setup := rpc.AccessSetup{Version: 1, ID: "one", Server: "prod", ExpiresAt: time.Now().Add(time.Hour)}

	code, err := setup.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := loginEnrollment(&cobra.Command{}, code); err == nil {
		t.Fatal("accepted setup without a bearer")
	}

	if _, err := os.Stat(workspaces.Root()); !os.IsNotExist(err) {
		t.Fatalf("missing bearer created local state: %v", err)
	}
}

func TestManagedSecretsExportRequiresUnlockAndDoesNotExportDeviceKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(workspaces.EnvWorkspace, "")

	c := &workspaces.Workspace{Name: "prod", Server: "prod", Managed: true}
	if err := workspaces.Save(c); err != nil {
		t.Fatal(err)
	}

	if _, err := workspaceDocument(c); err == nil {
		t.Fatal("managed workspace produced a secrets document")
	}

	root := NewRoot()

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"secrets", "export", "-w", "prod", "--json"})

	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "needs the master password") {
		t.Fatalf("managed export: %v, output %s", err, &out)
	}
}

func TestReadRecoveryPaste(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"handoff\n" + rpc.AccessSetupPrefix + "example\n", ""},
		{"handoff\n" + secretsdoc.ArmorHeader + "\nbody\n" + secretsdoc.ArmorFooter + "\n", secretsdoc.ArmorHeader + "\nbody\n" + secretsdoc.ArmorFooter + "\n"},
	} {
		got, err := readRecoveryPaste(strings.NewReader(tc.input))
		if err != nil || string(got) != tc.want {
			t.Fatalf("got %q, %v; want %q", got, err, tc.want)
		}
	}
}

func TestLoginRejectsRecoveryKit(t *testing.T) {
	root := NewRoot()
	root.SetArgs([]string{"login"})
	root.SetIn(strings.NewReader(secretsdoc.ArmorHeader + "\n"))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "expected a one-time device setup code") {
		t.Fatalf("login accepted a recovery kit: %v", err)
	}
}

func TestEnrollmentPendingPreservesKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	setup := rpc.AccessSetup{ID: "one", Label: "laptop"}

	a, path, resumed, err := enrollmentPending("code", setup)
	if err != nil || resumed {
		t.Fatalf("first pending: %v, resumed %v", err, resumed)
	}

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pending mode: %v %v", info, err)
	}

	b, _, resumed, err := enrollmentPending("code", setup)
	if err != nil || !resumed || a != b {
		t.Fatalf("resume: %v, resumed %v", err, resumed)
	}

	if _, _, _, err := enrollmentPending("different", setup); err == nil {
		t.Fatal("accepted a different setup code for the same ID")
	}
}

func TestInstallEnrollmentNeverOverwrites(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	host, hostPub, err := transport.GenerateSSHKey("host")
	if err != nil || len(host) == 0 {
		t.Fatal(err)
	}

	sshKey, _, err := transport.GenerateSSHKey("device")
	if err != nil {
		t.Fatal(err)
	}

	wg, wgPub, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	setup := rpc.AccessSetup{Server: "prod", Node: "primary", User: "alice", Label: "laptop",
		UnixLogin: "vdev-test", Address: "10.42.255.2", NodeAddress: "10.42.0.1",
		NodePublicAddress: "192.0.2.1", NodeWGPublicKey: wgPub, HostPublicKey: hostPub, WGPort: 51820, ExpiresAt: time.Now().Add(time.Hour)}

	keys := enrollmentKeys{SSH: string(sshKey), WG: wg}
	if err := installEnrollment("prod", setup, keys, nil); err != nil {
		t.Fatal(err)
	}

	c, err := workspaces.Load("prod")
	if err != nil || !c.Managed || c.LockdownConfirmedAt == "" || c.Operator.SSHUser != setup.UnixLogin {
		t.Fatalf("workspace: %+v, %v", c, err)
	}

	if err := installEnrollment("prod", setup, keys, nil); err == nil {
		t.Fatal("overwrote workspace")
	}

	if _, err := workspaces.Load("prod"); err != nil {
		t.Fatal(err)
	}

	known, err := transport.LoadKnownHosts(workspaces.Path("prod", workspaces.KnownHostsFile))
	if err != nil || len(known.Keys("10.42.0.1:22")) != 1 {
		t.Fatalf("host pin: %v", err)
	}
}
