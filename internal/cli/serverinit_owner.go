package cli

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"golang.org/x/crypto/ssh"
)

// bootstrapOwner persists the new device keys before making the idempotent
// RPC. Neither key is included in the legacy recovery handoff.
func (r *initRun) bootstrapOwner(ctx context.Context, caller remote.Caller) (rpc.AccessSetup, enrollmentKeys, error) {
	var setup rpc.AccessSetup

	if r.sec.OwnerSSHPrivateKey == "" && r.sec.OwnerWGPrivateKey == "" {
		wg, _, err := transport.GenerateWGKey()
		if err != nil {
			return setup, enrollmentKeys{}, err
		}

		key, _, err := transport.GenerateSSHKey("vaire-setup-device")
		if err != nil {
			return setup, enrollmentKeys{}, err
		}

		r.sec.OwnerSSHPrivateKey, r.sec.OwnerWGPrivateKey = string(key), wg
		registerPending(r.sec)

		if err := workspaces.SaveSecrets(r.name, r.sec); err != nil {
			return setup, enrollmentKeys{}, err
		}
	}

	keys := enrollmentKeys{SSH: r.sec.OwnerSSHPrivateKey, WG: r.sec.OwnerWGPrivateKey}

	sshPub, err := transport.AuthorizedKey([]byte(keys.SSH), "")
	if err != nil {
		return setup, keys, err
	}

	wgPub, err := transport.WGPublicKey(keys.WG)
	if err != nil {
		return setup, keys, err
	}

	reply, err := r.p.call(ctx, caller, "access.bootstrap-owner", rpc.AccessBootstrapOwnerParams{
		User: r.sec.OperatorName, SSHPublicKey: sshPub, WGPublicKey: wgPub,
	}, remote.CallOptions{OperationID: r.prog.SetupOperationID, OnEvent: r.p.event})
	if err != nil {
		return setup, keys, err
	}

	if err := reply.Decode(&setup); err != nil {
		return setup, keys, err
	}

	if err := r.validateOwnerSetup(setup); err != nil {
		return setup, keys, err
	}

	verifyCtx, cancel := context.WithTimeout(ctx, timeoutFlag(r.cmd))
	defer cancel()

	if err := verifyOwnerEnrollment(verifyCtx, setup, keys); err != nil {
		return setup, keys, fmt.Errorf("owner device not verified; resume server init with the same pending keys: %w", err)
	}

	return setup, keys, nil
}

// The bootstrap connection is already pinned; do not let a malformed reply
// redirect verification to another endpoint or host key.
func (r *initRun) validateOwnerSetup(s rpc.AccessSetup) error {
	n := r.master()
	if s.ID == "" || s.Label != "setup-device" || s.UnixLogin == "" || s.Address == "" ||
		s.Server != r.name || s.Node != n.Name || s.User != r.sec.OperatorName ||
		s.Domain != r.cfg.Domain || s.TLSMode != r.cfg.TLS.Mode ||
		s.NodeAddress != n.WGAddress || s.NodePublicAddress != n.PublicAddress ||
		s.NodeWGPublicKey != r.prog.WGPublicKey || s.WGPort != r.cfg.Transport.ListenPort {
		return fmt.Errorf("owner bootstrap returned unexpected node identity")
	}

	host, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.HostPublicKey))
	if err != nil {
		return fmt.Errorf("owner bootstrap returned invalid host key: %w", err)
	}

	for _, pinned := range r.prog.SSHHostKeys {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pinned))
		if err == nil && bytes.Equal(key.Marshal(), host.Marshal()) {
			return nil
		}
	}

	return fmt.Errorf("owner bootstrap returned an unpinned host key")
}

// A completed atomic install may precede a failed SetCurrent or pending
// cleanup. Accept only the exact device and credentials from this init.
func (r *initRun) installOrResumeOwner(setup rpc.AccessSetup, keys enrollmentKeys, creds workspaces.Credentials, lockedAt string) error {
	if !workspaces.Exists(r.name) {
		return installOwnerEnrollment(r.name, setup, keys, creds, lockedAt)
	}

	ws, err := workspaces.Load(r.name)
	if err != nil {
		return err
	}

	sshKey, err := workspaces.ReadFile(r.name, workspaces.SSHKeyFile)
	if err != nil {
		return err
	}

	wgKey, err := workspaces.ReadFile(r.name, workspaces.WGKeyFile)
	if err != nil {
		return err
	}

	saved, err := workspaces.LoadCredentials(r.name)
	if err != nil {
		return err
	}

	n, ok := ws.Master()
	if !ws.Managed || ws.Server != setup.Server || ws.Operator.Name != setup.User ||
		ws.Operator.SSHUser != setup.UnixLogin || ws.Operator.WGAddress != setup.Address ||
		!ok || n.Name != setup.Node || n.WGPublicKey != setup.NodeWGPublicKey ||
		string(sshKey) != keys.SSH || strings.TrimSpace(string(wgKey)) != keys.WG ||
		!reflect.DeepEqual(saved, creds) {
		return fmt.Errorf("workspace %s appeared during init and does not match the pending owner device", r.name)
	}

	return nil
}
