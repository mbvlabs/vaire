package cli

import (
	"context"
	"strings"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/pelletier/go-toml/v2"
)

// phaseBackups is recorded once init has configured backups (Phase 3). It
// runs after the certificate wait and before the handoff, so the handoff
// carries the recovery kit and follows the first full backup.
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

// ensureBackupKit probes the bucket before anything reaches the server and
// generates the recovery kit (cipher pass, dump age identity), saving it in
// the pending secrets before it is ever sent.
func (r *initRun) ensureBackupKit(ctx context.Context) error {
	if !r.cfg.BackupConfigured() || r.prog.Done(phaseBackups) {
		return nil
	}
	s := r.backupSettings()
	if s.Secret == "" {
		return output.NewError(output.CodeInputRequired, "server init needs the bucket's secret key", output.ExitInput,
			"Pass --backup-secret-stdin or --backup-secret-file.").WithDetails(map[string]any{"flag": "--backup-secret-file"})
	}
	r.p.note("checking the bucket " + s.Bucket + " at " + s.Endpoint)
	if err := backup.Probe(ctx, s.target(), r.name); err != nil {
		return err
	}
	kit, _, err := prepareKit(r.sec.Backup, s, r.name, false)
	if err != nil {
		return err
	}
	r.sec.Backup = kit
	registerPending(r.sec)
	return contexts.SaveSecrets(r.name, r.sec)
}

// nodeConfig is cluster.toml as the host and database phases send it:
// without [backup], which the backups phase writes through config.set so
// eru backup setup and init share one path.
func (r *initRun) nodeConfig() config.Cluster {
	cfg := r.cfg
	cfg.Backup = config.DefaultCluster().Backup
	return cfg
}

// configureInitBackups runs the backups phase. Init has no traffic yet, so
// the restart WAL archiving needs is confirmed with the cluster's token.
func (r *initRun) configureInitBackups(ctx context.Context, caller remote.Caller) error {
	if !r.cfg.BackupConfigured() || r.prog.Done(phaseBackups) {
		return nil
	}
	r.p.phase("init.backups", "started", "turn on WAL archiving and take the first full backup to "+r.backupSettings().Bucket)
	data, warnings, err := configureBackups(ctx, caller, r.p, r.backupSettings(), r.sec.Backup, r.name+"/restart")
	if err != nil {
		return err
	}
	r.warnings = append(r.warnings, warnings...)
	detail := "WAL archiving on"
	if data.Restarted {
		detail += ", PostgreSQL restarted once"
	}
	r.p.phase("init.backups", "completed", detail)
	r.backupsDone = true
	return r.mark(phaseBackups)
}

// backupSummary is the Backups row of init's summary.
func (r *initRun) backupSummary() string {
	if !r.cfg.BackupConfigured() {
		return "not configured: run eru backup setup"
	}
	s := r.backupSettings()
	return strings.Join([]string{s.Provider, s.Bucket + "/" + s.Prefix, "first full backup taken"}, "  ")
}
