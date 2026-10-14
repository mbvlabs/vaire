package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/spf13/cobra"
)

// backupSettings are the answers of eru backup setup and of init's Backups
// page.
type backupSettings struct {
	Provider      string
	Endpoint      string
	Region        string
	Bucket        string
	Prefix        string
	URIStyle      string
	KeyID         string
	Secret        string
	FullSchedule  string
	DiffSchedule  string
	IncrSchedule  string
	RetentionFull int
}

// backupFlagNames maps the settings to flag names: eru backup setup uses
// --provider, server init --backup-provider and its --db-*-schedule flags.
type backupFlagNames struct {
	Provider, Endpoint, Region, Bucket, Prefix, URIStyle, KeyID, SecretStdin, SecretFile string
	FullSchedule, DiffSchedule, IncrSchedule, RetentionFull                              string
}

var setupFlags = backupFlagNames{
	Provider: "provider", Endpoint: "endpoint", Region: "region", Bucket: "bucket", Prefix: "prefix", URIStyle: "uri-style",
	KeyID: "key-id", SecretStdin: "secret-stdin", SecretFile: "secret-file",
	FullSchedule: "full-schedule", DiffSchedule: "diff-schedule", IncrSchedule: "incr-schedule", RetentionFull: "retention-full",
}

var initBackupFlags = backupFlagNames{
	Provider: "backup-provider", Endpoint: "backup-endpoint", Region: "backup-region", Bucket: "backup-bucket", Prefix: "backup-prefix",
	KeyID: "backup-key-id", SecretStdin: "backup-secret-stdin", SecretFile: "backup-secret-file",
	FullSchedule: "db-full-schedule", DiffSchedule: "db-diff-schedule",
}

// settingsFromConfig starts the answers from cluster.toml and the recovery
// kit, so a re-run keeps the bucket and only asks for what changes.
func settingsFromConfig(cfg config.Cluster, kit *contexts.BackupCreds) backupSettings {
	b := cfg.Backup
	s := backupSettings{
		Provider: b.Provider, Endpoint: b.Endpoint, Region: b.Region, Bucket: b.Bucket, Prefix: b.Prefix, URIStyle: b.URIStyle,
		FullSchedule: b.Database.FullSchedule, DiffSchedule: b.Database.DiffSchedule, IncrSchedule: b.Database.IncrSchedule,
		RetentionFull: b.Database.RetentionFull,
	}
	if s.Prefix == "" {
		s.Prefix = cfg.Name()
	}
	if kit != nil && kit.Bucket == b.Bucket {
		s.KeyID = kit.AccessKeyID
	}
	return s
}

// applyBackupFlags overlays the flags that were passed.
func applyBackupFlags(cmd *cobra.Command, names backupFlagNames, s *backupSettings) error {
	set := func(flag string, dst *string) {
		if flag != "" && flagChanged(cmd, flag) {
			*dst = stringFlag(cmd, flag)
		}
	}
	set(names.Provider, &s.Provider)
	set(names.Endpoint, &s.Endpoint)
	set(names.Region, &s.Region)
	set(names.Bucket, &s.Bucket)
	set(names.Prefix, &s.Prefix)
	set(names.URIStyle, &s.URIStyle)
	set(names.KeyID, &s.KeyID)
	set(names.FullSchedule, &s.FullSchedule)
	set(names.DiffSchedule, &s.DiffSchedule)
	set(names.IncrSchedule, &s.IncrSchedule)
	if names.RetentionFull != "" && flagChanged(cmd, names.RetentionFull) {
		n, err := cmd.Flags().GetInt(names.RetentionFull)
		if err != nil {
			return err
		}
		s.RetentionFull = n
	}
	switch {
	case flagTrue(cmd, names.SecretStdin):
		v, err := readFirstLine(cmd.InOrStdin())
		if err != nil {
			return err
		}
		s.Secret = strings.TrimSpace(v)
	case stringFlag(cmd, names.SecretFile) != "":
		raw, err := os.ReadFile(stringFlag(cmd, names.SecretFile))
		if err != nil {
			return output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		s.Secret = strings.TrimSpace(string(raw))
	}
	secrets.Add(s.Secret)
	return nil
}

// fillPreset derives the URI style, region and endpoint a provider implies.
func fillPreset(s *backupSettings) {
	preset, ok := backup.PresetFor(s.Provider)
	if !ok {
		return
	}
	if s.URIStyle == "" {
		s.URIStyle = preset.URIStyle
	}
	if s.Region == "" {
		s.Region = preset.Region
	}
	if s.Endpoint == "" && preset.Endpoint != "" && !strings.Contains(preset.Endpoint, "<account_id>") && s.Region != "" {
		s.Endpoint = backup.FillEndpoint(preset.Endpoint, s.Region)
	}
}

// requireBackupSettings fails with input_required naming the first missing
// value when eru must not prompt.
func requireBackupSettings(cmd *cobra.Command, names backupFlagNames, s backupSettings, action string) error {
	checks := []struct {
		ok   bool
		flag string
	}{
		{s.Provider != "", names.Provider},
		{s.Endpoint != "" && !strings.Contains(s.Endpoint, "<"), names.Endpoint},
		{s.Region != "", names.Region},
		{s.Bucket != "", names.Bucket},
		{s.KeyID != "", names.KeyID},
		{s.Secret != "", names.SecretStdin + " or --" + names.SecretFile},
	}
	for _, c := range checks {
		if !c.ok {
			return output.NewError(output.CodeInputRequired, action+" needs --"+c.flag, output.ExitInput,
				"Pass --"+c.flag+" to continue without a prompt.").WithDetails(map[string]any{"flag": "--" + c.flag})
		}
	}
	return nil
}

// backupFields are the interview questions for the bucket. The retention is
// phrased as a restore window: with weekly fulls, keeping N fulls lets any
// database rewind about N weeks.
func backupFields(s *backupSettings, weeks *string) []huh.Field {
	providers := make([]huh.Option[string], 0, len(backup.Presets))
	labels := map[string]string{"r2": "Cloudflare R2", "s3": "AWS S3", "b2": "Backblaze B2", "hetzner": "Hetzner Object Storage", "minio": "MinIO", "custom": "Other S3-compatible"}
	for _, p := range backup.Presets {
		providers = append(providers, huh.NewOption(labels[p.Provider], p.Provider))
	}
	if s.Provider == "" {
		s.Provider = "r2"
	}
	return []huh.Field{
		huh.NewSelect[string]().Title("Storage provider").Options(providers...).Value(&s.Provider),
		huh.NewInput().Title("Region").Description("R2 uses auto; Hetzner fsn1, nbg1 or hel1.").
			PlaceholderFunc(func() string { p, _ := backup.PresetFor(s.Provider); return p.Region }, &s.Provider).Value(&s.Region),
		huh.NewInput().Title("S3 endpoint").
			DescriptionFunc(func() string {
				p, _ := backup.PresetFor(s.Provider)
				if p.Endpoint == "" {
					return "The https:// URL of the S3 API."
				}
				return "For example " + p.Endpoint
			}, &s.Provider).
			Validate(optional(config.CheckEndpoint)).Value(&s.Endpoint),
		huh.NewInput().Title("Bucket").Validate(config.CheckBucket).Value(&s.Bucket),
		huh.NewInput().Title("Prefix inside the bucket").Validate(config.CheckPrefix).Value(&s.Prefix),
		huh.NewInput().Title("Access key ID").Validate(func(v string) error { return nonEmpty(v, "an access key ID") }).Value(&s.KeyID),
		huh.NewInput().Title("Secret access key").EchoMode(huh.EchoModePassword).
			Validate(func(v string) error { return nonEmpty(v, "the secret key") }).Value(&s.Secret),
		huh.NewSelect[string]().Title("Restore window").
			Description("How far back you can rewind any database. Longer windows keep more full backups in the bucket.").
			Options(huh.NewOption("about 1 week", "1"), huh.NewOption("about 2 weeks", "2"), huh.NewOption("about 4 weeks", "4"),
				huh.NewOption("about 8 weeks", "8"), huh.NewOption("about 12 weeks", "12")).
			Value(weeks),
	}
}

func nonEmpty(v, what string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", what)
	}
	return nil
}

// askBackupSettings runs the bucket questions in a terminal.
func askBackupSettings(s *backupSettings) error {
	weeks := strconv.Itoa(s.RetentionFull)
	if weeks == "0" {
		weeks = "4"
	}
	if err := ui.RunForm(huh.NewGroup(backupFields(s, &weeks)...).Title("Backups")); err != nil {
		return promptErr(err)
	}
	if n, err := strconv.Atoi(weeks); err == nil {
		s.RetentionFull = n
	}
	s.Region, s.Endpoint = strings.TrimSpace(s.Region), strings.TrimSpace(s.Endpoint)
	secrets.Add(s.Secret)
	return nil
}

// target is the bucket the settings name.
func (s backupSettings) target() backup.Target {
	return backup.Target{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: s.Prefix, URIStyle: s.URIStyle, KeyID: s.KeyID, Secret: s.Secret}
}

// configKeys are the dotted cluster.toml keys config.set writes.
func (s backupSettings) configKeys() map[string]string {
	keys := map[string]string{
		"backup.provider":  s.Provider,
		"backup.endpoint":  strings.TrimSuffix(s.Endpoint, "/"),
		"backup.region":    s.Region,
		"backup.bucket":    s.Bucket,
		"backup.prefix":    s.Prefix,
		"backup.uri_style": s.URIStyle,
	}
	if s.FullSchedule != "" {
		keys["backup.database.full_schedule"] = s.FullSchedule
	}
	if s.DiffSchedule != "" {
		keys["backup.database.diff_schedule"] = s.DiffSchedule
	}
	keys["backup.database.incr_schedule"] = s.IncrSchedule
	if s.RetentionFull > 0 {
		keys["backup.database.retention_full"] = strconv.Itoa(s.RetentionFull)
	}
	return keys
}

// ensureRecoveryIdentity returns kit with the age recovery identity, creating
// the kit or the identity when missing. server init calls it whether or not a
// bucket is configured; backup setup keeps the identity it finds.
func ensureRecoveryIdentity(kit *contexts.BackupCreds) (*contexts.BackupCreds, bool, error) {
	out := &contexts.BackupCreds{}
	if kit != nil {
		*out = *kit
	}
	if out.Dumps.AgeIdentity != "" {
		return out, false, nil
	}
	identity, recipient, err := backup.NewIdentity()
	if err != nil {
		return nil, false, err
	}
	out.Dumps.AgeIdentity, out.Dumps.AgeRecipient = identity, recipient
	secrets.Add(identity)
	return out, true, nil
}

// prepareKit fills the recovery kit for s, generating the pgBackRest cipher
// pass when this bucket has none yet and the recovery identity when the kit
// has none. A cluster whose backups are already on (wasConfigured) never gets
// a new cipher: that would orphan the repository.
func prepareKit(kit *contexts.BackupCreds, s backupSettings, wasConfigured bool) (*contexts.BackupCreds, bool, error) {
	sameRepo := kit != nil && kit.Bucket == s.Bucket && kit.Prefix == s.Prefix && kit.PgBackRest.CipherPass != ""
	if wasConfigured && !sameRepo {
		return nil, false, output.NewError(output.CodeInputRequired,
			"backups are on, but this machine's context has no recovery kit for "+s.Bucket+"/"+s.Prefix,
			output.ExitInput,
			"eru never makes a new cipher for an existing repository. Log in from a current transfer bundle (eru login), then retry.").
			WithDetails(map[string]any{"reason": "recovery_kit_missing"})
	}
	generated := false
	out := &contexts.BackupCreds{}
	if sameRepo {
		*out = *kit
	} else {
		pass, err := secrets.Base62(64)
		if err != nil {
			return nil, false, err
		}
		out.PgBackRest.CipherPass = pass
		if kit != nil {
			out.Dumps = kit.Dumps
		}
		generated = true
	}
	out, created, err := ensureRecoveryIdentity(out)
	if err != nil {
		return nil, false, err
	}
	generated = generated || created
	if out.AccessKeyID != s.KeyID || out.SecretAccessKey != s.Secret {
		generated = true
	}
	out.Provider, out.Endpoint, out.Region, out.Bucket, out.Prefix, out.URIStyle = s.Provider, strings.TrimSuffix(s.Endpoint, "/"), s.Region, s.Bucket, s.Prefix, s.URIStyle
	out.AccessKeyID, out.SecretAccessKey = s.KeyID, s.Secret
	for _, v := range out.SecretValues() {
		secrets.Add(v)
	}
	return out, generated, nil
}

// backupSecrets is the RPC secret map for the recovery kit.
func backupSecrets(kit *contexts.BackupCreds) map[string]string {
	if kit == nil {
		return nil
	}
	return map[string]string{
		steps.SecretBackupKeyID:        kit.AccessKeyID,
		steps.SecretBackupKeySecret:    kit.SecretAccessKey,
		steps.SecretBackupCipherPass:   kit.PgBackRest.CipherPass,
		steps.SecretBackupAgeRecipient: kit.Dumps.AgeRecipient,
	}
}

// requirePackagesCurrent refuses a node whose package step would still run,
// such as one set up before pgBackRest was part of it. eru apply is the
// update path; backup setup doesn't install packages itself.
func requirePackagesCurrent(ctx context.Context, caller remote.Caller, p *progress) error {
	reply, err := p.call(ctx, caller, "plan", map[string]any{"steps": steps.PackagesStepIDs()}, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var plan engine.PlanData
	if err := reply.Decode(&plan); err != nil {
		return err
	}
	if plan.WouldApply == 0 {
		return nil
	}
	return output.NewError(output.CodePrecondition,
		"the node's packages are out of date: pgBackRest isn't installed yet",
		output.ExitPrecondition,
		"Run eru apply to bring the node up to date, then run eru backup setup again.",
	).WithDetails(map[string]any{"reason": "node_outdated", "steps": steps.PackagesStepIDs()})
}

// configureBackups writes [backup] through config.set and runs the backup
// steps: every cluster's archive_command (a reload), the repository with a
// stanza and first full backup per cluster, and the timers. kit must already
// be saved on the operator.
func configureBackups(ctx context.Context, caller remote.Caller, p *progress, s backupSettings, kit *contexts.BackupCreds) (engine.ApplyData, []output.Warning, error) {
	if err := requirePackagesCurrent(ctx, caller, p); err != nil {
		return engine.ApplyData{}, nil, err
	}
	reply, err := p.call(ctx, caller, "config.get", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return engine.ApplyData{}, nil, err
	}
	var current configData
	if err := reply.Decode(&current); err != nil {
		return engine.ApplyData{}, nil, err
	}
	setReply, err := p.call(ctx, caller, "config.set", map[string]any{"set": s.configKeys(), "base_revision": current.Revision},
		remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return engine.ApplyData{}, nil, err
	}
	applyReply, err := p.call(ctx, caller, "apply", map[string]any{"steps": steps.BackupStepIDs(), "rotate_backup_secrets": true}, remote.CallOptions{
		Secrets: backupSecrets(kit),
		OnEvent: p.event,
	})
	if err != nil {
		return engine.ApplyData{}, setReply.Warnings, err
	}
	var data engine.ApplyData
	if err := applyReply.Decode(&data); err != nil {
		return data, nil, err
	}
	return data, append(setReply.Warnings, applyReply.Warnings...), nil
}

func runBackupSetup(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx := commandContext(cmd)
	cfg, _, err := remoteConfig(cmd, s, p)
	if err != nil {
		return err
	}
	creds, err := contexts.LoadCredentials(s.ctx.Name)
	if err != nil {
		return err
	}
	settings := settingsFromConfig(cfg, creds.Backup)
	if err := applyBackupFlags(cmd, setupFlags, &settings); err != nil {
		return err
	}
	wasConfigured := cfg.BackupConfigured()
	if wasConfigured && (settings.Bucket != cfg.Backup.Bucket || settings.Prefix != cfg.Backup.Prefix || strings.TrimSuffix(settings.Endpoint, "/") != cfg.Backup.Endpoint) {
		return output.NewError(output.CodePrecondition,
			"backups already go to "+cfg.Backup.Bucket+"/"+cfg.Backup.Prefix+"; moving them would lose the restore window",
			output.ExitPrecondition, "Keep the bucket and prefix. Re-running setup with them rotates the keys.").
			WithDetails(map[string]any{"reason": "repo_in_use"})
	}
	if !MustNotPrompt(cmd) {
		if err := askBackupSettings(&settings); err != nil {
			return err
		}
	}
	fillPreset(&settings)
	if err := requireBackupSettings(cmd, setupFlags, settings, "backup setup"); err != nil {
		return err
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{
			Data:     map[string]any{"config": settings.configKeys(), "steps": steps.BackupStepIDs(), "dry_run": true},
			Message:  "dry run: would probe " + settings.Bucket + ", write [backup] and run " + strings.Join(steps.BackupStepIDs(), ", "),
			Warnings: s.warnings,
		})
	}
	p.note("checking the bucket " + settings.Bucket + " at " + settings.Endpoint)
	if err := p.during("checking the bucket "+settings.Bucket, func() error { return backup.Probe(ctx, settings.target(), cfg.Name()) }); err != nil {
		return err
	}
	kit, generated, err := prepareKit(creds.Backup, settings, wasConfigured)
	if err != nil {
		return err
	}
	// The kit is saved before anything reaches the node: a repository
	// encrypted with a cipher the operator then lost would be unreadable.
	creds.Backup = kit
	if err := contexts.SaveCredentials(s.ctx.Name, creds); err != nil {
		return err
	}
	data, warnings, err := configureBackups(ctx, s.node, p, settings, kit)
	if err != nil {
		return err
	}
	warnings = append(s.warnings, warnings...)
	// A changed kit is shown in a new handoff right here when there is a
	// terminal; the warning and the export step remain only when the
	// operator didn't store it during this command.
	stored := false
	if generated && !MustNotPrompt(cmd) {
		if ok, _ := PromptYes(cmd, "Show the secrets handoff with the recovery kit now?"); ok {
			// show returns nil only once the operator typed 'stored'.
			if _, err := showContextHandoff(cmd, s.ctx); err != nil {
				return err
			}
			stored = true
		}
	}
	next := []string{"eru backup list"}
	if generated && !stored {
		warnings = append(warnings, output.Warning{
			Code:    "recovery_kit_changed",
			Message: "the recovery kit changed; run eru secrets export and store the new handoff",
		})
	}
	if !stored {
		next = append([]string{"eru secrets export"}, next...)
	}
	message := []string{fmt.Sprintf("backups go to %s/%s (%s); applied %d of %d steps", settings.Bucket, settings.Prefix, settings.Provider, data.Applied, data.Total)}
	if list, err := p.call(ctx, s.node, "backup.list", nil, remote.CallOptions{OnEvent: p.event}); err == nil {
		var bl backupRowsData
		if list.Decode(&bl) == nil {
			for _, r := range bl.Databases {
				if r.DroppedAt != "" {
					continue
				}
				line := r.Database + ": restore window " + r.Window.describe()
				if r.Latest != nil {
					line = r.Database + ": latest backup " + r.Latest.Label + " (" + humanBytes(r.Latest.Size) + "), restore window " + r.Window.describe()
				}
				message = append(message, line)
			}
		}
	}
	res := output.Result{
		Data:     map[string]any{"apply": data, "bucket": settings.Bucket, "prefix": settings.Prefix},
		Message:  strings.Join(message, "\n"),
		Warnings: warnings,
		Next:     next,
	}
	return output.Success(cmd, res)
}

// backupListData is backup.list's result for one database (--db) as the
// operator reads it.
type backupListData struct {
	Stanza   string                    `json:"stanza"`
	Status   string                    `json:"status"`
	Window   windowData                `json:"restore_window"`
	Archive  backup.ArchiveStatus      `json:"archive"`
	Sets     []backup.Set              `json:"sets"`
	Database string                    `json:"database"`
	Dropped  *backup.DroppedStanza     `json:"dropped,omitempty"`
	Verify   *backup.VerifyResult      `json:"last_verify,omitempty"`
	Drill    *backup.Drill             `json:"last_drill,omitempty"`
	Dumps    []backup.Dump             `json:"dumps"`
	Policies map[string]any            `json:"dump_policies,omitempty"`
	Restores map[string]backup.Restore `json:"restores,omitempty"`
}

// backupRowData is one database's row of backup.list without --db.
type backupRowData struct {
	Database  string               `json:"database"`
	Stanza    string               `json:"stanza"`
	Status    string               `json:"status"`
	Window    windowData           `json:"restore_window"`
	Sets      int                  `json:"sets"`
	Latest    *backup.Set          `json:"latest,omitempty"`
	Verify    *backup.VerifyResult `json:"last_verify,omitempty"`
	Drill     *backup.Drill        `json:"last_drill,omitempty"`
	Dump      *backup.Dump         `json:"last_dump,omitempty"`
	DroppedAt string               `json:"dropped_at,omitempty"`
	ExpiresAt string               `json:"expires_at,omitempty"`
}

// backupRowsData is backup.list's result without --db.
type backupRowsData struct {
	Databases []backupRowData           `json:"databases"`
	Restores  map[string]backup.Restore `json:"restores,omitempty"`
}

type windowData struct {
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	LagSeconds int64  `json:"archive_lag_seconds"`
}

func (w windowData) describe() string {
	if w.From == "" {
		return "none yet"
	}
	return fmt.Sprintf("%s to %s (archive lag %ds)", w.From, w.To, w.LagSeconds)
}

// backupDatabases are the databases a backup command runs for: --db, else
// every database in cluster.toml, by name.
func backupDatabases(cmd *cobra.Command, s *session, p *progress) ([]string, error) {
	if db := stringFlag(cmd, "db"); db != "" {
		return []string{db}, nil
	}
	reply, err := p.call(commandContext(cmd), s.node, "db.list", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return nil, err
	}
	var data struct {
		Databases []databaseInfo `json:"databases"`
	}
	if err := reply.Decode(&data); err != nil {
		return nil, err
	}
	var names []string
	for _, db := range data.Databases {
		if db.InConfig {
			names = append(names, db.Name)
		}
	}
	if len(names) == 0 {
		return nil, output.NewError(output.CodePrecondition, "there is no database to back up", output.ExitPrecondition,
			"Create one with eru db create <name>.").WithDetails(map[string]any{"reason": "no_databases"})
	}
	return names, nil
}

// dbJobOutcome is one database's job of backup now or verify without --db.
type dbJobOutcome struct {
	Database string            `json:"database"`
	JobID    string            `json:"job_id,omitempty"`
	Result   any               `json:"result,omitempty"`
	Error    *output.ErrorBody `json:"error,omitempty"`
}

// eachDatabase runs method once per database, one after another: each job
// takes the server's heavy-I/O lock, so they can't run side by side. A
// failed or skipped database doesn't stop the others. Ctrl-C detaches from
// the running job and starts no more; detached is then that job's id.
// result decodes one job's result; line describes it.
func eachDatabase(cmd *cobra.Command, s *session, p *progress, dbs []string, method string, params func(db string) any,
	result func(raw []byte) (any, string, error)) (outcomes []dbJobOutcome, lines []string, warnings []output.Warning, detached *string) {
	for _, db := range dbs {
		if len(dbs) > 1 {
			p.note(db + ":")
		}
		reply, jobID, wasDetached, callErr := callJob(cmd, s, p, method, params(db), remote.CallOptions{})
		if wasDetached {
			return outcomes, lines, warnings, &jobID
		}
		o := dbJobOutcome{Database: db, JobID: jobID}
		if callErr == nil {
			var jr jobReply
			if callErr = reply.Decode(&jr); callErr == nil {
				var line string
				o.JobID = jr.JobID
				if o.Result, line, callErr = result(jr.Result); callErr == nil {
					lines = append(lines, line)
					warnings = append(warnings, reply.Warnings...)
				}
			}
		}
		if callErr != nil {
			body := output.ErrorBody{Code: output.CodeError, Message: callErr.Error()}
			var ce *output.CLIError
			if errors.As(callErr, &ce) {
				body = output.ErrorBody{Code: ce.Code, Message: ce.Error(), Hint: ce.Hint, Details: ce.Details}
			}
			o.Error = &body
			lines = append(lines, db+": "+body.Message)
		}
		outcomes = append(outcomes, o)
	}
	return outcomes, lines, warnings, nil
}

// failedOutcomes is the error of a per-database run in which some failed:
// the first failure's code, every outcome in details.
func failedOutcomes(what, key string, outcomes []dbJobOutcome) error {
	var failed []string
	var first *output.ErrorBody
	for _, o := range outcomes {
		if o.Error != nil {
			failed = append(failed, o.Database)
			if first == nil {
				first = o.Error
			}
		}
	}
	if first == nil {
		return nil
	}
	return output.NewError(first.Code, fmt.Sprintf("%s failed for %s: %s", what, strings.Join(failed, ", "), first.Message), output.ExitFor(first.Code),
		first.Hint).WithDetails(map[string]any{key: outcomes, "failed": failed})
}

func runBackupNow(cmd *cobra.Command, _ []string) error {
	kind := stringFlag(cmd, "type")
	switch kind {
	case "", backup.TypeFull, backup.TypeDiff, backup.TypeIncr:
	default:
		return output.NewError(output.CodeUsage, "--type must be full, diff or incr", output.ExitUsage, "")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if dryRun(cmd) {
		reply, err := p.call(commandContext(cmd), s.node, "backup.now", map[string]string{"type": kind, "database": stringFlag(cmd, "db")},
			remote.CallOptions{DryRun: true, OnEvent: p.event})
		if err != nil {
			return err
		}
		var data struct {
			Backups []struct {
				Database string `json:"database"`
				Type     string `json:"type"`
			} `json:"backups"`
		}
		_ = reply.Decode(&data)
		lines := make([]string, 0, len(data.Backups))
		for _, b := range data.Backups {
			lines = append(lines, fmt.Sprintf("dry run: would take a %s backup of %s", b.Type, b.Database))
		}
		return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: append(s.warnings, reply.Warnings...)})
	}
	dbs, err := backupDatabases(cmd, s, p)
	if err != nil {
		return err
	}
	outcomes, lines, warnings, detached := eachDatabase(cmd, s, p, dbs, "backup.now",
		func(db string) any { return map[string]string{"type": kind, "database": db} },
		func(raw []byte) (any, string, error) {
			var res struct {
				DB  string     `json:"db"`
				Set backup.Set `json:"set"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return nil, "", err
			}
			return res.Set, fmt.Sprintf("%s: backup %s (%s) finished: %s", res.DB, res.Set.Label, res.Set.Type, humanBytes(res.Set.Size)), nil
		})
	if detached != nil {
		return detachedEach(cmd, s, *detached, "the backup", notStarted(dbs, outcomes), func(db string) string {
			if kind != "" {
				return "eru backup now --db " + db + " --type " + kind
			}
			return "eru backup now --db " + db
		})
	}
	if err := failedOutcomes("the backup", "backups", outcomes); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"backups": outcomes},
		Message:  strings.Join(lines, "\n"),
		Warnings: append(s.warnings, warnings...),
		Next:     []string{"eru backup list"},
	})
}

// notStarted is the databases a detached per-database run never reached:
// the ones after the detached job.
func notStarted(dbs []string, outcomes []dbJobOutcome) []string {
	return dbs[min(len(outcomes)+1, len(dbs)):]
}

// detachedEach is the result of Ctrl-C on a per-database run: the running
// job keeps going on the node, and the databases after it weren't started.
// The node skips a job while another holds the heavy-I/O lock, so the
// message (and next steps) give the command for each of them to run once
// the job is done.
func detachedEach(cmd *cobra.Command, s *session, jobID, what string, rest []string, command func(db string) string) error {
	if len(rest) == 0 {
		return detachedResult(cmd, s, jobID, what)
	}
	commands := make([]string, len(rest))
	for i, db := range rest {
		commands[i] = command(db)
	}
	message := "Detached; " + what + " keeps running on the node."
	next := []string{"eru job list"}
	if jobID != "" {
		message += " Re-attach with eru job follow " + jobID + "."
		next = []string{"eru job follow " + jobID}
	}
	message += "\nNot started for " + strings.Join(rest, ", ") + ": each job takes the heavy-I/O lock, so once this one is done, start the rest with " +
		strings.Join(commands, ", then ") + "."
	data := map[string]any{"job_id": jobID, "detached": true, "not_started": rest, "commands": commands}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: s.warnings, Next: append(next, commands...)})
}

func runBackupList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	db := stringFlag(cmd, "db")
	reply, err := p.call(commandContext(cmd), s.node, "backup.list", map[string]string{"database": db}, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	if db == "" {
		var data backupRowsData
		if err := reply.Decode(&data); err != nil {
			return err
		}
		return output.Success(cmd, output.Result{Data: data, Message: strings.Join(backupRowLines(data, time.Now()), "\n"), Warnings: warnings})
	}
	var data backupListData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	lines := []string{"Restore window for " + db + ": " + data.Window.describe()}
	if data.Dropped != nil {
		lines = append(lines, "Dropped at "+data.Dropped.DroppedAt+"; stanza "+data.Stanza+" is kept until "+data.Dropped.ExpiresAt)
	}
	lines = append(lines, "", fmt.Sprintf("  %-24s %-5s %-21s %10s %10s", "SET", "TYPE", "FINISHED", "SIZE", "IN REPO"))
	for _, set := range data.Sets {
		lines = append(lines, fmt.Sprintf("  %-24s %-5s %-21s %10s %10s", set.Label, set.Type, set.Stop, humanBytes(set.Size), humanBytes(set.RepoSize)))
	}
	if data.Verify != nil {
		lines = append(lines, "", "Last verify: "+verifyState(data.Verify)+" at "+data.Verify.FinishedAt)
	}
	if data.Drill != nil {
		lines = append(lines, "Last drill: "+data.Drill.Result+" at "+data.Drill.FinishedAt+" (backup "+data.Drill.BackupLabel+")")
	}
	if len(data.Dumps) > 0 {
		lines = append(lines, "", "Long-retention dumps:")
		for _, d := range data.Dumps {
			lines = append(lines, fmt.Sprintf("  %-20s %-24s %10s", d.Database, d.ID, humanBytes(d.Size)))
		}
	}
	for name, r := range data.Restores {
		lines = append(lines, "", "Open restore of "+name+": "+r.Phase+" (target "+r.Target+")")
	}
	return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: warnings})
}

// backupRowLines renders backup list without --db: a row per database, then
// the dropped databases' kept stanzas. Times are ages, to keep a row within
// about 120 columns; backup list --db has the full times.
func backupRowLines(data backupRowsData, now time.Time) []string {
	rows := [][]string{{"DATABASE", "STANZA", "STATUS", "SETS", "LATEST", "VERIFY", "DRILL", "DUMP", "RESTORE WINDOW"}}
	var dropped []string
	for _, r := range data.Databases {
		if r.DroppedAt != "" {
			dropped = append(dropped, fmt.Sprintf("  %s (%s, %d sets): dropped %s, kept until %s; window %s", r.Database, r.Stanza, r.Sets, r.DroppedAt, r.ExpiresAt, r.Window.describe()))
			continue
		}
		latest, verify, drill, dump := "-", "-", "-", "-"
		if r.Latest != nil {
			latest = r.Latest.Type + " " + ago(r.Latest.Stop, now)
		}
		if r.Verify != nil {
			verify = verifyState(r.Verify) + " " + ago(r.Verify.FinishedAt, now)
		}
		if r.Drill != nil {
			drill = drillState(r.Drill) + " " + ago(r.Drill.FinishedAt, now)
		}
		if r.Dump != nil {
			dump = ago(r.Dump.Created, now)
		}
		rows = append(rows, []string{r.Database, r.Stanza, r.Status, strconv.Itoa(r.Sets), latest, verify, drill, dump, r.Window.short()})
	}
	lines := alignColumns(rows)
	if len(dropped) > 0 {
		lines = append(lines, "", "Dropped databases (restorable until their stanza expires):")
		lines = append(lines, dropped...)
	}
	for name, r := range data.Restores {
		lines = append(lines, "", "Open restore of "+name+": "+r.Phase+" (target "+r.Target+")")
	}
	return lines
}

// short is the restore window in a table cell: where it starts and the
// archive lag, which says how far behind now it ends.
func (w windowData) short() string {
	if w.From == "" {
		return "none yet"
	}
	from := w.From
	if t, err := time.Parse(time.RFC3339, w.From); err == nil {
		from = t.UTC().Format("2006-01-02 15:04Z")
	}
	return fmt.Sprintf("from %s, lag %s", from, compactDuration(time.Duration(w.LagSeconds)*time.Second))
}

// compactDuration is d in its largest whole unit: 45s, 12m, 3h, 2d.
func compactDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// alignColumns pads each column of rows to its widest cell, two spaces
// apart and indented by two; the last column isn't padded.
func alignColumns(rows [][]string) []string {
	var widths []int
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], len(cell))
		}
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		var b strings.Builder
		b.WriteString(" ")
		for i, cell := range row {
			b.WriteString(" ")
			if i == len(row)-1 {
				b.WriteString(cell)
				break
			}
			fmt.Fprintf(&b, "%-*s ", widths[i], cell)
		}
		lines = append(lines, b.String())
	}
	return lines
}

// ago is an RFC 3339 time as a compact age: "just now", "5m ago", "2h ago",
// "3d ago". A time that doesn't parse is shown as it is.
func ago(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	return compactDuration(d) + " ago"
}

// drillState is a drill result in one word, like verifyState.
func drillState(d *backup.Drill) string {
	switch d.Result {
	case backup.DrillPass:
		return "passed"
	case backup.DrillFail:
		return "FAILED"
	}
	return d.Result
}

// verifyState is a verify result in one word.
func verifyState(v *backup.VerifyResult) string {
	switch {
	case v == nil:
		return "-"
	case v.OK:
		return "passed"
	}
	return "FAILED"
}

func runBackupVerify(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dbs, err := backupDatabases(cmd, s, p)
	if err != nil {
		return err
	}
	outcomes, lines, warnings, detached := eachDatabase(cmd, s, p, dbs, "backup.verify",
		func(db string) any { return map[string]string{"database": db} },
		func(raw []byte) (any, string, error) {
			var res backup.VerifyResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return nil, "", err
			}
			return res, "pgbackrest verify passed at " + res.FinishedAt, nil
		})
	if detached != nil {
		return detachedEach(cmd, s, *detached, "the verify", notStarted(dbs, outcomes), func(db string) string {
			return "eru backup verify --db " + db
		})
	}
	if err := failedOutcomes("pgbackrest verify", "verifies", outcomes); err != nil {
		return err
	}
	for i := range lines {
		lines[i] = outcomes[i].Database + ": " + lines[i]
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"verifies": outcomes},
		Message:  strings.Join(lines, "\n"),
		Warnings: append(s.warnings, warnings...),
	})
}

// humanBytes renders a size for humans.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// parseTarget reads --time: RFC 3339, or relative like -15m or -2h.
func parseTarget(v string, now time.Time) (time.Time, error) {
	if strings.HasPrefix(v, "-") {
		d, err := time.ParseDuration(strings.TrimPrefix(v, "-"))
		if err != nil {
			return time.Time{}, output.NewError(output.CodeUsage, "--time "+v+" is not a duration like -15m", output.ExitUsage, "")
		}
		return now.Add(-d).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, output.NewError(output.CodeUsage, "--time "+v+" is neither RFC 3339 (2026-10-09T12:00:00Z) nor relative (-15m)", output.ExitUsage, "")
	}
	return t.UTC(), nil
}

// echoTime shows a target in UTC and the cluster's timezone.
func echoTime(t time.Time, tz string) string {
	out := t.UTC().Format(time.RFC3339)
	if loc, err := time.LoadLocation(tz); err == nil && tz != "UTC" {
		out += " (" + t.In(loc).Format("2006-01-02 15:04:05 MST") + ")"
	}
	return out
}
