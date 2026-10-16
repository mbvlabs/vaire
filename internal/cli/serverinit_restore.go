package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// eru server init --restore (Phase 5, D7): rebuild this context's server on
// a new VPS from its master bundle. The bundle is decrypted here, with the
// recovery identity, and never written in clear; the new server takes over
// the old one's WireGuard key, SSH host keys and certificate, so the context
// keeps working; every database comes back awaiting recovery.

// Restore phases, beside init's own in pending/<name>/progress.json.
const (
	phaseBundleFetched = "bundle_fetched"
	phaseOldChecked    = "old_master_checked"
	phaseFenced        = "fenced"
	phasePlaced        = "bundle_placed"
	phaseFirstBundle   = "first_bundle"
	phaseContextUpdate = "context_updated"
)

// oldMasterProbe is how long the old-master check waits for WireGuard.
const oldMasterProbe = 6 * time.Second

// restoreRun is a rebuild's state beside the initRun it drives.
type restoreRun struct {
	ctx      *contexts.Context
	identity string
	fetched  fetchedBundle
	bundle   masterbackup.Bundle
	state    *contexts.RestoreProgress
}

func (rr *restoreRun) addressChanged() bool {
	return rr.state.OldAddress != rr.state.NewAddress
}

// restoreResult is the rebuild's result.
type restoreResult struct {
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
	if flagTrue(cmd, "local") {
		return output.NewError(output.CodeUsage, "--restore runs from your machine, not with --local", output.ExitUsage,
			"The bundle is decrypted with the recovery identity, which only your machine holds. Run eru server init --restore --host root@<new-ip> there.")
	}
	name := stringFlag(cmd, "name")
	if name == "" {
		name = stringFlag(cmd, "context")
	}
	c, err := contexts.Resolve(name)
	if err != nil {
		return withHint(err, "server init --restore rebuilds the server of an existing context. With only the secrets document, run eru login first.")
	}
	r := &initRun{cmd: cmd, p: newProgress(cmd), name: c.Name}
	rr := &restoreRun{ctx: c}
	r.restore = rr
	kit, identity, err := recoveryIdentity(c.Name)
	if err != nil {
		return err
	}
	rr.identity = identity
	if err := r.restoreLoadPending(); err != nil {
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
	if err := r.restoreConfirm(kit); err != nil {
		return err
	}
	if err := r.savePending(); err != nil {
		return err
	}
	if !r.prog.Done(phaseBundleFetched) {
		// The bundle stays encrypted in the pending state for --resume.
		if err := contexts.SavePendingBundle(r.name, rr.fetched.Cipher); err != nil {
			return err
		}
		if err := r.mark(phaseBundleFetched); err != nil {
			return err
		}
	}
	// 2. The old master must not answer.
	if !r.prog.Done(phaseOldChecked) {
		if err := r.checkOldMaster(ctx); err != nil {
			return err
		}
		if err := r.mark(phaseOldChecked); err != nil {
			return err
		}
	}
	// 3. Fence it in the bucket.
	if !r.prog.Done(phaseFenced) {
		if err := timer.run(phaseFenced, func() error { return r.fence(ctx, kit) }); err != nil {
			return err
		}
		if err := r.mark(phaseFenced); err != nil {
			return err
		}
	}
	// 4. The operator's secrets come from the context.
	if err := r.restoreSecrets(); err != nil {
		return err
	}
	return r.restoreRemote(ctx, timer)
}

// restoreLoadPending starts or resumes the rebuild's pending state.
func (r *initRun) restoreLoadPending() error {
	name := r.name
	resume := flagTrue(r.cmd, "resume")
	if contexts.PendingExists(name) {
		if !resume {
			return output.NewError(output.CodePrecondition, "an unfinished init or rebuild of "+name+" exists", output.ExitPrecondition,
				"Continue it with eru server init --restore --resume --name "+name+", or drop it with eru server init --discard --name "+name+".").
				WithDetails(map[string]any{"reason": "init_pending", "server": name})
		}
		prog, err := contexts.LoadProgress(name)
		if err != nil {
			return err
		}
		if prog.Restore == nil {
			return output.NewError(output.CodePrecondition, "the unfinished init of "+name+" is not a rebuild", output.ExitPrecondition,
				"Resume it without --restore, or drop it with eru server init --discard --name "+name+".")
		}
		raw, boot, err := contexts.LoadAnswers(name)
		if err != nil {
			return err
		}
		cfg, err := config.ParseCluster(raw)
		if err != nil {
			return output.NewError(output.CodeConfig, "the pending cluster.toml does not parse: "+err.Error(), output.ExitConfig, "")
		}
		r.cfg, r.boot, r.prog = cfg, boot, prog
		r.restore.state = prog.Restore
		if v := stringFlag(r.cmd, "identity"); v != "" {
			r.boot.Identity = v
		}
		return nil
	} else if resume {
		return output.NewError(output.CodePrecondition, "there is no unfinished rebuild of "+name+" to resume", output.ExitPrecondition,
			"Start one with eru server init --restore --host root@<new-ip>.").WithDetails(map[string]any{"reason": "nothing_to_resume"})
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
	r.boot = contexts.Bootstrap{Host: h, User: user, Port: port, Identity: stringFlag(r.cmd, "identity"), HostKeyFingerprint: stringFlag(r.cmd, "host-key-fingerprint")}
	r.prog = &contexts.Progress{Phases: map[string]string{}}
	old, _ := r.restore.ctx.Master()
	r.restore.state = &contexts.RestoreProgress{OldAddress: old.PublicAddress, Timings: map[string]float64{}}
	r.prog.Restore = r.restore.state
	return nil
}

// restoreFetch fetches and opens the bundle: from the pending copy on
// --resume, from --bundle-file, or from the bucket.
func (r *initRun) restoreFetch(ctx context.Context, kit *contexts.BackupCreds) error {
	rr := r.restore
	var f fetchedBundle
	var err error
	switch {
	case r.prog.Done(phaseBundleFetched):
		raw, lerr := contexts.LoadPendingBundle(r.name)
		if lerr != nil {
			return lerr
		}
		f = fetchedBundle{Name: rr.state.Bundle, Cipher: raw, From: rr.state.From}
	case stringFlag(r.cmd, "bundle-file") != "":
		f, err = readBundleFile(stringFlag(r.cmd, "bundle-file"), r.cmd.InOrStdin())
	default:
		if kit == nil || !kit.HasBucket() {
			return output.NewError(output.CodeInputRequired, "this context has no bucket to fetch the master bundle from", output.ExitInput,
				"Rebuild from an exported bundle: --bundle-file <file> (eru server backup export made it).").WithDetails(map[string]any{"flag": "--bundle-file"})
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
	if err := operatorInBundle(b, rr.ctx.Operator.Name); err != nil {
		return err
	}
	rr.fetched, rr.bundle = f, b
	if f.Name == "" {
		// A file whose name isn't the bundle's: its manifest names it.
		f.Name = b.Manifest.Name()
	}
	rr.state.Bundle, rr.state.From = f.Name, f.From
	return nil
}

// operatorInBundle refuses a bundle whose operator registry lacks this
// context's operator: the rebuilt server wouldn't let it in.
func operatorInBundle(b masterbackup.Bundle, operator string) error {
	f, ok := b.File(masterbackup.OperatorsPath)
	if !ok {
		return nil
	}
	var ops config.Operators
	if err := toml.Unmarshal(f.Data, &ops); err != nil {
		return output.NewError(output.CodeBundleCorrupt, "the bundle's operators.toml does not parse: "+err.Error(), output.ExitSecrets, "")
	}
	for _, op := range ops.Operators {
		if op.Name == operator {
			return nil
		}
	}
	return output.NewError(output.CodePrecondition, "the bundle's operators don't include this context's operator "+operator, output.ExitPrecondition,
		"Use the context of an operator the server knew, or an older bundle with --bundle.").WithDetails(map[string]any{"reason": "operator_not_in_bundle"})
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
	var dbs []string
	for _, c := range r.cfg.Clusters {
		dbs = append(dbs, c.Name)
	}
	data := map[string]any{"server": r.name, "bundle": rr.state.Bundle, "from": rr.state.From, "created_at": m.CreatedAt,
		"epoch": m.Epoch, "databases": dbs, "host": r.boot.User + "@" + r.boot.Host, "public_address": rr.state.NewAddress, "dry_run": true}
	plan := "fence the old master, place the bundle, run every server step; then " + strings.Join(dbs, ", ") + " await eru db recover."
	if !r.cfg.BackupConfigured() {
		plan = "place the bundle, run every server step; with no bucket the old master isn't fenced, and the data of " + strings.Join(dbs, ", ") + " is lost with it."
	}
	msg := fmt.Sprintf("Would rebuild server %s on %s from master bundle %s (created %s, epoch %d): %s",
		r.name, rr.state.NewAddress, rr.state.Bundle, m.CreatedAt, m.Epoch, plan)
	return output.Success(r.cmd, output.Result{Data: data, Message: msg})
}

// restoreConfirm asks for <server>/replace.
func (r *initRun) restoreConfirm(kit *contexts.BackupCreds) error {
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
	if flagTrue(r.cmd, "old-master-unreachable") || !rr.addressChanged() || rr.state.OldAddress == "" {
		return nil
	}
	old, ok := rr.ctx.Master()
	if !ok {
		return nil
	}
	wgKey, err := contexts.ReadFile(rr.ctx.Name, contexts.WGKeyFile)
	if err != nil {
		return contextFileError(rr.ctx.Name, contexts.WGKeyFile, err)
	}
	stopTunnelDaemonQuietly(rr.ctx.Name, true)
	probeCtx, cancel := context.WithTimeout(ctx, oldMasterProbe)
	defer cancel()
	stop := r.p.wait("checking that the old master " + old.PublicAddress + " no longer answers")
	defer stop()
	tun, err := upTunnel(probeCtx, rr.ctx.Name, strings.TrimSpace(string(wgKey)), rr.ctx.Operator.WGAddress, old.WGPublicKey, old.PublicAddress, old.ListenPort, old.WGAddress, nil)
	if err != nil {
		return nil //nolint:nilerr // no tunnel: the old master doesn't answer.
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

// canFence reports whether a rebuild can fence the old master: the epoch
// lives in the bucket.
func canFence(cfg config.Cluster, kit *contexts.BackupCreds) bool {
	return cfg.BackupConfigured() && kit != nil && kit.HasBucket()
}

// fence writes the next epoch with a new instance id (D5). Without a bucket
// there is nothing to fence.
func (r *initRun) fence(ctx context.Context, kit *contexts.BackupCreds) error {
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
				"Check the context's bucket and prefix.").WithDetails(map[string]any{"reason": "bundle_server_mismatch", "server": cur.Server})
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

// restoreSecrets fills the pending secrets from the context: the operator's
// keys, the sudo password and the service credentials, which the steps
// re-send (D11).
func (r *initRun) restoreSecrets() error {
	rr := r.restore
	wgKey, err := contexts.ReadFile(rr.ctx.Name, contexts.WGKeyFile)
	if err != nil {
		return contextFileError(rr.ctx.Name, contexts.WGKeyFile, err)
	}
	sshKey, err := contexts.ReadFile(rr.ctx.Name, contexts.SSHKeyFile)
	if err != nil {
		return contextFileError(rr.ctx.Name, contexts.SSHKeyFile, err)
	}
	creds, err := contexts.LoadCredentials(rr.ctx.Name)
	if err != nil {
		return err
	}
	r.sec = contexts.PendingSecrets{
		OperatorName: rr.ctx.Operator.Name, WGPrivateKey: strings.TrimSpace(string(wgKey)), SSHPrivateKey: string(sshKey),
		SudoPassword: creds.SudoPassword, Database: creds.Database, Backup: creds.Backup, Metrics: creds.Metrics,
	}
	registerPending(r.sec)
	r.prog.Mark(phaseSecrets)
	if err := r.ensureDatabaseSecrets(); err != nil {
		return err
	}
	addRecoveryKit(r.dbSecrets, r.sec.Backup)
	if r.sec.Database != nil && creds.Database != nil && len(r.sec.Database.Services) != len(creds.Database.Services) {
		r.warnings = append(r.warnings, output.Warning{Code: "secrets_document_stale", Message: "service credentials were missing from this context and were generated; run eru secrets export"})
	}
	// Generated credentials (a context from before a phase) go back into
	// the context too, not only the pending state.
	creds.Database, creds.Metrics = r.sec.Database, r.sec.Metrics
	return contexts.SaveCredentials(rr.ctx.Name, creds)
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
		CreatedAt: parseRFC3339(rr.bundle.Manifest.CreatedAt), EruVersion: rr.bundle.Manifest.EruVersion,
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
// the context update.
func (r *initRun) restoreRemote(ctx context.Context, timer *phaseTimer) error {
	cmd := r.cmd
	rr := r.restore
	timeout := timeoutFlag(cmd)
	n := r.master()
	publicSSH := net.JoinHostPort(n.PublicAddress, strconv.Itoa(r.boot.Port))
	if err := r.savePending(); err != nil {
		return err
	}
	if !r.prog.Done(phaseHost) {
		if err := r.p.during("checking that "+publicSSH+" answers", func() error { return localenv.CheckReachable(ctx, publicSSH, timeout) }); err != nil {
			return err
		}
		known, err := transport.LoadKnownHosts(contexts.PendingKnownHosts(r.name))
		if err != nil {
			return err
		}
		if err := timer.run(phaseHost, func() error { return r.firstContact(ctx, known, publicSSH, timeout) }); err != nil {
			return err
		}
	}

	// WireGuard with the context's own keys: the server has the old key.
	stopTunnelDaemonQuietly(r.name, true)
	tun, err := upTunnel(ctx, r.name, r.sec.WGPrivateKey, rr.ctx.Operator.WGAddress, r.prog.WGPublicKey, n.PublicAddress, r.cfg.Transport.ListenPort, n.WGAddress, r.p)
	if err != nil {
		return err
	}
	defer tun.Close()
	known, err := transport.LoadKnownHosts(contexts.Path(r.name, contexts.KnownHostsFile))
	if err != nil {
		return err
	}
	opAuth, err := transport.KeyAuth([]byte(r.sec.SSHPrivateKey))
	if err != nil {
		return err
	}
	adminTarget := transport.SSHTarget{User: r.cfg.Server.AdminUser, Addr: net.JoinHostPort(n.WGAddress, "22")}
	sshOpts := transport.SSHOptions{Auth: []ssh.AuthMethod{opAuth}, HostKeys: known, Timeout: 15 * time.Second}
	r.p.phase("init.transport", "started", "connect over WireGuard to "+n.WGAddress)
	wgClient, err := dialRetryReport(ctx, tun, adminTarget, sshOpts, confirmWithin, r.attemptReporter("init.transport", "SSH over WireGuard to "+adminTarget.Addr))
	if err != nil {
		return tunnelHint(err, n.PublicAddress, r.cfg.Transport.ListenPort)
	}
	defer wgClient.Close()
	wgNode := &remote.Node{SSH: wgClient, Operator: r.sec.OperatorName}
	if err := wgNode.Handshake(ctx, remote.HandshakeOptions{AutoUpgrade: flagTrue(cmd, "auto-upgrade-nodes"), Progress: r.p.note}); err != nil {
		return err
	}
	r.p.phase("init.transport", "completed", "the server answers with the old host key")
	if err := r.mark(phaseTransport); err != nil {
		return err
	}
	if !r.prog.Done(phaseLockdown) {
		if err := timer.run(phaseLockdown, func() error {
			_, err := r.lockdown(ctx, wgNode, tun, adminTarget, sshOpts, net.JoinHostPort(n.PublicAddress, "22"))
			return err
		}); err != nil {
			return err
		}
		if err := r.mark(phaseLockdown); err != nil {
			return err
		}
	}
	if r.cfg.TLS.Mode == "acme" && rr.addressChanged() && !r.prog.Done(phaseDNS) {
		if err := timer.run(phaseDNS, func() error { return r.dnsStep(ctx) }); err != nil {
			return err
		}
		if err := r.mark(phaseDNS); err != nil {
			return err
		}
	}
	if !r.prog.Done(phaseDatabase) {
		r.p.phase("init.database", "started", "run every server step; the databases await recovery")
		if err := timer.run(phaseDatabase, func() error {
			_, err := r.callDatabasePhase(ctx, wgNode)
			return err
		}); err != nil {
			return err
		}
		r.p.phase("init.database", "completed", "")
		if err := r.mark(phaseDatabase); err != nil {
			return err
		}
	}
	if !r.prog.Done(phaseFirstBundle) {
		if err := timer.run(phaseFirstBundle, func() error {
			_, err := r.p.call(ctx, wgNode, "master-backup.now", map[string]any{"force": true}, remote.CallOptions{OnEvent: r.p.event})
			return err
		}); err != nil {
			r.warnings = append(r.warnings, output.Warning{Code: "master_backup_failed", Message: "the rebuilt master's first bundle failed: " + err.Error() + "; run eru server backup now"})
		}
		if err := r.mark(phaseFirstBundle); err != nil {
			return err
		}
	}
	if !r.prog.Done(phaseContextUpdate) {
		if err := r.updateContext(); err != nil {
			return err
		}
		if err := r.mark(phaseContextUpdate); err != nil {
			return err
		}
	}
	if !r.prog.Done(phaseSecretsDone) {
		if rr.addressChanged() {
			if err := r.restoreHandoff(); err != nil {
				return err
			}
		}
		if _, err := r.p.call(ctx, wgNode, "cluster.confirm-secrets", nil, remote.CallOptions{OnEvent: r.p.event}); err != nil {
			return err
		}
		if err := r.mark(phaseSecretsDone); err != nil {
			return err
		}
	}
	res := r.restoreResultData()
	if err := contexts.RemovePending(r.name); err != nil {
		return err
	}
	awaiting := "The databases await recovery: " + strings.Join(res.AwaitingRecovery, ", ") + "."
	if !r.cfg.BackupConfigured() {
		awaiting = "With no bucket the databases' data is lost with the old server: drop and create " + strings.Join(res.AwaitingRecovery, ", ") + " again."
	}
	msg := []string{
		fmt.Sprintf("Server %s is rebuilt on %s from master bundle %s.", r.name, rr.state.NewAddress, rr.state.Bundle),
		"Your context works as before. " + awaiting,
	}
	if rr.addressChanged() && r.cfg.TLS.Mode == "acme" {
		msg = append(msg, "Clients reconnect once "+r.cfg.MasterHostname()+" resolves to "+rr.state.NewAddress+".")
	}
	return output.Success(cmd, output.Result{Data: res, Message: strings.Join(msg, "\n"), Warnings: r.warnings, Next: res.Next})
}

// updateContext points the context at the new address.
func (r *initRun) updateContext() error {
	rr := r.restore
	c, err := contexts.Load(rr.ctx.Name)
	if err != nil {
		return err
	}
	for i := range c.Nodes {
		if c.Nodes[i].Role == contexts.RoleMaster || len(c.Nodes) == 1 {
			if c.Nodes[i].PublicAddress != rr.state.NewAddress {
				r.warnings = append(r.warnings, output.Warning{Code: "context_endpoint_updated",
					Message: "context " + c.Name + " now reaches the server at " + rr.state.NewAddress + " (was " + c.Nodes[i].PublicAddress + ")"})
			}
			c.Nodes[i].PublicAddress = rr.state.NewAddress
			if k := r.prog.WGPublicKey; k != "" && k != c.Nodes[i].WGPublicKey {
				// A bundle without the WireGuard key made a new one.
				r.warnings = append(r.warnings, output.Warning{Code: "context_wg_key_updated",
					Message: "the rebuilt server has a new WireGuard key; context " + c.Name + " follows it, other machines need eru secrets export and eru login"})
				c.Nodes[i].WGPublicKey = k
			}
		}
	}
	c.LockdownConfirmedAt = time.Now().UTC().Format(time.RFC3339)
	rr.ctx = c
	return contexts.Save(c)
}

// restoreHandoff shows a new secrets handoff: the address and the
// break-glass text changed.
func (r *initRun) restoreHandoff() error {
	if MustNotPrompt(r.cmd) {
		r.warnings = append(r.warnings, output.Warning{Code: "secrets_document_stale",
			Message: "the server's address changed; run eru secrets export at a terminal and store the new handoff"})
		return nil
	}
	_, err := showContextHandoff(r.cmd, r.restore.ctx)
	return err
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
		res.Next = append(res.Next, "eru db recover --all")
	default:
		// Without a bucket there is no stanza to recover from.
		for _, db := range res.AwaitingRecovery {
			res.Next = append(res.Next, "eru db drop "+db+" --confirm "+db, "eru db create "+db)
		}
	}
	res.Next = append(res.Next, "eru doctor", "eru server backup list")
	return res
}

// phaseTimer records how long each phase took, for the RTO.
type phaseTimer struct{ state *contexts.RestoreProgress }

func newPhaseTimer(st *contexts.RestoreProgress) *phaseTimer {
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
