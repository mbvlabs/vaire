package steps

import (
	"bufio"
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/secrets"
)

const pgbackrestID = "pgbackrest-v1"

// BackupEnvPath holds the bucket keys and the dump age recipient for the
// node-side jobs that talk to the bucket directly (long-retention dumps).
const BackupEnvPath = RuntimeDir + "/backup.env"

// backup.env keys.
const (
	envKeyID     = "AWS_ACCESS_KEY_ID"
	envSecret    = "AWS_SECRET_ACCESS_KEY" //nolint:gosec // G101: a key name, not a value.
	envRecipient = "VAIRE_DUMP_AGE_RECIPIENT"
)

// redactedValue stands in for a secret the plan cannot know.
const redactedValue = "«redacted»"

type pgbackrestStep struct{}

func (pgbackrestStep) ID() string { return pgbackrestID }

func (pgbackrestStep) Describe(config.Cluster) string {
	return "render pgbackrest.conf with a stanza per database, then create each stanza, check its WAL archiving and take its first full backup"
}

// SkipReason skips the step while no bucket is configured. With no database
// yet it still runs: it stores the bucket keys and the cipher pass on the
// node (backup.env and a pgbackrest.conf without stanzas), because server
// init and backup setup on an empty server are the requests that carry them
// and the first db create needs them. archive_mode is on from creation, so the step never
// waits for a restart: postgres-config-v1@<db> switches archive_command with
// a reload.
func (pgbackrestStep) SkipReason(cfg config.Cluster) string {
	if !cfg.BackupConfigured() {
		return "backups_not_configured"
	}
	return ""
}

// noDatabasesNote is the apply note of a run with no stanza to set up.
const noDatabasesNote = "bucket keys and cipher pass stored; no database to back up yet"

// backupValues are the effective bucket keys, cipher pass and age recipient.
type backupValues struct {
	render.BackupSecrets
	Recipient string
}

// ReadBackupEnv parses /etc/vaire/runtime/backup.env.
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
			"Run vaire backup setup, or vaire apply, so the node gets them.").WithDetails(map[string]any{"reason": "backup_not_configured", "path": BackupEnvPath})
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
				"The cipher of an existing repository never changes. Log in again from a current transfer bundle (vaire login), then retry.").
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
		"# managed by vaire - bucket keys for node-side backup jobs",
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
	if len(cfg.Clusters) == 0 {
		return complete(), nil
	}
	all, err := BackupDriver(config.ClusterSpec{}, rt.Shell, pgbackrestID, nil).InfoAll(ctx)
	if err != nil {
		return incomplete("pgbackrest info failed: " + err.Error()), nil
	}
	for _, c := range cfg.Clusters {
		if !all[c.Stanza].HasFull() {
			return incomplete("stanza " + c.Stanza + " (" + c.Name + ") has no full backup"), nil
		}
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
			"Run vaire backup setup, or vaire apply from a machine whose context has the recovery kit (vaire login from a current transfer bundle).",
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
	if len(cfg.Clusters) == 0 {
		return engine.ApplyResult{Restart: engine.RestartNone, Notes: []string{noDatabasesNote}}, nil
	}
	all, err := BackupDriver(config.ClusterSpec{}, rt.Shell, pgbackrestID, nil).InfoAll(ctx)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	var notes []string
	for _, c := range cfg.Clusters {
		// A stanza with a full backup is done; rotated keys check every
		// stanza again, so a key that cannot push WAL shows up now.
		if all[c.Stanza].HasFull() && !rt.RotateSecrets {
			continue
		}
		note, err := setupStanza(ctx, cfg, c, BackupDriver(c, rt.Shell, pgbackrestID, report))
		if err != nil {
			return engine.ApplyResult{}, err
		}
		notes = append(notes, note)
	}
	return engine.ApplyResult{Restart: engine.RestartNone, Notes: notes}, nil
}

// setupStanza creates one cluster's stanza, checks its WAL archiving and
// takes its first full backup when the stanza has none.
func setupStanza(ctx context.Context, cfg config.Cluster, c config.ClusterSpec, d backup.PgBackRest) (string, error) {
	if err := d.StanzaCreate(ctx); err != nil {
		return "", err
	}
	if err := d.Check(ctx); err != nil {
		return "", err
	}
	info, err := d.Info(ctx, "")
	if err != nil {
		return "", err
	}
	if info.HasFull() {
		return "stanza " + d.Stanza + " (" + c.Name + ") checked", nil
	}
	set, err := d.Backup(ctx, backup.TypeFull)
	if err != nil {
		return "", err
	}
	RecordBackup(cfg, c.Name, set)
	RecordRepo(ctx, cfg, d, c.Name)
	return "stanza " + d.Stanza + " (" + c.Name + ") checked, first full backup " + set.Label, nil
}

// BackupDriver is the pgBackRest driver for spec's stanza, connecting to its
// socket and port. A zero spec lists every stanza (InfoAll).
func BackupDriver(spec config.ClusterSpec, shell engine.Shell, stepID string, report engine.Reporter) backup.PgBackRest {
	return backup.PgBackRest{Stanza: spec.Stanza, Port: spec.Port, Shell: shell, StepID: stepID, Report: report, PG: ClusterPG(spec)}
}

// RecordBackup stores a successful backup of db in records.json and refreshes
// the vaire.prom backup lines. Neither failing fails the backup.
func RecordBackup(cfg config.Cluster, db string, set backup.Set) {
	finished := set.Stop
	if finished == "" {
		finished = time.Now().UTC().Format(time.RFC3339)
	}
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		s.RecordBackup(db, set, finished)
		return nil
	})
	WriteMetrics(cfg)
}

// RecordBackupFailure stores a failed backup of db in records.json and
// refreshes vaire.prom.
func RecordBackupFailure(cfg config.Cluster, db, kind string) {
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		s.RecordBackupFailure(db, kind, time.Now().UTC().Format(time.RFC3339))
		return nil
	})
	WriteMetrics(cfg)
}

// RecordRepo reads db's stanza once with pgbackrest info and stores its
// facts (window start, sets, repository size) for vaire.prom, in place of a
// pgbackrest_exporter polling the bucket. A failure leaves the old facts.
func RecordRepo(ctx context.Context, cfg config.Cluster, d backup.PgBackRest, db string) {
	info, err := d.Info(ctx, "")
	if err != nil {
		return
	}
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		s.RecordRepo(db, backup.FactsFrom(info, time.Now().UTC().Format(time.RFC3339)))
		return nil
	})
	WriteMetrics(cfg)
}

// WriteMetrics refreshes vaire.prom; a failure is ignored.
func WriteMetrics(cfg config.Cluster) {
	node := ""
	if n, ok := cfg.Master(); ok {
		node = n.Name
	}
	_ = backup.WriteMetrics(cfg.Name(), node)
}
