package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

type enrollmentKeys struct {
	CodeHash string `json:"code_hash"`
	SSH      string `json:"ssh"`
	WG       string `json:"wg"`
}

// enrollmentPending stores keys before redemption so a lost response never
// replaces the identity already installed at the node.
func enrollmentPending(code string, setup rpc.AccessSetup) (enrollmentKeys, string, bool, error) {
	id := sha256.Sum256([]byte(setup.ID))
	hash := sha256.Sum256([]byte(code))
	dir := filepath.Join(workspaces.Root(), "enrollment")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return enrollmentKeys{}, "", false, err
	}

	path := filepath.Join(dir, hex.EncodeToString(id[:])+".json")

	read := func() (enrollmentKeys, error) {
		var k enrollmentKeys

		raw, err := os.ReadFile(path)
		if err != nil {
			return k, err
		}

		if err = json.Unmarshal(raw, &k); err != nil {
			return k, err
		}

		if k.CodeHash != hex.EncodeToString(hash[:]) {
			return k, fmt.Errorf("setup code differs from the pending enrollment")
		}

		return k, nil
	}
	if k, err := read(); err == nil {
		return k, path, true, nil
	} else if !os.IsNotExist(err) {
		return enrollmentKeys{}, "", false, err
	}

	wg, _, err := transport.GenerateWGKey()
	if err != nil {
		return enrollmentKeys{}, "", false, err
	}

	key, _, err := transport.GenerateSSHKey("vaire-" + setup.Label)
	if err != nil {
		return enrollmentKeys{}, "", false, err
	}

	k := enrollmentKeys{CodeHash: hex.EncodeToString(hash[:]), SSH: string(key), WG: wg}

	raw, err := json.Marshal(k)
	if err != nil {
		return k, "", false, err
	}

	f, err := os.CreateTemp(dir, ".enrollment-*")
	if err != nil {
		return k, "", false, err
	}

	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()

	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}

	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}

	if err != nil {
		return k, "", false, err
	}

	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			k, err = read()
			return k, path, true, err
		}

		return k, "", false, err
	}

	return k, path, false, nil
}

func enrollmentTunnel(setup rpc.AccessSetup, private, address string) (*transport.Tunnel, error) {
	cfg, err := tunnelConfig(private, address, setup.NodeWGPublicKey, setup.NodePublicAddress, setup.WGPort, setup.NodeAddress)
	if err != nil {
		return nil, err
	}

	node, err := netip.ParseAddr(setup.NodeAddress)
	if err != nil {
		return nil, err
	}

	cfg.AllowedIPs = []netip.Prefix{netip.PrefixFrom(node, node.BitLen())}

	return transport.UpWireGuard(cfg)
}

func redeemEnrollment(ctx context.Context, setup rpc.AccessSetup, keys enrollmentKeys, sent func()) error {
	tun, err := enrollmentTunnel(setup, setup.TemporaryPrivateKey, setup.TemporaryAddress)
	if err != nil {
		return err
	}

	defer tun.Close()

	conn, err := tun.DialContext(ctx, "tcp", net.JoinHostPort(setup.NodeAddress, strconv.Itoa(rpc.AccessEnrollPort)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	sshPub, err := transport.AuthorizedKey([]byte(keys.SSH), "")
	if err != nil {
		return err
	}

	wgPub, err := transport.WGPublicKey(keys.WG)
	if err != nil {
		return err
	}

	return sendEnrollment(conn, rpc.AccessRedeemParams{ID: setup.ID, Token: setup.Token, SSHPublicKey: sshPub, WGPublicKey: wgPub}, sent)
}

func sendEnrollment(conn net.Conn, params rpc.AccessRedeemParams, sent func()) error {
	if strings.TrimSpace(params.Token) == "" {
		return fmt.Errorf("invalid Vaire setup code")
	}

	if err := json.NewEncoder(conn).Encode(params); err != nil {
		return err
	}

	if sent != nil {
		sent()
	}

	var reply struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&reply); err != nil {
		return err
	}

	if !reply.OK {
		return fmt.Errorf("enrollment was rejected")
	}

	return nil
}

// The temporary peer can disappear before the server writes its reply. Start
// checking the permanent identity once the request is sent, rather than
// waiting for that reply to time out.
func redeemAndVerify(ctx context.Context, redeem func(context.Context, func()) error, verify func() error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sent := make(chan struct{}, 1)
	result := make(chan error, 1)

	go func() {
		result <- redeem(ctx, func() { sent <- struct{}{} })
	}()

	select {
	case redeemErr := <-result:
		if err := verify(); err != nil {
			return fmt.Errorf("enrollment not verified; retry with the same code: %w", errors.Join(redeemErr, err))
		}

		return nil
	case <-sent:
		if err := verify(); err == nil {
			cancel()
			<-result

			return nil
		}
	}

	redeemErr := <-result
	if err := verify(); err != nil {
		return fmt.Errorf("enrollment not verified; retry with the same code: %w", errors.Join(redeemErr, err))
	}

	return nil
}

func verifyEnrollment(ctx context.Context, setup rpc.AccessSetup, keys enrollmentKeys) error {
	return withEnrollmentNode(ctx, setup, keys, func(node *remote.Node) error {
		return verifyEnrollmentIdentity(ctx, node, setup)
	})
}

func withEnrollmentNode(ctx context.Context, setup rpc.AccessSetup, keys enrollmentKeys, use func(*remote.Node) error) error {
	tun, err := enrollmentTunnel(setup, keys.WG, setup.Address)
	if err != nil {
		return err
	}
	defer tun.Close()

	hostKeys, err := transport.ParseHostKeys([]string{setup.HostPublicKey})
	if err != nil {
		return err
	}

	known, err := transport.LoadKnownHosts("")
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(setup.NodeAddress, "22")
	known.Pin(addr, hostKeys...)

	auth, err := transport.KeyAuth([]byte(keys.SSH))
	if err != nil {
		return err
	}

	client, err := transport.DialSSH(ctx, tun, transport.SSHTarget{User: setup.UnixLogin, Addr: addr},
		transport.SSHOptions{Auth: []ssh.AuthMethod{auth}, HostKeys: known, Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	defer client.Close()

	return use(&remote.Node{SSH: client, Operator: setup.User})
}

func verifyEnrollmentIdentity(ctx context.Context, node *remote.Node, setup rpc.AccessSetup) error {
	reply, err := node.Call(ctx, "whoami", nil, remote.CallOptions{})
	if err != nil {
		return err
	}

	var identity rpc.WhoamiReply
	if err := reply.Decode(&identity); err != nil {
		return err
	}

	if identity.IdentityKind != rpc.IdentityVerifiedDevice || identity.Server != setup.Server || identity.Node != setup.Node ||
		identity.SSHAccount != setup.UnixLogin || identity.Operator != setup.User || identity.Device != setup.Label || identity.DeviceID != setup.ID ||
		(setup.Role != "" && identity.Role != setup.Role) {
		return fmt.Errorf("node returned an unexpected device identity")
	}

	return nil
}

func verifyOwnerEnrollment(ctx context.Context, setup rpc.AccessSetup, keys enrollmentKeys) error {
	setup.Role = "owner"

	return verifyEnrollment(ctx, setup, keys)
}

func loginEnrollment(cmd *cobra.Command, code string) error {
	return loginEnrollmentWithCredentials(cmd, code, nil)
}

func loginEnrollmentWithCredentials(cmd *cobra.Command, code string, credentials *workspaces.Credentials) error {
	secrets.Add(code)

	setup, err := rpc.DecodeAccessSetup(code)
	if err != nil {
		return err
	}

	if strings.TrimSpace(setup.Token) == "" {
		return fmt.Errorf("invalid Vaire setup code")
	}

	if credentials != nil && (setup.Mode != rpc.AccessSetupRecovery || setup.Role != "owner") {
		return fmt.Errorf("the recovery bundle does not enroll an owner")
	}

	if credentials == nil && setup.Mode == rpc.AccessSetupRecovery {
		return fmt.Errorf("use vaire recover with the encrypted owner recovery kit")
	}

	secrets.Add(setup.TemporaryPrivateKey)
	secrets.Add(setup.Token)

	name := setup.Server

	if err := workspaces.ValidateName(name); err != nil {
		return err
	}

	if err := checkEnrollmentWorkspace(name, setup); err != nil {
		return err
	}

	keys, path, resumed, err := enrollmentPending(code, setup)
	if err != nil {
		return err
	}

	secrets.Add(keys.WG)
	secrets.Add(keys.SSH)

	verify := func() error {
		ctx, cancel := context.WithTimeout(commandContext(cmd), timeoutFlag(cmd))
		defer cancel()

		return verifyEnrollment(ctx, setup, keys)
	}

	verified := resumed && verify() == nil
	if !verified {
		if !setup.ExpiresAt.IsZero() && time.Now().After(setup.ExpiresAt) {
			return fmt.Errorf("access setup code has expired; request a new code")
		}

		ctx, cancel := context.WithTimeout(commandContext(cmd), timeoutFlag(cmd))
		err := redeemAndVerify(ctx, func(ctx context.Context, sent func()) error {
			return redeemEnrollment(ctx, setup, keys, sent)
		}, verify)

		cancel()

		if err != nil {
			return err
		}
	}

	if err := installEnrollment(name, setup, keys, credentials); err != nil {
		return err
	}

	_ = os.Remove(path)
	current, _ := workspaces.Current()

	return output.Success(cmd, output.Result{Data: map[string]any{"workspace": name, "current": current == name, "path": workspaces.Dir(name)},
		Message: fmt.Sprintf("Connected to %s / %s as %s / %s.", name, setup.Node, setup.User, setup.Label)})
}

func installEnrollment(name string, setup rpc.AccessSetup, keys enrollmentKeys, credentials *workspaces.Credentials) error {
	if workspaces.Exists(name) {
		return addEnrollmentIdentity(name, setup, keys, credentials)
	}

	return saveEnrollmentWorkspace(name, setup, keys, credentials, "")
}

func checkEnrollmentWorkspace(name string, setup rpc.AccessSetup) error {
	if !workspaces.Exists(name) {
		return nil
	}

	existing, err := workspaces.LoadIdentity(name, "")
	if err != nil {
		return err
	}

	node, ok := existing.Master()
	if !existing.Managed || existing.Server != setup.Server || !ok ||
		node.Name != setup.Node || node.WGPublicKey != setup.NodeWGPublicKey {
		return fmt.Errorf("workspace %s already points to a different server", name)
	}

	host, _, _, _, err := ssh.ParseAuthorizedKey([]byte(setup.HostPublicKey))
	if err != nil {
		return err
	}

	known, err := transport.LoadKnownHosts(workspaces.IdentityPath(name, "", workspaces.KnownHostsFile))
	if err != nil {
		return err
	}

	if !slices.ContainsFunc(known.Keys(net.JoinHostPort(node.WGAddress, "22")), func(key ssh.PublicKey) bool {
		return ssh.FingerprintSHA256(key) == ssh.FingerprintSHA256(host)
	}) {
		return fmt.Errorf("workspace %s has a different SSH host key", name)
	}

	if existing.Operator.SSHUser == setup.UnixLogin {
		return fmt.Errorf("this device is already enrolled in workspace %s", name)
	}

	return nil
}

func addEnrollmentIdentity(name string, setup rpc.AccessSetup, keys enrollmentKeys, credentials *workspaces.Credentials) error {
	if err := checkEnrollmentWorkspace(name, setup); err != nil {
		return err
	}

	if setup.ID == "" {
		return fmt.Errorf("missing device ID")
	}

	if err := workspaces.ValidateIdentity(setup.ID); err != nil {
		return err
	}

	target := workspaces.IdentityDir(name, setup.ID)

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}

	stage, err := os.MkdirTemp(parent, ".enrollment-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := writeEnrollmentFiles(stage, setup, keys, credentials); err != nil {
		return err
	}

	if err := workspaces.SaveStagedEnrollment(stage, enrolledWorkspace(name, setup, "")); err != nil {
		return err
	}

	return os.Rename(stage, target)
}

func installOwnerEnrollment(name string, setup rpc.AccessSetup, keys enrollmentKeys, credentials workspaces.Credentials, lockedAt string) error {
	return saveEnrollmentWorkspace(name, setup, keys, &credentials, lockedAt)
}

func saveEnrollmentWorkspace(name string, setup rpc.AccessSetup, keys enrollmentKeys, credentials *workspaces.Credentials, lockedAt string) error {
	parent := filepath.Dir(workspaces.Dir(name))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}

	target := workspaces.Dir(name)
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("workspace %s already exists", name)
	} else if !os.IsNotExist(err) {
		return err
	}

	stage, err := os.MkdirTemp(parent, ".enrollment-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := writeEnrollmentFiles(stage, setup, keys, credentials); err != nil {
		return err
	}

	c := enrolledWorkspace(name, setup, lockedAt)
	if err := workspaces.SaveStagedEnrollment(stage, c); err != nil {
		return err
	}

	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("workspace %s already exists", name)
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.Rename(stage, target); err != nil {
		return err
	}

	if current, _ := workspaces.Current(); current == "" || !workspaces.Exists(current) {
		return workspaces.SetCurrent(name)
	}

	return nil
}

func writeEnrollmentFiles(stage string, setup rpc.AccessSetup, keys enrollmentKeys, credentials *workspaces.Credentials) error {
	hostKeys, err := transport.ParseHostKeys([]string{setup.HostPublicKey})
	if err != nil {
		return err
	}

	known, err := transport.LoadKnownHosts(filepath.Join(stage, workspaces.KnownHostsFile))
	if err != nil {
		return err
	}

	known.Pin(net.JoinHostPort(setup.NodeAddress, "22"), hostKeys...)

	if err := known.Save(); err != nil {
		return err
	}

	if err := transport.WriteSecretFile(filepath.Join(stage, workspaces.SSHKeyFile), []byte(keys.SSH)); err != nil {
		return err
	}

	if err := transport.WriteSecretFile(filepath.Join(stage, workspaces.WGKeyFile), []byte(keys.WG+"\n")); err != nil {
		return err
	}

	if credentials == nil {
		return nil
	}

	raw, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return err
	}

	return transport.WriteSecretFile(filepath.Join(stage, workspaces.CredentialsFile), append(raw, '\n'))
}

func enrolledWorkspace(name string, setup rpc.AccessSetup, lockedAt string) *workspaces.Workspace {
	now := time.Now().UTC().Format(time.RFC3339)
	if lockedAt == "" {
		lockedAt = now
	}

	hostname := setup.Node
	if setup.Domain != "" {
		hostname += "." + setup.Domain
	}

	return &workspaces.Workspace{Name: name, Server: setup.Server, Domain: setup.Domain, TLSMode: setup.TLSMode, Managed: true,
		Operator: workspaces.Operator{Name: setup.User, Device: setup.Label, SSHUser: setup.UnixLogin, WGAddress: setup.Address},
		Nodes: []workspaces.Node{{Name: setup.Node, Role: workspaces.RoleMaster, Hostname: hostname, PublicAddress: setup.NodePublicAddress,
			WGAddress: setup.NodeAddress, WGPublicKey: setup.NodeWGPublicKey, ListenPort: setup.WGPort}},
		Databases: map[string]string{}, LockdownConfirmedAt: lockedAt, CreatedAt: now, VaireVersion: buildinfo.Version}
}
