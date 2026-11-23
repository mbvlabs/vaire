package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/localenv"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// vaire server init --restore (Phase 5, D7): rebuild this workspace's server on
// a new VPS from its master bundle. The bundle is decrypted here, with the
// recovery identity, and never written in clear; the new server takes over
// the old one's WireGuard key, SSH host keys and certificate, so the workspace
// keeps working; every database comes back awaiting recovery.

// Restore phases, beside init's own in pending/<name>/progress.json.
const (
	phaseBundleFetched   = "bundle_fetched"
	phaseOldChecked      = "old_master_checked"
	phaseFenced          = "fenced"
	phasePlaced          = "bundle_placed"
	phaseFirstBundle     = "first_bundle"
	phaseWorkspaceUpdate = "workspace_updated"
)

// oldMasterProbe is how long the old-master check waits for WireGuard.
const oldMasterProbe = 6 * time.Second

// restoreRun is a rebuild's state beside the initRun it drives.
type restoreRun struct {
	ctx      *workspaces.Workspace
	identity string
	fetched  fetchedBundle
	bundle   masterbackup.Bundle
	state    *workspaces.RestoreProgress
}

func (rr *restoreRun) addressChanged() bool {
	return rr.state.OldAddress != rr.state.NewAddress
}

// restoreResult is the rebuild's result.
type restoreResult struct {
	SecretsDocument  string                `json:"secrets_document,omitempty"`
	Passphrase       string                `json:"passphrase,omitempty"`
	Server           string                `json:"server"`
	Bundle           string                `json:"bundle"`
	From             string                `json:"from"`
	PublicAddress    string                `json:"public_address"`
	OldAddress       string                `json:"old_address"`
	Epoch            int64                 `json:"epoch,omitempty"`
	AwaitingRecovery []string              `json:"awaiting_recovery"`
	Pruned           []backup.PrunedRecord `json:"pruned,omitempty"`
	Timings          map[string]float64    `json:"timings_s"`
	Next             []string              `json:"next"`
}

func runInitRestore(cmd *cobra.Command) error {
	r, rr, kit, err := newRestoreRun(cmd)
	if err != nil {
		return err
	}

	ctx := commandContext(cmd)
	timer := newPhaseTimer(rr.state)

	// 1. Fetch and open the bundle, before anything else happens.
	if err := timer.run(phaseBundleFetched, func() error { return r.restoreFetch(ctx, kit) }); err != nil {
		return err
	}

	if err := r.restoreConfig(); err != nil {
		return err
	}

	if dryRun(cmd) {
		return r.restoreDryRun()
	}

	if err := requireStructuredHandoff(cmd, "secrets-out", "server init --restore", "confirm-secrets-stored"); err != nil {
		return err
	}

	if r.prog.Done(phaseWorkspaceUpdate) {
		return r.restoreFinish(ctx)
	}

	if err := r.restoreConfirm(kit); err != nil {
		return err
	}

	if err := r.savePending(); err != nil {
		return err
	}

	if err := r.restoreSaveBundle(); err != nil {
		return err
	}
	// 2. The old master must not answer.
	if err := r.restoreOldMasterPhase(ctx); err != nil {
		return err
	}
	// 3. Fence it in the bucket.
	if err := r.restoreFencePhase(ctx, timer, kit); err != nil {
		return err
	}
	// 4. The operator's secrets come from the workspace.
	if err := r.restoreSecrets(); err != nil {
		return err
	}

	return r.restoreRemote(ctx, timer)
}

// newRestoreRun resolves the workspace and its recovery identity, and loads a
// pending rebuild.
func newRestoreRun(cmd *cobra.Command) (*initRun, *restoreRun, *workspaces.BackupCreds, error) {
	name := stringFlag(cmd, "name")
	if name == "" {
		name = stringFlag(cmd, "workspace")
	}

	c, err := workspaces.Resolve(name)
	if err != nil {
		return nil, nil, nil, withHint(err, "server init --restore rebuilds the server of an existing workspace. With only the secrets document, run vaire login first.")
	}

	r := &initRun{cmd: cmd, p: newProgress(cmd), name: c.Name}
	rr := &restoreRun{ctx: c}
	r.restore = rr

	kit, identity, err := recoveryIdentity(c.Name)
	if err != nil {
		return nil, nil, nil, err
	}

	rr.identity = identity

	if err := r.restoreLoadPending(); err != nil {
		return nil, nil, nil, err
	}

	return r, rr, kit, nil
}

// restoreSaveBundle keeps the bundle encrypted in the pending state for
// --resume.
func (r *initRun) restoreSaveBundle() error {
	if r.prog.Done(phaseBundleFetched) {
		return nil
	}

	if err := workspaces.SavePendingBundle(r.name, r.restore.fetched.Cipher); err != nil {
		return err
	}

	return r.mark(phaseBundleFetched)
}

// restoreOldMasterPhase checks once that the old master no longer answers.
func (r *initRun) restoreOldMasterPhase(ctx context.Context) error {
	if r.prog.Done(phaseOldChecked) {
		return nil
	}

	if err := r.checkOldMaster(ctx); err != nil {
		return err
	}

	return r.mark(phaseOldChecked)
}

// restoreFencePhase fences the old master in the bucket once.
func (r *initRun) restoreFencePhase(ctx context.Context, timer *phaseTimer, kit *workspaces.BackupCreds) error {
	if r.prog.Done(phaseFenced) {
		return nil
	}

	if err := timer.run(phaseFenced, func() error { return r.fence(ctx, kit) }); err != nil {
		return err
	}

	return r.mark(phaseFenced)
}

// restoreLoadPending starts or resumes the rebuild's pending state.
func (r *initRun) restoreLoadPending() error {
	name := r.name
	if workspaces.PendingExists(name) {
		return r.restoreResumePending(name)
	}

	if flagTrue(r.cmd, "resume") {
		return output.NewError(output.CodePrecondition, "there is no unfinished rebuild of "+name+" to resume", output.ExitPrecondition,
			"Start one with vaire server init --restore --host root@<new-ip>.").WithDetails(map[string]any{"reason": "nothing_to_resume"})
	}

	host := stringFlag(r.cmd, "host")
	if err := RequireFlag(r.cmd, host != "", "--host", "server init --restore"); err != nil {
		return err
	}

	if host == "" {
		return output.NewError(output.CodeInputRequired, "server init --restore needs the new server", output.ExitInput,
			"Pass --host root@<new-ip>.").WithDetails(map[string]any{"flag": "--host"})
	}

	user, h, port, err := parseHostFlag(host)
	if err != nil {
		return output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
	}

	if user == "" {
		user = "root"
	}

	if port == 0 {
		port = 22
	}

	r.boot = workspaces.Bootstrap{Host: h, User: user, Port: port, Identity: stringFlag(r.cmd, "identity"), HostKeyFingerprint: stringFlag(r.cmd, "host-key-fingerprint")}
	r.prog = &workspaces.Progress{Phases: map[string]string{}}
	old, _ := r.restore.ctx.Master()
	r.restore.state = &workspaces.RestoreProgress{OldAddress: old.PublicAddress, Timings: map[string]float64{}}
	r.prog.Restore = r.restore.state

	return nil
}

// restoreResumePending loads the pending state of an unfinished rebuild.
func (r *initRun) restoreResumePending(name string) error {
	if !flagTrue(r.cmd, "resume") {
		return output.NewError(output.CodePrecondition, "an unfinished init or rebuild of "+name+" exists", output.ExitPrecondition,
			"Continue it with vaire server init --restore --resume --name "+name+", or drop it with vaire server init --discard --name "+name+".").
			WithDetails(map[string]any{"reason": "init_pending", "server": name})
	}

	prog, err := workspaces.LoadProgress(name)
	if err != nil {
		return err
	}

	if prog.Restore == nil {
		return output.NewError(output.CodePrecondition, "the unfinished init of "+name+" is not a rebuild", output.ExitPrecondition,
			"Resume it without --restore, or drop it with vaire server init --discard --name "+name+".")
	}

	raw, boot, err := workspaces.LoadAnswers(name)
	if err != nil {
		return err
	}

	cfg, err := config.ParseAnswers(raw)
	if err != nil {
		return output.NewError(output.CodeConfig, "the pending answers do not parse: "+err.Error(), output.ExitConfig, "")
	}

	r.cfg, r.boot, r.prog = cfg, boot, prog

	r.restore.state = prog.Restore
	if prog.Done(phaseSecrets) {
		sec, err := workspaces.LoadSecrets(name)
		if err != nil {
			return err
		}

		r.sec = sec
		r.masterHash = sec.MasterPasswordHash
		registerPending(sec)
	}

	if v := stringFlag(r.cmd, "identity"); v != "" {
		r.boot.Identity = v
	}

	return nil
}

// restoreFetch fetches and opens the bundle: from the pending copy on
// --resume, from --bundle-file, or from the bucket.
func (r *initRun) restoreFetch(ctx context.Context, kit *workspaces.BackupCreds) error {
	rr := r.restore

	var (
		f   fetchedBundle
		err error
	)

	switch {
	case r.prog.Done(phaseBundleFetched):
		raw, lerr := workspaces.LoadPendingBundle(r.name)
		if lerr != nil {
			return lerr
		}

		f = fetchedBundle{Name: rr.state.Bundle, Cipher: raw, From: rr.state.From}
	case stringFlag(r.cmd, "bundle-file") != "":
		f, err = readBundleFile(stringFlag(r.cmd, "bundle-file"), r.cmd.InOrStdin())
	default:
		if kit == nil || !kit.HasBucket() {
			return output.NewError(output.CodeInputRequired, "this workspace has no bucket to fetch the master bundle from", output.ExitInput,
				"Rebuild from an exported bundle: --bundle-file <file> (vaire server backup export made it).").WithDetails(map[string]any{"flag": "--bundle-file"})
		}

		stop := r.p.wait("fetching the master bundle from " + kit.Bucket)
		f, err = fetchBundleFromBucket(ctx, kit, stringFlag(r.cmd, "bundle"))

		stop()
	}

	if err != nil {
		return err
	}

	b, err := openFetched(f, rr.identity, rr.ctx.Server)
	if err != nil {
		return err
	}

	if _, ok := b.File(masterbackup.ClusterPath); !ok {
		return output.NewError(output.CodeBundleCorrupt, "the bundle has no cluster.toml", output.ExitSecrets, "")
	}

	rr.fetched, rr.bundle = f, b
	if f.Name == "" {
		// A file whose name isn't the bundle's: its manifest names it.
		f.Name = b.Manifest.Name()
	}

	rr.state.Bundle, rr.state.From = f.Name, f.From

	return nil
}

// restoreConfig is the bundle's cluster.toml with the new public address.
func (r *initRun) restoreConfig() error {
	rr := r.restore
	f, _ := rr.bundle.File(masterbackup.ClusterPath)

	cfg, err := config.ParseCluster(f.Data)
	if err != nil {
		return output.NewError(output.CodeBundleCorrupt, "the bundle's cluster.toml does not parse: "+err.Error(), output.ExitSecrets, "")
	}

	if cfg.Name() != rr.ctx.Server {
		return output.NewError(output.CodePrecondition, "the bundle is server "+cfg.Name()+"'s, not "+rr.ctx.Server+"'s", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "bundle_server_mismatch"})
	}

	// Approvals belong to the old node, not the backup. Retain the private
	// WireGuard topology and require new approval for public endpoints.
	cfg.Network.ClientAllowlist = nil
	if cfg.Network.RoleAllowlist != nil {
		cfg.Network.RoleAllowlist = &config.RoleAllowlist{}
	}

	cfg.Metrics.Enabled = false

	r.cfg = cfg
	if rr.state.NewAddress == "" {
		addr, err := r.publicAddress()
		if err != nil {
			return err
		}

		rr.state.NewAddress = addr
	}

	for i := range r.cfg.Nodes {
		if r.cfg.Nodes[i].Role == config.RoleMaster {
			r.cfg.Nodes[i].PublicAddress = rr.state.NewAddress
		}
	}

	return r.cfg.Validate()
}

func (r *initRun) restoreDryRun() error {
	rr := r.restore
	m := rr.bundle.Manifest

	dbs := make([]string, 0, len(r.cfg.Clusters))
	for _, c := range r.cfg.Clusters {
		dbs = append(dbs, c.Name)
	}

	data := map[string]any{"server": r.name, "bundle": rr.state.Bundle, "from": rr.state.From, "created_at": m.CreatedAt,
		"epoch": m.Epoch, "databases": dbs, "host": r.boot.User + "@" + r.boot.Host, "public_address": rr.state.NewAddress, "dry_run": true}

	plan := "fence the old master, place the bundle, run every server step; then " + strings.Join(dbs, ", ") + " await vaire db recover."
	if !r.cfg.BackupConfigured() {
		plan = "place the bundle, run every server step; with no bucket the old master isn't fenced, and the data of " + strings.Join(dbs, ", ") + " is lost with it."
	}

	msg := fmt.Sprintf("Would rebuild server %s on %s from master bundle %s (created %s, epoch %d): %s",
		r.name, rr.state.NewAddress, rr.state.Bundle, m.CreatedAt, m.Epoch, plan)

	return output.Success(r.cmd, output.Result{Data: data, Message: msg})
}

// restoreConfirm asks for <server>/replace.
func (r *initRun) restoreConfirm(kit *workspaces.BackupCreds) error {
	cmd := r.cmd

	token := r.name + "/replace"
	if stringFlag(cmd, "confirm") != "" || MustNotPrompt(cmd) {
		return Confirm(cmd, token)
	}

	rr := r.restore

	takeover := "This takes over the old server's identity and fences it: power it off or destroy it first."
	if !canFence(r.cfg, kit) {
		takeover = "This takes over the old server's identity. With no bucket it can't be fenced: power it off or destroy it first."
	}

	r.p.panel("Rebuild "+r.name, []string{
		"bundle   " + rr.state.Bundle + " (" + rr.bundle.Manifest.CreatedAt + ")",
		"new VPS  " + r.boot.User + "@" + rr.state.NewAddress,
		"",
		takeover,
	})

	return typeWord(cmd, "Type '"+token+"' to rebuild", "", token)
}

// checkOldMaster refuses while the old master still answers over WireGuard.
func (r *initRun) checkOldMaster(ctx context.Context) error {
	rr := r.restore
	if flagTrue(r.cmd, "old-master-unreachable") {
		return nil
	}

	old, ok := rr.ctx.Master()
	if !ok || !rr.addressChanged() || rr.state.OldAddress == "" {
		return oldMasterCheckUnavailable()
	}

	wgKey, err := workspaces.ReadFile(rr.ctx.Name, workspaces.WGKeyFile)
	if err != nil {
		return oldMasterCheckUnavailable()
	}

	if _, err := transport.WGPublicKey(strings.TrimSpace(string(wgKey))); err != nil {
		return oldMasterCheckUnavailable()
	}

	stopTunnelDaemonQuietly(ctx, rr.ctx.Name)

	probeCtx, cancel := context.WithTimeout(ctx, oldMasterProbe)
	defer cancel()

	stop := r.p.wait("checking that the old master " + old.PublicAddress + " no longer answers")
	defer stop()

	tun, err := upTunnel(probeCtx, rr.ctx.Name, strings.TrimSpace(string(wgKey)), rr.ctx.Operator.WGAddress, old.WGPublicKey, old.PublicAddress, old.ListenPort, old.WGAddress, nil)
	if err != nil {
		return oldMasterCheckUnavailable()
	}
	defer tun.Close()

	conn, err := tun.DialContext(probeCtx, "tcp", net.JoinHostPort(old.WGAddress, "22"))
	if err != nil {
		return nil //nolint:nilerr // nothing answers on its WireGuard address.
	}

	_ = conn.Close()

	return output.NewError(output.CodePrecondition, "the old master "+old.PublicAddress+" still answers over WireGuard", output.ExitPrecondition,
		"Power it off or destroy it first, then run the rebuild again. If it only looks alive, pass --old-master-unreachable.").
		WithDetails(map[string]any{"reason": "old_master_alive", "address": old.PublicAddress})
}

func oldMasterCheckUnavailable() error {
	return output.NewError(output.CodePrecondition, "cannot check whether the old master is reachable", output.ExitPrecondition,
		"Power off or destroy the old master, then pass --old-master-unreachable.").
		WithDetails(map[string]any{"reason": "old_master_check_unavailable"})
}

// canFence reports whether a rebuild can fence the old master: the epoch
// lives in the bucket.
func canFence(cfg config.Cluster, kit *workspaces.BackupCreds) bool {
	return cfg.BackupConfigured() && kit != nil && kit.HasBucket()
}

// fence writes the next epoch with a new instance id (D5). Without a bucket
// there is nothing to fence.
func (r *initRun) fence(ctx context.Context, kit *workspaces.BackupCreds) error {
	rr := r.restore
	if rr.state.InstanceID == "" {
		id, err := masterbackup.NewInstanceID()
		if err != nil {
			return err
		}

		rr.state.InstanceID = id
	}

	if !canFence(r.cfg, kit) {
		r.warnings = append(r.warnings, output.Warning{Code: "no_fence",
			Message: "no bucket, so the old master can't be fenced: make sure it is powered off or destroyed"})

		return nil
	}

	t := kitTarget(kit)

	cur, err := backup.ReadEpoch(ctx, t)
	if err != nil {
		return err
	}

	next := int64(1)

	if cur != nil {
		if cur.Server != "" && cur.Server != r.name {
			return output.NewError(output.CodePrecondition, "the bucket's master epoch belongs to server "+cur.Server+", not "+r.name, output.ExitPrecondition,
				"Check the workspace's bucket and prefix.").WithDetails(map[string]any{"reason": "bundle_server_mismatch", "server": cur.Server})
		}

		if cur.InstanceID == rr.state.InstanceID {
			// A resumed rebuild that already wrote it: keep its time, which
			// db recover's stale-WAL check compares against.
			rr.state.Epoch, rr.state.EpochWrittenAt = cur.Epoch, cur.WrittenAt
			return nil
		}

		next = cur.Epoch + 1
	}

	e := masterbackup.NewEpoch(r.name, next, rr.state.InstanceID, rr.ctx.Operator.Name, masterbackup.ReasonRestore)
	if err := backup.WriteEpoch(ctx, t, e); err != nil {
		return err
	}

	back, err := backup.ReadEpoch(ctx, t)
	if err != nil {
		return err
	}

	if back == nil || back.InstanceID != rr.state.InstanceID {
		return output.NewError(output.CodePrecondition, "the bucket's epoch changed while it was written: another rebuild is running", output.ExitPrecondition,
			"Stop the other rebuild; one operator at a time.").WithDetails(map[string]any{"reason": "epoch_conflict"})
	}

	rr.state.Epoch, rr.state.EpochWrittenAt = e.Epoch, e.WrittenAt
	r.p.note(fmt.Sprintf("fenced the old master: epoch %d now names this rebuild", e.Epoch))

	return nil
}

// restoreSecrets creates a temporary transport identity and persists it before
// contacting the new host. The workspace device keys are never sent to it.
func (r *initRun) restoreSecrets() error {
	rr := r.restore

	subnet, err := netip.ParsePrefix(r.cfg.Transport.Subnet)
	if err != nil {
		return err
	}

	var reserved []netip.Addr

	for _, n := range r.cfg.Nodes {
		addr, err := netip.ParseAddr(n.WGAddress)
		if err != nil {
			return err
		}

		reserved = append(reserved, addr)
	}

	addr, err := config.NextAccessAddress(subnet, reserved)
	if err != nil {
		return err
	}
	// operator() uses this address for the bootstrap peer. This is separate
	// from the original address used by checkOldMaster above.
	rr.ctx.Operator.WGAddress = addr.String()

	if !r.prog.Done(phaseSecrets) {
		if err := r.saveRestoreSecrets(); err != nil {
			return err
		}
	}

	if err := r.ensureDatabaseSecrets(); err != nil {
		return err
	}

	addRecoveryKit(r.dbSecrets, r.sec.Backup)

	return nil
}

func (r *initRun) saveRestoreSecrets() error {
	creds, err := workspaces.LoadCredentials(r.restore.ctx.Name)
	if err != nil {
		return err
	}

	wg, _, err := transport.GenerateWGKey()
	if err != nil {
		return err
	}

	key, _, err := transport.GenerateSSHKey("vaire-restore-bootstrap")
	if err != nil {
		return err
	}

	sudo := creds.SudoPassword
	if sudo == "" {
		sudo, err = secrets.Base62(24)
		if err != nil {
			return err
		}
	}

	r.sec = workspaces.PendingSecrets{OperatorName: r.restore.ctx.Operator.Name, WGPrivateKey: wg, SSHPrivateKey: string(key),
		SudoPassword: sudo, Database: creds.Database, Backup: creds.Backup, Metrics: creds.Metrics, MasterPasswordHash: creds.MasterPasswordHash}
	registerPending(r.sec)

	if err := workspaces.SaveSecrets(r.name, r.sec); err != nil {
		return err
	}

	return r.mark(phaseSecrets)
}

// place is called by firstContact once the binary is installed: it sends
// the decrypted bundle and returns the host keys the server now presents.
func (rr *restoreRun) place(ctx context.Context, r *initRun, n *remote.Node) ([]ssh.PublicKey, error) {
	raw, err := r.cfg.Encode()
	if err != nil {
		return nil, err
	}

	tarball, _, err := masterbackup.Build(masterbackup.Meta{
		Server: rr.bundle.Manifest.Server, Node: rr.bundle.Manifest.Node, Epoch: rr.bundle.Manifest.Epoch,
		CreatedAt: parseRFC3339(rr.bundle.Manifest.CreatedAt), VaireVersion: rr.bundle.Manifest.VaireVersion,
	}, rr.bundle.Files)
	if err != nil {
		return nil, err
	}

	encoded := base64.StdEncoding.EncodeToString(tarball)
	secrets.Add(encoded)

	var epoch *masterbackup.Epoch

	if rr.state.Epoch > 0 {
		e := masterbackup.NewEpoch(r.name, rr.state.Epoch, rr.state.InstanceID, rr.ctx.Operator.Name, masterbackup.ReasonRestore)
		e.WrittenAt = rr.state.EpochWrittenAt
		epoch = &e
	}

	r.p.phase("init.restore", "started", "place the master bundle on the new server")

	reply, err := r.p.call(ctx, n, "cluster.bootstrap", map[string]any{
		"phase":        node.PhaseRestore,
		"cluster_toml": string(raw),
		"node_name":    r.master().Name,
		"instance_id":  rr.state.InstanceID,
		"epoch":        epoch,
	}, remote.CallOptions{Secrets: map[string]string{steps.SecretMasterBundle: encoded}, Confirm: r.name + "/replace", OnEvent: r.p.event})
	if err != nil {
		return nil, err
	}

	var placed node.RestorePlaced
	if err := reply.Decode(&placed); err != nil {
		return nil, err
	}

	r.warnings = append(r.warnings, reply.Warnings...)

	if len(placed.Pruned) > 0 {
		var names []string
		for _, p := range placed.Pruned {
			names = append(names, p.Database+": "+p.Name)
		}

		r.warnings = append(r.warnings, output.Warning{Code: "restore_leftovers_lost",
			Message: "these were on the lost server's disk and are gone: " + strings.Join(names, ", ")})
	}

	keys, err := transport.ParseHostKeys(placed.SSHHostKeys)
	if err != nil {
		return nil, err
	}

	r.p.phase("init.restore", "completed", fmt.Sprintf("%d files placed; %d databases await recovery", len(placed.Placed), len(placed.Awaiting)))

	if err := r.mark(phasePlaced); err != nil {
		return nil, err
	}

	return keys, nil
}

// withHint adds hint to err's.
func withHint(err error, hint string) error {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return output.WrapError(output.CodeError, err, output.ExitGeneric, hint)
	}

	out := *cliErr
	if out.Hint != "" {
		hint = out.Hint + " " + hint
	}

	out.Hint = hint

	return &out
}

func parseRFC3339(ts string) time.Time {
	t, _ := time.Parse(time.RFC3339, ts)
	return t
}

// restoreRemote runs first contact, placement, the host phase, the switch to
// WireGuard and the lockdown, DNS, the database phase, the first bundle and
// the workspace update.
func (r *initRun) restoreRemote(ctx context.Context, timer *phaseTimer) error {
	cmd := r.cmd
	rr := r.restore
	timeout := timeoutFlag(cmd)
	n := r.master()

	publicSSH := net.JoinHostPort(n.PublicAddress, strconv.Itoa(r.boot.Port))
	if err := r.savePending(); err != nil {
		return err
	}

	if err := r.restoreHostPhase(ctx, timer, publicSSH, timeout); err != nil {
		return err
	}

	// The pending keys are a temporary bootstrap transport, not the owner.
	stopTunnelDaemonQuietly(ctx, r.name)

	tun, err := upTunnel(ctx, r.name, r.sec.WGPrivateKey, rr.ctx.Operator.WGAddress, r.prog.WGPublicKey, n.PublicAddress, r.cfg.Transport.ListenPort, n.WGAddress, r.p)
	if err != nil {
		return err
	}
	defer tun.Close()

	rt, err := r.restoreConnect(ctx, tun, n)
	if err != nil {
		return err
	}
	defer rt.close()

	if err := r.restoreLockdown(ctx, timer, rt, tun, n); err != nil {
		return err
	}

	if err := r.restoreDNSPhase(ctx, timer); err != nil {
		return err
	}

	if err := r.restoreDatabasePhase(ctx, timer, rt.node); err != nil {
		return err
	}

	setup, keys, err := r.bootstrapOwner(ctx, rt.node)
	if err != nil {
		return err
	}

	if err := r.issueRecovery(ctx, rt.node); err != nil {
		return err
	}

	handoff, err := r.writeDocument()
	if err != nil {
		return err
	}

	if err := r.saveRestoreOwner(setup); err != nil {
		return err
	}

	if err := r.restoreSecretsPhase(ctx, rt.node); err != nil {
		return err
	}

	if err := r.restoreWorkspacePhase(setup, keys); err != nil {
		return err
	}

	if err := r.completeSetupAccess(ctx, setup, keys); err != nil {
		return err
	}

	return r.restoreResult(cmd, rr, handoff)
}

// restoreHostPhase pins the host key and installs the binary over public SSH.
func (r *initRun) restoreHostPhase(ctx context.Context, timer *phaseTimer, publicSSH string, timeout time.Duration) error {
	if r.prog.Done(phaseHost) {
		return nil
	}

	if err := r.p.during("checking that "+publicSSH+" answers", func() error { return localenv.CheckReachable(ctx, publicSSH, timeout) }); err != nil {
		return err
	}

	known, err := transport.LoadKnownHosts(workspaces.PendingKnownHosts(r.name))
	if err != nil {
		return err
	}

	return timer.run(phaseHost, func() error { return r.firstContact(ctx, known, publicSSH, timeout) })
}

// restoreTransport is the SSH connection to the rebuilt server and what the
// lockdown needs from it.
type restoreTransport struct {
	node   *remote.Node
	client *transport.SSHClient
	target transport.SSHTarget
	opts   transport.SSHOptions
}

// close ends the connection.
func (t *restoreTransport) close() {
	t.client.Close()
}

// restoreConnect opens SSH to the rebuilt server over WireGuard, trusting
// only host keys pinned for the old server.
func (r *initRun) restoreConnect(ctx context.Context, tun *transport.Tunnel, n config.NodeSpec) (*restoreTransport, error) {
	known, err := transport.LoadKnownHosts(workspaces.PendingKnownHosts(r.name))
	if err != nil {
		return nil, err
	}

	opAuth, err := transport.KeyAuth([]byte(r.sec.SSHPrivateKey))
	if err != nil {
		return nil, err
	}

	adminTarget := transport.SSHTarget{User: r.cfg.Server.AdminUser, Addr: net.JoinHostPort(n.WGAddress, "22")}
	sshOpts := transport.SSHOptions{Auth: []ssh.AuthMethod{opAuth}, HostKeys: known, Timeout: 15 * time.Second}

	r.p.phase("init.transport", "started", "connect over WireGuard to "+n.WGAddress)

	wgClient, err := dialRetryReport(ctx, tun, adminTarget, sshOpts, confirmWithin, r.attemptReporter("init.transport", "SSH over WireGuard to "+adminTarget.Addr))
	if err != nil {
		return nil, tunnelHint(err, n.PublicAddress, r.cfg.Transport.ListenPort)
	}

	wgNode := &remote.Node{SSH: wgClient, Operator: r.sec.OperatorName}
	if err := wgNode.Handshake(ctx, remote.HandshakeOptions{AutoUpgrade: flagTrue(r.cmd, "auto-upgrade-nodes"), Progress: r.p.note}); err != nil {
		wgClient.Close()
		return nil, err
	}

	r.p.phase("init.transport", "completed", "the server answers with the old host key")

	if err := r.mark(phaseTransport); err != nil {
		wgClient.Close()
		return nil, err
	}

	return &restoreTransport{node: wgNode, client: wgClient, target: adminTarget, opts: sshOpts}, nil
}

// restoreLockdown closes public SSH on the rebuilt server once.
func (r *initRun) restoreLockdown(ctx context.Context, timer *phaseTimer, rt *restoreTransport, tun *transport.Tunnel, n config.NodeSpec) error {
	if r.prog.Done(phaseLockdown) {
		return nil
	}

	if err := timer.run(phaseLockdown, func() error {
		_, err := r.lockdown(ctx, rt.node, tun, rt.target, rt.opts, net.JoinHostPort(n.PublicAddress, "22"))
		return err
	}); err != nil {
		return err
	}

	return r.mark(phaseLockdown)
}

// restoreDNSPhase points the name at the rebuilt server when ACME needs it.
func (r *initRun) restoreDNSPhase(ctx context.Context, timer *phaseTimer) error {
	if r.cfg.TLS.Mode != "acme" || !r.restore.addressChanged() || r.prog.Done(phaseDNS) {
		return nil
	}

	if err := timer.run(phaseDNS, func() error { return r.dnsStep(ctx) }); err != nil {
		return err
	}

	return r.mark(phaseDNS)
}

// restoreDatabasePhase runs every server step; the databases await recovery.
func (r *initRun) restoreDatabasePhase(ctx context.Context, timer *phaseTimer, wgNode *remote.Node) error {
	if r.prog.Done(phaseDatabase) {
		return nil
	}

	r.p.phase("init.database", "started", "run every server step; the databases await recovery")

	if err := timer.run(phaseDatabase, func() error {
		_, err := r.callDatabasePhase(ctx, wgNode)
		return err
	}); err != nil {
		return err
	}

	r.p.phase("init.database", "completed", "")

	return r.mark(phaseDatabase)
}

// restoreFirstBundle bundles the rebuilt master's first backup once.
func (r *initRun) restoreFirstBundle(ctx context.Context, timer *phaseTimer, wgNode *remote.Node) error {
	if r.prog.Done(phaseFirstBundle) {
		return nil
	}

	if err := timer.run(phaseFirstBundle, func() error {
		_, err := r.p.call(ctx, wgNode, "master-backup.now", map[string]any{"force": true}, remote.CallOptions{OnEvent: r.p.event})
		return err
	}); err != nil {
		r.warnings = append(r.warnings, output.Warning{Code: "master_backup_failed", Message: "the rebuilt master's first bundle failed: " + err.Error() + "; run vaire server backup now"})
	}

	return r.mark(phaseFirstBundle)
}

// restoreWorkspacePhase points the workspace at the rebuilt server once.
func (r *initRun) restoreWorkspacePhase(setup rpc.AccessSetup, keys enrollmentKeys) error {
	if r.prog.Done(phaseWorkspaceUpdate) {
		return nil
	}

	if err := r.replaceRestoreWorkspace(setup, keys); err != nil {
		return err
	}

	return r.mark(phaseWorkspaceUpdate)
}

const restoreOwnerFile = "restore-owner.json"

func (r *initRun) saveRestoreOwner(setup rpc.AccessSetup) error {
	raw, err := json.Marshal(setup)
	if err != nil {
		return err
	}

	return transport.WriteSecretFile(filepath.Join(workspaces.PendingDir(r.name), restoreOwnerFile), raw)
}

// replaceRestoreWorkspace exchanges two complete directories atomically. A
// crash leaves either the old workspace or the verified owner workspace in
// place; the pending directory holds the other until the next resume.
func (r *initRun) replaceRestoreWorkspace(setup rpc.AccessSetup, keys enrollmentKeys) error {
	target := workspaces.Dir(r.name)
	stage := filepath.Join(workspaces.PendingDir(r.name), "restore-workspace")

	c, err := workspaces.Load(r.name)
	if err != nil {
		return err
	}

	if c.Server != setup.Server || c.Operator.Name != setup.User {
		return fmt.Errorf("workspace %s no longer matches the restore owner", r.name)
	}

	if c.Managed && c.Operator.SSHUser == setup.UnixLogin && c.Operator.WGAddress == setup.Address {
		if err := r.checkInstalledRestoreOwner(c, setup, keys); err != nil {
			return err
		}

		r.restore.ctx = c

		return os.RemoveAll(stage)
	}

	if err := os.RemoveAll(stage); err != nil {
		return err
	}

	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}

	creds := workspaces.Credentials{Database: r.sec.Database, Backup: r.sec.Backup,
		Metrics: r.sec.Metrics, MasterPasswordHash: r.sec.MasterPasswordHash}
	if err := writeEnrollmentFiles(stage, setup, keys, &creds); err != nil {
		return err
	}

	c.Managed = true
	c.Operator = workspaces.Operator{Name: setup.User, SSHUser: setup.UnixLogin, WGAddress: setup.Address}

	for i := range c.Nodes {
		if c.Nodes[i].Name == setup.Node {
			c.Nodes[i].PublicAddress, c.Nodes[i].WGPublicKey = setup.NodePublicAddress, setup.NodeWGPublicKey
			c.Nodes[i].WGAddress, c.Nodes[i].ListenPort = setup.NodeAddress, setup.WGPort
		}
	}

	c.LockdownConfirmedAt = time.Now().UTC().Format(time.RFC3339)
	if err := workspaces.SaveStagedEnrollment(stage, c); err != nil {
		return err
	}
	// Keep the same lock inode across the exchange while this process still
	// owns the bootstrap tunnel; otherwise another command could acquire a
	// different lock file and race it.
	if err := os.Link(filepath.Join(target, TunnelLockFile), filepath.Join(stage, TunnelLockFile)); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := swapWorkspaceDirs(stage, target); err != nil {
		return fmt.Errorf("exchange restored workspace: %w", err)
	}

	r.restore.ctx = c

	return os.RemoveAll(stage)
}

func (r *initRun) checkInstalledRestoreOwner(c *workspaces.Workspace, setup rpc.AccessSetup, keys enrollmentKeys) error {
	n, ok := c.Master()
	if !ok || n.Name != setup.Node || n.PublicAddress != setup.NodePublicAddress ||
		n.WGAddress != setup.NodeAddress || n.WGPublicKey != setup.NodeWGPublicKey || n.ListenPort != setup.WGPort {
		return fmt.Errorf("installed restore owner node does not match pending setup")
	}

	sshKey, err := workspaces.ReadFile(r.name, workspaces.SSHKeyFile)
	if err != nil {
		return err
	}

	wgKey, err := workspaces.ReadFile(r.name, workspaces.WGKeyFile)
	if err != nil {
		return err
	}

	if string(sshKey) != keys.SSH || strings.TrimSpace(string(wgKey)) != keys.WG {
		return fmt.Errorf("installed restore owner keys do not match pending keys")
	}

	return nil
}

// A crash after retirement must not require the retired transport again.
func (r *initRun) restoreFinish(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(workspaces.PendingDir(r.name), restoreOwnerFile))
	if err != nil {
		return err
	}

	var setup rpc.AccessSetup
	if err := json.Unmarshal(raw, &setup); err != nil {
		return err
	}

	keys := enrollmentKeys{SSH: r.sec.OwnerSSHPrivateKey, WG: r.sec.OwnerWGPrivateKey}
	if err := r.validateOwnerSetup(setup); err != nil {
		return err
	}

	if err := r.replaceRestoreWorkspace(setup, keys); err != nil {
		return err
	}

	if err := verifyOwnerEnrollment(ctx, setup, keys); err != nil {
		return err
	}

	handoff, err := r.writeDocument()
	if err != nil {
		return err
	}

	if err := r.completeSetupAccess(ctx, setup, keys); err != nil {
		return err
	}

	return r.restoreResult(r.cmd, r.restore, handoff)
}

// restoreSecretsPhase confirms the rebuilt server's recovery kit once.
func (r *initRun) restoreSecretsPhase(ctx context.Context, wgNode *remote.Node) error {
	if r.prog.Done(phaseSecretsDone) {
		return nil
	}

	if _, err := r.p.call(ctx, wgNode, "cluster.confirm-secrets", nil, remote.CallOptions{OnEvent: r.p.event}); err != nil {
		return err
	}

	return r.mark(phaseSecretsDone)
}

// restoreResult is the rebuild's success result.
func (r *initRun) restoreResult(cmd *cobra.Command, rr *restoreRun, handoff initResult) error {
	res := r.restoreResultData()
	res.SecretsDocument, res.Passphrase = handoff.SecretsDocument, handoff.Passphrase

	if err := workspaces.RemovePending(r.name); err != nil {
		return err
	}

	awaiting := "The databases await recovery: " + strings.Join(res.AwaitingRecovery, ", ") + "."
	if !r.cfg.BackupConfigured() {
		awaiting = "With no bucket the databases' data is lost with the old server: drop them, create the sources again, then re-follow the followers."
	}

	msg := []string{
		fmt.Sprintf("Server %s is rebuilt on %s from master bundle %s.", r.name, rr.state.NewAddress, rr.state.Bundle),
		"Your workspace has a verified owner device. " + awaiting,
	}
	if rr.addressChanged() && r.cfg.TLS.Mode == "acme" {
		msg = append(msg, "Clients reconnect once "+r.cfg.MasterHostname()+" resolves to "+rr.state.NewAddress+".")
	}

	return output.Success(cmd, output.Result{Data: res, Message: strings.Join(msg, "\n"), Warnings: r.warnings, Next: res.Next})
}

func (r *initRun) restoreResultData() restoreResult {
	rr := r.restore

	res := restoreResult{Server: r.name, Bundle: rr.state.Bundle, From: rr.state.From, PublicAddress: rr.state.NewAddress,
		OldAddress: rr.state.OldAddress, Epoch: rr.state.Epoch, Timings: rr.state.Timings, AwaitingRecovery: []string{}}
	for _, c := range r.cfg.Clusters {
		res.AwaitingRecovery = append(res.AwaitingRecovery, c.Name)
	}

	switch {
	case len(res.AwaitingRecovery) == 0:
	case r.cfg.BackupConfigured():
		res.Next = append(res.Next, "vaire db recover --all")
	default:
		// Without a bucket there is no stanza to recover from. Followers go
		// first: a source with followers can't be dropped, and a follower comes
		// back with db follow once its source exists again, not with db create
		// as a database of its own.
		var followers, sources []config.ClusterSpec

		for _, c := range r.cfg.Clusters {
			if c.IsFollower() {
				followers = append(followers, c)
			} else {
				sources = append(sources, c)
			}
		}

		for _, c := range followers {
			res.Next = append(res.Next, "vaire db drop "+c.Name+" --confirm "+c.Name)
		}

		for _, c := range sources {
			res.Next = append(res.Next, "vaire db drop "+c.Name+" --confirm "+c.Name)
		}

		for _, c := range sources {
			res.Next = append(res.Next, "vaire db create "+c.Name)
		}

		for _, c := range followers {
			res.Next = append(res.Next, "vaire db follow "+c.Follows+" --name "+c.Name)
		}
	}

	res.Next = append(res.Next, "vaire doctor", "vaire server backup list")

	return res
}

// phaseTimer records how long each phase took, for the RTO.
type phaseTimer struct{ state *workspaces.RestoreProgress }

func newPhaseTimer(st *workspaces.RestoreProgress) *phaseTimer {
	if st != nil && st.Timings == nil {
		st.Timings = map[string]float64{}
	}

	return &phaseTimer{state: st}
}

func (t *phaseTimer) run(phase string, fn func() error) error {
	start := time.Now()

	err := fn()
	if t.state != nil && err == nil {
		t.state.Timings[phase] += time.Since(start).Round(100 * time.Millisecond).Seconds()
	}

	return err
}
