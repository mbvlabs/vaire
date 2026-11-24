package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/secretsdoc"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

func (rr *restoreRun) validateKitConfig(cfg config.Cluster) error {
	old, _ := rr.ctx.Master()
	matched := false

	for _, n := range cfg.Nodes {
		if n.Role == config.RoleMaster && n.Name == old.Name && n.WGAddress == old.WGAddress {
			matched = true
		}
	}

	b := rr.kitCreds.Backup
	if cfg.BackupConfigured() && (b.Bucket == "" || b.AccessKeyID == "" || b.SecretAccessKey == "" || b.PgBackRest.CipherPass == "") {
		return output.NewError(output.CodePrecondition, "recovery kit lacks backup target credentials", output.ExitPrecondition,
			"Use a complete recovery kit for this backup target.")
	}

	if !matched || cfg.Domain != rr.ctx.Domain || cfg.TLS.Mode != rr.ctx.TLSMode ||
		cfg.Transport.ListenPort != old.ListenPort ||
		cfg.Backup.Provider != b.Provider || cfg.Backup.Endpoint != b.Endpoint ||
		cfg.Backup.Region != b.Region || cfg.Backup.Bucket != b.Bucket ||
		cfg.Backup.Prefix != b.Prefix || cfg.Backup.URIStyle != b.URIStyle {
		return output.NewError(output.CodePrecondition, "bundle configuration does not match the recovery kit", output.ExitPrecondition,
			"Use the recovery kit for this bundle and backup target.")
	}

	return nil
}

// The kit is an offline source of identity and credentials, not an access code.
func newKitRestoreRun(cmd *cobra.Command) (*initRun, *restoreRun, *workspaces.BackupCreds, error) { //nolint:gocognit,gocyclo // Validate the offline kit before preparing a rebuild.
	raw, err := recoveryBundle(cmd, []string{stringFlag(cmd, "recovery-kit")})
	if err != nil {
		return nil, nil, nil, err
	}

	pass, err := loginPassphrase(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	secrets.Add(pass)

	stop := waitOn(cmd, "decrypting the recovery kit")
	doc, err := secretsdoc.Decrypt(raw, pass)

	stop()

	if err != nil {
		return nil, nil, nil, err
	}

	for _, v := range doc.SecretValues() {
		secrets.Add(v)
	}

	name := doc.Server.Name
	if err := workspaces.ValidateName(name); err != nil {
		return nil, nil, nil, err
	}

	for _, selected := range []string{stringFlag(cmd, "name"), stringFlag(cmd, "workspace")} {
		if selected != "" && selected != name {
			return nil, nil, nil, fmt.Errorf("recovery kit is for server %s, not %s", name, selected)
		}
	}

	if doc.Operator.Name == "" || doc.Backup == nil || doc.Backup.Encryption.Identity == "" {
		return nil, nil, nil, fmt.Errorf("recovery kit lacks the operator or backup recovery identity")
	}

	c := &workspaces.Workspace{Name: name, Server: name, Domain: doc.Server.Domain, TLSMode: doc.Server.TLSMode,
		Operator: workspaces.Operator{Name: doc.Operator.Name}}

	var hostKeys []string

	masters := 0

	for _, n := range doc.Nodes {
		c.Nodes = append(c.Nodes, workspaces.Node{Name: n.Name, Role: n.Role, Hostname: n.Hostname,
			PublicAddress: n.PublicAddress, WGAddress: n.WGAddress, WGPublicKey: n.WGPublicKey, ListenPort: n.ListenPort})
		if n.Role == workspaces.RoleMaster {
			masters++
			hostKeys = n.SSHHostKeys
		}
	}

	if masters != 1 || len(hostKeys) == 0 {
		return nil, nil, nil, fmt.Errorf("recovery kit must pin exactly one master and its host keys")
	}

	old, _ := c.Master()
	if old.Name == "" || old.WGPublicKey == "" || old.WGAddress == "" {
		return nil, nil, nil, fmt.Errorf("recovery kit has an incomplete master identity")
	}

	if _, err := transport.ParseHostKeys(hostKeys); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid recovery kit host keys: %w", err)
	}

	sum := sha256.Sum256(raw)
	creds := doc.RecoveryCredentials()
	rr := &restoreRun{ctx: c, identity: doc.Backup.Encryption.Identity, kitCreds: &creds,
		kitHash: hex.EncodeToString(sum[:]), kitHostKeys: hostKeys}

	r := &initRun{cmd: cmd, p: newProgress(cmd), name: name, restore: rr}

	_, targetErr := os.Lstat(workspaces.Dir(name))
	if targetErr != nil && !os.IsNotExist(targetErr) {
		return nil, nil, nil, targetErr
	}

	if targetErr == nil && !workspaces.PendingExists(name) {
		return nil, nil, nil, fmt.Errorf("workspace %s already exists; restore it without --recovery-kit", name)
	}

	if err := r.restoreLoadPending(); err != nil {
		return nil, nil, nil, err
	}

	if targetErr == nil {
		if err := r.checkKitRestoreTarget(); err != nil {
			return nil, nil, nil, err
		}
	}

	if rr.state.KitHash == "" {
		rr.state.KitHash = rr.kitHash
	}

	if !r.prog.Done(phaseOldChecked) && !flagTrue(cmd, "old-master-unreachable") {
		return nil, nil, nil, oldMasterCheckUnavailable()
	}

	if flagTrue(cmd, "resume") {
		if host := stringFlag(cmd, "host"); host != "" {
			u, h, p, err := parseHostFlag(host)
			if err != nil || u != r.boot.User || h != r.boot.Host || p != r.boot.Port {
				return nil, nil, nil, fmt.Errorf("--host differs from the pending rebuild")
			}
		}

		if flagChanged(cmd, "bundle") || flagChanged(cmd, "bundle-file") || flagChanged(cmd, "host-key-fingerprint") {
			return nil, nil, nil, fmt.Errorf("resume uses the pending bundle and pinned host fingerprint")
		}
	}

	return r, rr, doc.Backup, nil
}

func (r *initRun) checkKitRestoreTarget() error {
	raw, err := os.ReadFile(filepath.Join(workspaces.PendingDir(r.name), restoreOwnerFile))
	if err != nil {
		return fmt.Errorf("workspace %s appeared before restore owner was saved", r.name)
	}

	var setup rpc.AccessSetup
	if err := json.Unmarshal(raw, &setup); err != nil {
		return err
	}

	c, err := workspaces.Load(r.name)
	if err != nil {
		return err
	}

	if !c.Managed || c.Server != r.name || setup.Server != r.name || c.Operator.Name != r.restore.ctx.Operator.Name ||
		setup.User != c.Operator.Name || c.Operator.SSHUser != setup.UnixLogin || c.Operator.WGAddress != setup.Address {
		return fmt.Errorf("workspace %s does not match the pending restore owner", r.name)
	}

	keys := enrollmentKeys{SSH: r.sec.OwnerSSHPrivateKey, WG: r.sec.OwnerWGPrivateKey}
	if err := r.checkInstalledRestoreOwner(c, setup, keys); err != nil {
		return err
	}

	creds, err := workspaces.LoadCredentials(r.name)
	if err != nil {
		return err
	}

	expected := workspaces.Credentials{Database: r.sec.Database, Backup: r.sec.Backup, Metrics: r.sec.Metrics, MasterPasswordHash: r.sec.MasterPasswordHash}
	if !reflect.DeepEqual(creds, expected) {
		return fmt.Errorf("installed restore owner credentials differ from pending credentials")
	}

	return nil
}

// Verify the actual private identities in the bundle against the kit, before
// fencing or contacting the replacement host.
func (rr *restoreRun) validateKitBundle(b masterbackup.Bundle) error {
	old, _ := rr.ctx.Master()
	if b.Manifest.Node != old.Name {
		return fmt.Errorf("bundle node does not match recovery kit")
	}

	wg, ok := b.File(masterbackup.WGKeyPath)
	if !ok {
		return fmt.Errorf("bundle lacks the pinned master WireGuard key")
	}

	pub, err := transport.WGPublicKey(strings.TrimSpace(string(wg.Data)))
	if err != nil || pub != old.WGPublicKey {
		return fmt.Errorf("bundle WireGuard key does not match recovery kit")
	}

	pinned, err := transport.ParseHostKeys(rr.kitHostKeys)
	if err != nil {
		return err
	}

	for _, f := range b.Files {
		match, _ := filepath.Match(masterbackup.HostKeyGlob, f.Path)
		if !match {
			continue
		}

		key, err := ssh.ParsePrivateKey(f.Data)
		if err != nil {
			continue
		}

		for _, p := range pinned {
			if bytes.Equal(p.Marshal(), key.PublicKey().Marshal()) {
				return nil
			}
		}
	}

	return fmt.Errorf("bundle host keys do not match recovery kit")
}

func (r *initRun) installKitRestoreWorkspace(setup rpc.AccessSetup, keys enrollmentKeys) error { //nolint:gocognit // Installation and exact resume checks share the same target.
	target := workspaces.Dir(r.name)
	//nolint:nestif // The existing and fresh targets require different checks.
	if _, err := os.Lstat(target); err == nil {
		c, err := workspaces.Load(r.name)
		if err != nil {
			return err
		}

		if !c.Managed || c.Server != setup.Server || c.Operator.Name != setup.User ||
			c.Operator.SSHUser != setup.UnixLogin || c.Operator.WGAddress != setup.Address {
			return fmt.Errorf("workspace %s appeared during restore", r.name)
		}

		if err := r.checkInstalledRestoreOwner(c, setup, keys); err != nil {
			return err
		}

		creds, err := workspaces.LoadCredentials(r.name)
		if err != nil {
			return err
		}

		expected := workspaces.Credentials{Database: r.sec.Database, Backup: r.sec.Backup, Metrics: r.sec.Metrics, MasterPasswordHash: r.sec.MasterPasswordHash}
		if !reflect.DeepEqual(creds, expected) {
			return fmt.Errorf("installed restore owner credentials differ from pending credentials")
		}

		r.restore.ctx = c
	} else if !os.IsNotExist(err) {
		return err
	} else {
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}

		stage, err := os.MkdirTemp(parent, ".restore-")
		if err != nil {
			return err
		}

		defer func() { _ = os.RemoveAll(stage) }()

		creds := workspaces.Credentials{Database: r.sec.Database, Backup: r.sec.Backup, Metrics: r.sec.Metrics, MasterPasswordHash: r.sec.MasterPasswordHash}
		if err := writeEnrollmentFiles(stage, setup, keys, &creds); err != nil {
			return err
		}

		c := *r.restore.ctx
		c.Managed = true

		c.Operator = workspaces.Operator{Name: setup.User, Device: setup.Label, SSHUser: setup.UnixLogin, WGAddress: setup.Address}
		for i := range c.Nodes {
			if c.Nodes[i].Name == setup.Node {
				c.Nodes[i].PublicAddress, c.Nodes[i].WGPublicKey = setup.NodePublicAddress, setup.NodeWGPublicKey
				c.Nodes[i].WGAddress, c.Nodes[i].ListenPort = setup.NodeAddress, setup.WGPort
			}
		}

		c.LockdownConfirmedAt = time.Now().UTC().Format(time.RFC3339)
		if err := workspaces.SaveStagedEnrollment(stage, &c); err != nil {
			return err
		}

		if err := os.Link(filepath.Join(workspaces.PendingDir(r.name), TunnelLockFile), filepath.Join(stage, TunnelLockFile)); err != nil && !os.IsNotExist(err) {
			return err
		}

		if err := os.Rename(stage, target); err != nil {
			return err
		}

		r.restore.ctx = &c
	}

	current, err := workspaces.Current()
	if err != nil {
		return err
	}

	if current == "" || !workspaces.Exists(current) {
		return workspaces.SetCurrent(r.name)
	}

	return nil
}
