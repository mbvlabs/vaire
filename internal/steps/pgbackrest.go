package steps

import (
	"bufio"
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/secrets"
)

const pgbackrestID = "pgbackrest-v1"

// BackupEnvPath holds the bucket keys and the dump age recipient for the
// node-side jobs that talk to the bucket directly (long-retention dumps).
const BackupEnvPath = RuntimeDir + "/backup.env"

// backup.env keys.
const (
	envKeyID     = "AWS_ACCESS_KEY_ID"
	envSecret    = "AWS_SECRET_ACCESS_KEY" //nolint:gosec // G101: a key name, not a value.
	envRecipient = "ERU_DUMP_AGE_RECIPIENT"
)

// redactedValue stands in for a secret the plan cannot know.
const redactedValue = "«redacted»"

type pgbackrestStep struct{}

func (pgbackrestStep) ID() string { return pgbackrestID }

func (pgbackrestStep) Describe(config.Cluster) string {
	return "render pgbackrest.conf, create the stanza, check WAL archiving and take the first full backup"
}

// NeedsRestartFirst: stanza-create and check need archive_mode on, which
// postgres-config-v1 turns on with a pending restart.
func (pgbackrestStep) NeedsRestartFirst() bool { return true }

// SkipReason skips the step while no bucket is configured.
func (pgbackrestStep) SkipReason(cfg config.Cluster) string {
	if !cfg.BackupConfigured() {
		return "backups_not_configured"
	}
	if _, ok := cfg.BackupCluster(); !ok {
		return "no_databases"
	}
	return ""
}

// backupValues are the effective bucket keys, cipher pass and age recipient.
type backupValues struct {
	render.BackupSecrets
	Recipient string
}

// ReadBackupEnv parses /etc/eru/runtime/backup.env.
func ReadBackupEnv() map[string]string {
	out := map[string]string{}
	f, err := os.Open(BackupEnvPath)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && !strings.HasPrefix(key, "#") {
			out[key] = value
			secrets.Add(value)
		}
	}
	return out
}

// BackupTarget is the bucket as the node sees it: cluster.toml plus the keys
// in backup.env.
func BackupTarget(cfg config.Cluster) (backup.Target, string, error) {
	env := ReadBackupEnv()
	if env[envKeyID] == "" || env[envSecret] == "" {
		return backup.Target{}, "", output.NewError(output.CodePrecondition, "the bucket keys are not on this node", output.ExitPrecondition,
			"Run eru backup setup, or eru apply, so the node gets them.").WithDetails(map[string]any{"reason": "backup_not_configured", "path": BackupEnvPath})
	}
	return backup.Target{
		Endpoint: cfg.Backup.Endpoint,
		Region:   cfg.Backup.Region,
		Bucket:   cfg.Backup.Bucket,
		Prefix:   cfg.Backup.Prefix,
		URIStyle: cfg.Backup.URIStyle,
		KeyID:    env[envKeyID],
		Secret:   env[envSecret],
	}, env[envRecipient], nil
}

// diskCipher reads repo1-cipher-pass from pgbackrest.conf.
func diskCipher() string {
	raw, err := os.ReadFile(backup.ConfPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "repo1-cipher-pass="); ok {
			secrets.Add(v)
			return v
		}
	}
	return ""
}

// resolve picks each value: the request's when the operator rotates or the
// node has none, otherwise what is on disk, so an operator with stale
// credentials never overwrites newer ones. The cipher pass of an existing
// repository never changes: a different one in the request is refused.
func resolveBackupValues(rt engine.Runtime) (backupValues, []string, error) {
	env := ReadBackupEnv()
	req := rt.SecretValues
	pick := func(disk, request string) string {
		if request != "" && (rt.RotateSecrets || disk == "") {
			return request
		}
		return disk
	}
	v := backupValues{
		BackupSecrets: render.BackupSecrets{
			KeyID:  pick(env[envKeyID], req[SecretBackupKeyID]),
			Secret: pick(env[envSecret], req[SecretBackupKeySecret]),
		},
		Recipient: pick(env[envRecipient], req[SecretBackupAgeRecipient]),
	}
	cipher := diskCipher()
	if cipher != "" && req[SecretBackupCipherPass] != "" && req[SecretBackupCipherPass] != cipher {
		if rt.RotateSecrets {
			return v, nil, output.NewError(output.CodePrecondition,
				"the request carries a different pgBackRest cipher pass than the repository uses",
				output.ExitPrecondition,
				"The cipher of an existing repository never changes. Log in again from a current transfer bundle (eru login), then retry.").
				WithDetails(map[string]any{"reason": "cipher_mismatch"})
		}
	}
	if cipher == "" {
		cipher = req[SecretBackupCipherPass]
	}
	v.CipherPass = cipher
	var missing []string
	if v.KeyID == "" {
		missing = append(missing, SecretBackupKeyID)
	}
	if v.Secret == "" {
		missing = append(missing, SecretBackupKeySecret)
	}
	if v.CipherPass == "" {
		missing = append(missing, SecretBackupCipherPass)
	}
	for _, s := range []string{v.KeyID, v.Secret, v.CipherPass} {
		secrets.Add(s)
	}
	return v, missing, nil
}

func backupEnv(v backupValues) string {
	lines := []string{
		"# managed by eru - bucket keys for node-side backup jobs",
		envKeyID + "=" + v.KeyID,
		envSecret + "=" + v.Secret,
	}
	if v.Recipient != "" {
		lines = append(lines, envRecipient+"="+v.Recipient)
	}
	return strings.Join(lines, "\n") + "\n"
}

// pgbackrestInputs is pgbackrest.conf rendered without its secrets.
func pgbackrestInputs(cfg config.Cluster, rt engine.Runtime) string {
	return string(render.PgBackRestConf(cfg, rt.Host, render.BackupSecrets{}))
}

func (pgbackrestStep) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	if !cfg.BackupConfigured() {
		return nil
	}
	v, missing, err := resolveBackupValues(rt)
	if err != nil || len(missing) > 0 {
		v.BackupSecrets = render.BackupSecrets{KeyID: redactedValue, Secret: redactedValue, CipherPass: redactedValue}
	}
	// The keys and cipher pass are registered secrets, so the plan diff shows
	// them as «redacted».
	return []engine.RenderedFile{{Path: backup.ConfPath, Content: render.PgBackRestConf(cfg, rt.Host, v.BackupSecrets)}}
}

func (pgbackrestStep) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	return canonical(map[string]any{
		"configured": cfg.BackupConfigured(),
		"conf":       pgbackrestInputs(cfg, rt),
		"script":     Script("pgbackrest"),
	})
}

// Force re-runs the step when the operator rotates the bucket keys, which are
// never part of the inputs, or when the node lost its copy of them.
func (pgbackrestStep) Force(cfg config.Cluster, rt engine.Runtime) string {
	if !cfg.BackupConfigured() {
		return ""
	}
	env := ReadBackupEnv()
	req := rt.SecretValues
	if req[SecretBackupKeySecret] == "" {
		return ""
	}
	if env[envSecret] == "" || diskCipher() == "" {
		return "backup_credentials_missing"
	}
	if rt.RotateSecrets && (req[SecretBackupKeySecret] != env[envSecret] || (req[SecretBackupKeyID] != "" && req[SecretBackupKeyID] != env[envKeyID]) ||
		(req[SecretBackupAgeRecipient] != "" && req[SecretBackupAgeRecipient] != env[envRecipient])) {
		return "backup_credentials_rotated"
	}
	return ""
}

func (pgbackrestStep) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	if !cfg.BackupConfigured() {
		return complete(), nil
	}
	v, missing, err := resolveBackupValues(rt)
	if err != nil || len(missing) > 0 {
		return incomplete("the bucket keys or cipher pass are missing"), nil
	}
	if !fileIsOwned(backup.ConfPath, string(render.PgBackRestConf(cfg, rt.Host, v.BackupSecrets)), 0o600, pgFileOwner, pgFileGroup) {
		return incomplete(backup.ConfPath + " differs"), nil
	}
	if _, err := os.Stat(BackupEnvPath); err != nil {
		return incomplete(BackupEnvPath + " is missing"), nil
	}
	info, err := driverFor(cfg, rt, nil).Info(ctx, "")
	if err != nil {
		return incomplete("pgbackrest info failed: " + err.Error()), nil
	}
	if info.StatusCode != 0 && !info.HasFull() {
		return incomplete("the stanza reports " + info.Status), nil
	}
	if !info.HasFull() {
		return incomplete("the repository has no full backup"), nil
	}
	return complete(), nil
}

func (s pgbackrestStep) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	v, missing, err := resolveBackupValues(rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return engine.ApplyResult{}, output.NewError(output.CodeInputRequired,
			"the bucket keys or the cipher pass are not on the node and were not supplied",
			output.ExitInput,
			"Run eru backup setup, or eru apply from a machine whose context has the recovery kit (eru login from a current transfer bundle).",
		).WithDetails(map[string]any{"secrets": missing})
	}
	local := "false"
	if cfg.Backup.Database.LocalRepo {
		local = "true"
	}
	if err := rt.Shell.Run(ctx, s.ID(), Script("pgbackrest"), map[string]string{"LOCAL_REPO": local}, report); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFile(BackupEnvPath, backupEnv(v), 0o600); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFileOwned(backup.ConfPath, string(render.PgBackRestConf(cfg, rt.Host, v.BackupSecrets)), 0o600, pgFileOwner, pgFileGroup); err != nil {
		return engine.ApplyResult{}, err
	}
	d := driverFor(cfg, rt, report)
	if err := d.StanzaCreate(ctx); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := d.Check(ctx); err != nil {
		return engine.ApplyResult{}, err
	}
	info, err := d.Info(ctx, "")
	if err != nil {
		return engine.ApplyResult{}, err
	}
	notes := []string{"stanza " + d.Stanza + " checked"}
	if !info.HasFull() {
		set, err := d.Backup(ctx, backup.TypeFull)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		RecordBackup(cfg, set)
		notes = append(notes, "first full backup "+set.Label)
	}
	return engine.ApplyResult{Restart: engine.RestartNone, Notes: notes}, nil
}

// driverFor is the pgBackRest driver for cfg.
func driverFor(cfg config.Cluster, rt engine.Runtime, report engine.Reporter) backup.PgBackRest {
	first, _ := cfg.BackupCluster()
	return backup.PgBackRest{Stanza: first.Stanza, Shell: rt.Shell, StepID: pgbackrestID, Report: report, PG: ClusterPG(first)}
}

// RecordBackup stores a successful backup in records.json and refreshes the
// eru.prom backup lines. Neither failing fails the backup.
func RecordBackup(cfg config.Cluster, set backup.Set) {
	finished := set.Stop
	if finished == "" {
		finished = time.Now().UTC().Format(time.RFC3339)
	}
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		s.LastBackup[set.Type] = backup.BackupResult{Label: set.Label, FinishedAt: finished, Size: set.Size}
		return nil
	})
	WriteMetrics(cfg)
}

// WriteMetrics refreshes eru.prom; a failure is ignored.
func WriteMetrics(cfg config.Cluster) {
	node := ""
	if n, ok := cfg.Master(); ok {
		node = n.Name
	}
	_ = backup.WriteMetrics(cfg.Name(), node)
}
