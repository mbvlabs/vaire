package cli

import (
	"context"
	"strings"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/pelletier/go-toml/v2"
)

// phaseBackups is recorded once init has configured backups (Phase 3). It
// runs after the certificate wait and before the handoff, so the handoff
// carries the recovery kit, and the node holds the bucket keys and cipher
// pass before the first db create.
const phaseBackups = "backups_configured"

// answersBackupCreds reads the answers file's [backup.credentials].
func answersBackupCreds(raw []byte) (keyID, secret string) {
	var wrap struct {
		Backup struct {
			Credentials struct {
				AccessKeyID     string `toml:"access_key_id"`
				SecretAccessKey string `toml:"secret_access_key"`
			} `toml:"credentials"`
		} `toml:"backup"`
	}
	if toml.Unmarshal(raw, &wrap) != nil {
		return "", ""
	}
	return wrap.Backup.Credentials.AccessKeyID, wrap.Backup.Credentials.SecretAccessKey
}

// collectBackups asks init's Backups page: configure a bucket now, or skip
// for now and run eru backup setup later. Flags and the answers file answer
// it without a prompt.
func (r *initRun) collectBackups(cfg *config.Cluster, answersRaw []byte) error {
	cmd := r.cmd
	s := settingsFromConfig(*cfg, nil)
	s.KeyID, s.Secret = answersBackupCreds(answersRaw)
	if err := applyBackupFlags(cmd, initBackupFlags, &s); err != nil {
		return err
	}
	if s.Provider == "" && !MustNotPrompt(cmd) {
		choice, err := chooseOption(cmd, "Backups", [][2]string{
			{"Configure an S3-compatible bucket now", "configure"},
			{"Skip for now (eru backup setup later)", "skip"},
		})
		if err != nil {
			return err
		}
		if choice == "skip" {
			return nil
		}
		if err := askBackupSettings(&s); err != nil {
			return err
		}
	} else if s.Provider != "" && !MustNotPrompt(cmd) && (s.Bucket == "" || s.KeyID == "" || s.Secret == "") {
		if err := askBackupSettings(&s); err != nil {
			return err
		}
	}
	if s.Provider == "" {
		return nil
	}
	fillPreset(&s)
	if err := requireBackupSettings(cmd, initBackupFlags, s, "server init"); err != nil {
		return err
	}
	for key, value := range s.configKeys() {
		if err := cfg.Set(key, value); err != nil {
			return err
		}
	}
	r.backup = &s
	return nil
}

// backupSettings returns the bucket answers, from this run or, on --resume,
// from the pending answers and recovery kit.
func (r *initRun) backupSettings() backupSettings {
	if r.backup != nil {
		return *r.backup
	}
	s := settingsFromConfig(r.cfg, r.sec.Backup)
	if r.sec.Backup != nil {
		s.KeyID, s.Secret = r.sec.Backup.AccessKeyID, r.sec.Backup.SecretAccessKey
	}
	return s
}

// ensureBackupKit creates the age recovery identity, which every server gets
// with or without a bucket, then probes the bucket before anything reaches
// the server and generates its cipher pass. Both are saved in the pending
// secrets before they are ever sent.
func (r *initRun) ensureBackupKit(ctx context.Context) error {
	kit, created, err := ensureRecoveryIdentity(r.sec.Backup)
	if err != nil {
		return err
	}
	if created {
		r.sec.Backup = kit
		registerPending(r.sec)
		if err := contexts.SaveSecrets(r.name, r.sec); err != nil {
			return err
		}
	}
	if !r.cfg.BackupConfigured() || r.prog.Done(phaseBackups) {
		return nil
	}
	s := r.backupSettings()
	if s.Secret == "" {
		return output.NewError(output.CodeInputRequired, "server init needs the bucket's secret key", output.ExitInput,
			"Pass --backup-secret-stdin or --backup-secret-file.").WithDetails(map[string]any{"flag": "--backup-secret-file"})
	}
	r.p.note("checking the bucket " + s.Bucket + " at " + s.Endpoint)
	if err := r.p.during("checking the bucket "+s.Bucket, func() error { return backup.Probe(ctx, s.target(), r.initPrefixOwner()) }); err != nil {
		return err
	}
	kit, _, err = prepareKit(r.sec.Backup, s, false)
	if err != nil {
		return err
	}
	r.sec.Backup = kit
	registerPending(r.sec)
	return contexts.SaveSecrets(r.name, r.sec)
}

// initPrefixOwner is a fresh server as the bucket probe sees it: no node and
// no stanzas yet. The probe runs before the host phase, so once that phase is
// done an earlier run of this init already probed the prefix, and an epoch
// in it is this node's own first one.
func (r *initRun) initPrefixOwner() backup.PrefixOwner {
	return backup.PrefixOwner{Server: r.name, Resumed: r.prog.Done(phaseHost)}
}

// nodeConfig is cluster.toml as the host and database phases send it:
// without [backup], which the backups phase writes through config.set so
// eru backup setup and init share one path.
func (r *initRun) nodeConfig() config.Cluster {
	if r.restore != nil {
		// A rebuild's cluster.toml already has [backup].
		return r.cfg
	}
	cfg := r.cfg
	cfg.Backup = config.DefaultCluster().Backup
	return cfg
}

// configureInitBackups runs the backups phase. Switching WAL archiving on is
// a reload, so nothing restarts.
func (r *initRun) configureInitBackups(ctx context.Context, caller remote.Caller) error {
	if !r.cfg.BackupConfigured() || r.prog.Done(phaseBackups) {
		return nil
	}
	started, completed := initBackupPhaseText(r.backupSettings().Bucket, len(r.cfg.Clusters))
	r.p.phase("init.backups", "started", started)
	_, warnings, err := configureBackups(ctx, caller, r.p, r.backupSettings(), r.sec.Backup)
	if err != nil {
		return err
	}
	r.warnings = append(r.warnings, warnings...)
	r.p.phase("init.backups", "completed", completed)
	r.backupsDone = true
	return r.mark(phaseBackups)
}

// backupSummary is the Backups row of init's summary.
func (r *initRun) backupSummary() string {
	if !r.cfg.BackupConfigured() {
		return "none off the server: run eru backup setup"
	}
	s := r.backupSettings()
	state := "first full backup taken"
	if len(r.cfg.Clusters) == 0 {
		state = "each database's first full backup runs at db create"
	}
	return strings.Join([]string{s.Provider, s.Bucket + "/" + s.Prefix, state}, "  ")
}

// initBackupPhaseText is the backups phase's started and completed lines.
// The server starts with no databases, so the phase then only stores the
// bucket keys and cipher pass on the node; each db create takes its own
// first full backup.
func initBackupPhaseText(bucket string, databases int) (started, completed string) {
	if databases == 0 {
		return "store the bucket keys and cipher pass for " + bucket + " on the server",
			"backups configured; each database's first full backup runs at db create"
	}
	return "turn on WAL archiving and take the first full backup to " + bucket, "WAL archiving on, first full backup taken"
}

// firstMasterBundle makes the server's first master bundle before init
// reports ready. With a bucket, the node writes epoch 1 and uploads the
// bundle, so a fresh server has an offsite copy of its control state at
// once instead of at the next change or the daily run. It runs after the
// secrets are confirmed, so the bundle holds the final state. A failure
// doesn't undo the server, but init says so in a warning.
func (r *initRun) firstMasterBundle(ctx context.Context, caller remote.Caller) {
	detail := "keep the first master bundle on the server"
	if r.cfg.BackupConfigured() {
		detail = "upload the first master bundle to " + r.backupSettings().Bucket
	}
	r.p.phase("init.master-backup", "started", detail)
	res, err := masterBundleNow(ctx, r.p, caller)
	if w := firstBundleWarning(r.cfg.BackupConfigured(), r.backupSettings().Bucket, res, err); w != nil {
		r.warnings = append(r.warnings, *w)
		r.p.phase("init.master-backup", "failed", w.Message)
		return
	}
	r.p.phase("init.master-backup", "completed", res.Name)
}

// masterBundleNow runs master-backup.now: build a bundle when the state
// changed, and with a bucket make sure the bucket holds it.
func masterBundleNow(ctx context.Context, p *progress, caller remote.Caller) (node.MasterBackupResult, error) {
	var res node.MasterBackupResult
	reply, err := p.call(ctx, caller, "master-backup.now", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return res, err
	}
	return res, reply.Decode(&res)
}

// firstBundleWarning is master_backup_failed when the master bundle failed,
// or, with a bucket, didn't reach it; nil when it is where it belongs.
func firstBundleWarning(bucket bool, name string, res node.MasterBackupResult, err error) *output.Warning {
	const noCopy = ". The bucket has no copy of the server's current control state until eru server backup now succeeds"
	switch {
	case err != nil && bucket:
		return &output.Warning{Code: "master_backup_failed", Message: "the master bundle didn't reach the bucket " + name + ": " + err.Error() + noCopy}
	case err != nil:
		return &output.Warning{Code: "master_backup_failed", Message: "the first master bundle failed: " + err.Error() + "; run eru server backup now"}
	case bucket && !res.Uploaded && !res.Verified:
		return &output.Warning{Code: "master_backup_failed", Message: "master bundle " + res.Name + " isn't confirmed in the bucket " + name + noCopy}
	}
	return nil
}
