package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/steps"
)

// Commit-confirm files and units (§8.3, §8.4).
const (
	SSHSnapshotDir    = "/var/lib/eru/ssh-snapshot"
	snapshotDropIn    = "sshd-dropin.conf"
	snapshotRules     = "ufw-rules.txt"
	snapshotMeta      = "snapshot.json"
	revertedFile      = "reverted.json"
	revertUnit        = "eru-ssh-revert"
	unlockUnit        = steps.SSHUnlockUnit
	defaultLockdownS  = 120
	maxLockdownS      = 2 * 60 * 60
	revertLockTimeout = 60 * time.Second
	maxUnlockMinutes  = 24 * 60
	defaultUnlockMins = 30
	lockdownStepID    = "ssh.lockdown"
	revertStepID      = "ssh-revert"
	unlockStepID      = "unlock-ssh"
	snapshotDirMode   = 0o700
	snapshotFileMode  = 0o600
)

type lockdownParams struct {
	TimeoutS int `json:"timeout_s"`
	// KeepPublic leaves public 22/tcp open until ssh.confirm. --local uses it,
	// because no operator is on WireGuard yet to prove the tunnel works.
	KeepPublic bool `json:"keep_public"`
}

// LockdownResult is the ssh.lockdown result.
type LockdownResult struct {
	Armed         bool   `json:"armed"`
	AlreadyLocked bool   `json:"already_locked"`
	TimeoutS      int    `json:"timeout_s,omitempty"`
	RevertUnit    string `json:"revert_unit,omitempty"`
	KeepPublic    bool   `json:"keep_public"`
	ArmedAt       string `json:"armed_at,omitempty"`
}

type snapshotInfo struct {
	TakenAt       string `json:"taken_at"`
	DropInPresent bool   `json:"dropin_present"`
}

// RevertResult is what ssh-revert writes to reverted.json and prints.
type RevertResult struct {
	RevertedAt    string   `json:"reverted_at"`
	Mode          string   `json:"mode"`
	Skipped       string   `json:"skipped,omitempty"`
	RestoredRules []string `json:"restored_rules,omitempty"`
	Error         string   `json:"error,omitempty"`
}

func shellFor(env *Env) engine.Shell {
	return engine.Shell{Secrets: secrets.Default, Log: env.Log}
}

func runScript(ctx context.Context, sh engine.Shell, stepID, name string, vars map[string]string, report engine.Reporter) error {
	return sh.Run(ctx, stepID, steps.Script(name), vars, report)
}

func armTimer(ctx context.Context, sh engine.Shell, stepID, unit string, seconds int, args string, report engine.Reporter) error {
	return runScript(ctx, sh, stepID, "revert-timer", map[string]string{
		"UNIT": unit, "ACTION": "arm", "ON_ACTIVE": strconv.Itoa(seconds), "EXEC_ARGS": args,
	}, report)
}

func disarmTimer(ctx context.Context, sh engine.Shell, stepID, unit string, report engine.Reporter) error {
	return runScript(ctx, sh, stepID, "revert-timer", map[string]string{"UNIT": unit, "ACTION": "disarm"}, report)
}

func ufwChange(ctx context.Context, sh engine.Shell, stepID string, add, del []string, report engine.Reporter) error {
	return runScript(ctx, sh, stepID, "ufw-change", map[string]string{
		"UFW_ADD": strings.Join(add, "\n"), "UFW_DELETE": strings.Join(del, "\n"),
	}, report)
}

func handleSSHLockdown(ctx context.Context, env *Env) (Result, error) {
	var p lockdownParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if p.TimeoutS == 0 {
		p.TimeoutS = defaultLockdownS
	}
	if p.TimeoutS < 30 || p.TimeoutS > maxLockdownS {
		return Result{}, output.NewError(output.CodeValidation, "timeout_s must be 30-7200", output.ExitConfig, "")
	}
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return Result{}, err
	}
	if st.LockedDownAt != "" && !st.LockdownPending {
		return Result{Data: LockdownResult{AlreadyLocked: true, KeepPublic: p.KeepPublic}}, nil
	}
	sh := shellFor(env)

	// 1. Snapshot. A re-armed lockdown keeps the first snapshot, which is the
	// state before any lockdown.
	if _, err := os.Stat(filepath.Join(SSHSnapshotDir, snapshotMeta)); err != nil || !st.LockdownPending {
		if err := takeSnapshot(ctx); err != nil {
			return Result{}, err
		}
	}
	_ = os.Remove(filepath.Join(SSHSnapshotDir, revertedFile))

	// 2. Arm the revert before changing anything.
	if err := armTimer(ctx, sh, lockdownStepID, revertUnit, p.TimeoutS, "node ssh-revert", env.Report); err != nil {
		return Result{}, err
	}
	armedAt := time.Now().UTC().Format(time.RFC3339)
	if err := engine.UpdateState(func(s *engine.State) error { s.LockdownPending = true; return nil }); err != nil {
		return Result{}, err
	}

	// 3. Apply: final drop-in, SSH on wg0, and public 22 closed unless kept.
	if err := runScript(ctx, sh, lockdownStepID, "sshd-dropin", steps.SSHDropInEnv(steps.SSHDropInPath, steps.RenderSSHDropIn(env.Cluster), true), env.Report); err != nil {
		return Result{}, err
	}
	var del []string
	if !p.KeepPublic {
		del = []string{steps.RulePublicSSH}
	}
	if err := ufwChange(ctx, sh, lockdownStepID, []string{steps.RuleWGSSH}, del, env.Report); err != nil {
		return Result{}, err
	}
	return Result{Data: LockdownResult{
		Armed:      true,
		TimeoutS:   p.TimeoutS,
		RevertUnit: revertUnit + ".timer",
		KeepPublic: p.KeepPublic,
		ArmedAt:    armedAt,
	}}, nil
}

func takeSnapshot(ctx context.Context) error {
	if err := os.RemoveAll(SSHSnapshotDir); err != nil {
		return err
	}
	if err := os.MkdirAll(SSHSnapshotDir, snapshotDirMode); err != nil {
		return err
	}
	info := snapshotInfo{TakenAt: time.Now().UTC().Format(time.RFC3339)}
	if raw, err := os.ReadFile(steps.SSHDropInPath); err == nil {
		info.DropInPresent = true
		if err := engine.WriteFileAtomic(filepath.Join(SSHSnapshotDir, snapshotDropIn), raw, snapshotFileMode); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	rules, err := steps.UFWRules(ctx)
	if err != nil {
		return err
	}
	if err := engine.WriteFileAtomic(filepath.Join(SSHSnapshotDir, snapshotRules), []byte(strings.Join(rules, "\n")+"\n"), snapshotFileMode); err != nil {
		return err
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(filepath.Join(SSHSnapshotDir, snapshotMeta), raw, snapshotFileMode)
}

type confirmParams struct {
	// Auto is set by an operator that is connected over WireGuard right now.
	// With no lockdown pending and public SSH still open, it closes public
	// 22/tcp, since the connection itself proves the tunnel works (§8.5).
	Auto bool `json:"auto"`
}

// ConfirmResult is the ssh.confirm result.
type ConfirmResult struct {
	Confirmed     bool   `json:"confirmed"`
	AlreadyLocked bool   `json:"already_locked"`
	LockedDownAt  string `json:"locked_down_at"`
}

func handleSSHConfirm(ctx context.Context, env *Env) (Result, error) {
	var p confirmParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return Result{}, err
	}
	if st.LockedDownAt != "" && !st.LockdownPending {
		return Result{Data: ConfirmResult{AlreadyLocked: true, LockedDownAt: st.LockedDownAt}}, nil
	}
	if !st.LockdownPending && !p.Auto {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"no SSH lockdown is pending",
			output.ExitPrecondition,
			"The revert timer may have fired already. Read ssh.status, then lock down again.",
		).WithDetails(map[string]any{"reason": "no_lockdown_pending"})
	}
	sh := shellFor(env)
	if st.LockdownPending {
		if err := disarmTimer(ctx, sh, "ssh.confirm", revertUnit, env.Report); err != nil {
			return Result{}, err
		}
	}
	if err := ufwChange(ctx, sh, "ssh.confirm", []string{steps.RuleWGSSH}, []string{steps.RulePublicSSH}, env.Report); err != nil {
		return Result{}, err
	}
	_ = os.RemoveAll(SSHSnapshotDir)
	lockedAt := time.Now().UTC().Format(time.RFC3339)
	if err := engine.UpdateState(func(s *engine.State) error {
		s.LockdownPending = false
		s.LockedDownAt = lockedAt
		return nil
	}); err != nil {
		return Result{}, err
	}
	return Result{Data: ConfirmResult{Confirmed: true, LockedDownAt: lockedAt}}, nil
}

// SSHStatus is the ssh.status result, used for lockdown diagnostics.
type SSHStatus struct {
	LockdownPending    bool          `json:"lockdown_pending"`
	LockedDownAt       string        `json:"locked_down_at,omitempty"`
	SnapshotPresent    bool          `json:"snapshot_present"`
	RevertTimerActive  bool          `json:"revert_timer_active"`
	UnlockTimerActive  bool          `json:"unlock_timer_active"`
	Reverted           *RevertResult `json:"reverted"`
	BreakGlassDropInOn bool          `json:"break_glass_dropin"`
}

func handleSSHStatus(ctx context.Context, _ *Env) (Result, error) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return Result{}, err
	}
	status := SSHStatus{LockdownPending: st.LockdownPending, LockedDownAt: st.LockedDownAt}
	if _, err := os.Stat(filepath.Join(SSHSnapshotDir, snapshotMeta)); err == nil {
		status.SnapshotPresent = true
	}
	if raw, err := os.ReadFile(filepath.Join(SSHSnapshotDir, revertedFile)); err == nil {
		var r RevertResult
		if json.Unmarshal(raw, &r) == nil {
			status.Reverted = &r
		}
	}
	if out, _ := steps.Probe(ctx, "systemctl", "is-active", revertUnit+".timer"); out == "active" {
		status.RevertTimerActive = true
	}
	if out, _ := steps.Probe(ctx, "systemctl", "is-active", unlockUnit+".timer"); out == "active" {
		status.UnlockTimerActive = true
	}
	if _, err := os.Stat(steps.BreakGlassPath); err == nil {
		status.BreakGlassDropInOn = true
	}
	return Result{Data: status}, nil
}

// RevertSSH is eru node ssh-revert. Without lockdownOnly it restores the
// commit-confirm snapshot when a lockdown is still pending (§8.3). With
// lockdownOnly it ends a break-glass window: the break-glass drop-in goes and
// public 22/tcp closes again when the node was locked down (§8.4).
func RevertSSH(ctx context.Context, lockdownOnly bool, report engine.Reporter) (RevertResult, error) {
	if os.Geteuid() != 0 {
		return RevertResult{}, rootRequired("eru node ssh-revert")
	}
	release, err := engine.Acquire(ctx, engine.LockExclusive, "ssh-revert", "", revertLockTimeout)
	if err != nil {
		return RevertResult{}, err
	}
	defer release()
	sh := engine.Shell{Secrets: secrets.Default}
	res := RevertResult{RevertedAt: time.Now().UTC().Format(time.RFC3339), Mode: "snapshot"}
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return res, err
	}

	if lockdownOnly {
		res.Mode = "lockdown-only"
		_ = disarmTimer(ctx, sh, revertStepID, unlockUnit, report)
		if err := runScript(ctx, sh, revertStepID, "sshd-dropin", steps.SSHDropInEnv(steps.BreakGlassPath, "", false), report); err != nil {
			return res, err
		}
		if st.LockedDownAt != "" && !st.LockdownPending {
			if err := ufwChange(ctx, sh, revertStepID, []string{steps.RuleWGSSH}, []string{steps.RulePublicSSH}, report); err != nil {
				return res, err
			}
		} else {
			res.Skipped = "the node was never locked down; public 22/tcp stays open"
		}
		return res, nil
	}

	if !st.LockdownPending {
		res.Skipped = "no lockdown is pending"
		return res, writeReverted(res)
	}
	revertErr := restoreSnapshot(ctx, sh, &res, report)
	if revertErr != nil {
		res.Error = secrets.Redact(revertErr.Error())
	}
	if err := engine.UpdateState(func(s *engine.State) error { s.LockdownPending = false; return nil }); err != nil && revertErr == nil {
		revertErr = err
	}
	if err := writeReverted(res); err != nil && revertErr == nil {
		revertErr = err
	}
	return res, revertErr
}

func restoreSnapshot(ctx context.Context, sh engine.Shell, res *RevertResult, report engine.Reporter) error {
	rawMeta, err := os.ReadFile(filepath.Join(SSHSnapshotDir, snapshotMeta))
	if err != nil {
		return errors.New("the SSH snapshot is missing")
	}
	var info snapshotInfo
	if err := json.Unmarshal(rawMeta, &info); err != nil {
		return errors.New("the SSH snapshot does not parse")
	}
	dropIn := ""
	if info.DropInPresent {
		raw, err := os.ReadFile(filepath.Join(SSHSnapshotDir, snapshotDropIn))
		if err != nil {
			return err
		}
		dropIn = string(raw)
	}
	rawRules, err := os.ReadFile(filepath.Join(SSHSnapshotDir, snapshotRules))
	if err != nil {
		return err
	}
	var rules []string
	hadWG := false
	for _, line := range strings.Split(string(rawRules), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rules = append(rules, line)
			hadWG = hadWG || line == steps.RuleWGSSH
		}
	}
	// Firewall first, so SSH is reachable on the snapshot's terms even if the
	// sshd reload fails.
	var del []string
	if !hadWG {
		del = []string{steps.RuleWGSSH}
	}
	if err := ufwChange(ctx, sh, revertStepID, rules, del, report); err != nil {
		return err
	}
	res.RestoredRules = rules
	return runScript(ctx, sh, revertStepID, "sshd-dropin", steps.SSHDropInEnv(steps.SSHDropInPath, dropIn, false), report)
}

func writeReverted(res RevertResult) error {
	if err := os.MkdirAll(SSHSnapshotDir, snapshotDirMode); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(filepath.Join(SSHSnapshotDir, revertedFile), append(raw, '\n'), snapshotFileMode)
}

// UnlockResult is the eru node unlock-ssh result.
type UnlockResult struct {
	AdminUser  string `json:"admin_user"`
	Minutes    int    `json:"minutes"`
	OpenUntil  string `json:"open_until"`
	DryRun     bool   `json:"dry_run"`
	RevertUnit string `json:"revert_unit"`
}

// UnlockSSH is eru node unlock-ssh: break-glass from the provider console
// (§8.4). It opens public 22/tcp, allows password login for the admin user
// only, and arms a timer that closes both again. It runs on the server as
// root; on an operator machine it says so instead of a config error.
func UnlockSSH(ctx context.Context, minutes int, dryRun bool, report engine.Reporter) (UnlockResult, error) {
	if _, err := os.Stat(config.ClusterPath); err != nil {
		if os.IsNotExist(err) {
			return UnlockResult{}, unlockSSHNotOnServer()
		}
		return UnlockResult{}, err
	}
	if minutes <= 0 {
		minutes = defaultUnlockMins
	}
	if minutes > maxUnlockMinutes {
		return UnlockResult{}, output.NewError(output.CodeValidation, "--minutes must be at most 1440", output.ExitConfig, "")
	}
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return UnlockResult{}, err
	}
	res := UnlockResult{
		AdminUser:  cfg.Server.AdminUser,
		Minutes:    minutes,
		OpenUntil:  time.Now().UTC().Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339),
		DryRun:     dryRun,
		RevertUnit: unlockUnit + ".timer",
	}
	if dryRun {
		return res, nil
	}
	if os.Geteuid() != 0 {
		return res, rootRequired("eru node unlock-ssh")
	}
	release, err := engine.Acquire(ctx, engine.LockExclusive, "unlock-ssh", "", revertLockTimeout)
	if err != nil {
		return res, err
	}
	defer release()
	sh := engine.Shell{Secrets: secrets.Default}
	if err := armTimer(ctx, sh, unlockStepID, unlockUnit, minutes*60, "node ssh-revert --lockdown-only", report); err != nil {
		return res, err
	}
	if err := ufwChange(ctx, sh, unlockStepID, []string{steps.RulePublicSSH}, nil, report); err != nil {
		return res, err
	}
	if err := runScript(ctx, sh, unlockStepID, "sshd-dropin", steps.SSHDropInEnv(steps.BreakGlassPath, steps.RenderBreakGlass(cfg.Server.AdminUser), false), report); err != nil {
		return res, err
	}
	return res, nil
}

func rootRequired(command string) error {
	return output.NewError(
		output.CodePrecondition,
		command+" must run as root",
		output.ExitPrecondition,
		"Run it with sudo, or as root from the provider console.",
	).WithDetails(map[string]any{"reason": "root_required"})
}

// unlockSSHNotOnServer is what eru node unlock-ssh returns on an operator
// machine, where there is no cluster.toml: the command opens break-glass on
// the server, so it must run there.
func unlockSSHNotOnServer() *output.CLIError {
	return output.NewError(
		output.CodePrecondition,
		"eru node unlock-ssh runs on the server, as root, not on this machine",
		output.ExitPrecondition,
		"Open break-glass from the server: log in at the provider's console (or a root shell), then run sudo eru node unlock-ssh --minutes 30 there. From this machine there is nothing to run until WireGuard works; check eru tunnel status, and that UDP reaches the server.",
	).WithDetails(map[string]any{"reason": "runs_on_server", "path": config.ClusterPath})
}
