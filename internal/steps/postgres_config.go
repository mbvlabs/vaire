package steps

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/tune"
)

const postgresConfigID = "postgres-config-v1"

// managedFile is one rendered file with its owner mode.
type managedFile struct {
	Path    string
	Content string
	Mode    os.FileMode
	Owner   string
	Group   string
	// removed marks a file Apply deleted, kept only so a failed validation
	// puts it back.
	removed bool
}

// BackupConfPath is conf.d/40-eru-backup.conf, which exists only while
// [backup] names a bucket.
const BackupConfPath = PGConfD + "/40-eru-backup.conf"

// confFiles returns the conf.d files in load order. 90-overrides.conf is
// written only when missing. 40-eru-backup.conf is in the list only while
// backups are configured; otherwise Apply removes it.
func confFiles(cfg config.Cluster, rt engine.Runtime) []managedFile {
	res := tuningFor(cfg, rt.Host)
	files := []managedFile{
		{Path: PGConfD + "/10-eru-tuning.conf", Content: string(render.TuningConf(res)), Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup},
		{Path: PGConfD + "/20-eru-security.conf", Content: string(render.SecurityConf()), Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup},
		{Path: PGConfD + "/30-eru-extensions.conf", Content: string(render.ExtensionsConf(cfg, res)), Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup},
	}
	if cfg.BackupConfigured() {
		files = append(files, managedFile{Path: BackupConfPath, Content: string(render.BackupConf(cfg)), Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup})
	}
	return append(files,
		managedFile{Path: PGConfD + "/50-eru-settings.conf", Content: string(render.SettingsConf(cfg)), Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup},
		managedFile{Path: PGConfDir + "/pg_hba.conf", Content: string(render.HBA(cfg)), Mode: 0o640, Owner: pgFileOwner, Group: pgFileGroup},
		managedFile{Path: PGConfDir + "/pg_ident.conf", Content: string(render.Ident(cfg)), Mode: 0o640, Owner: pgFileOwner, Group: pgFileGroup},
	)
}

// staleBackupConf reports whether 40-eru-backup.conf is on disk although
// backups are not configured.
func staleBackupConf(cfg config.Cluster) bool {
	if cfg.BackupConfigured() {
		return false
	}
	_, err := os.Stat(BackupConfPath)
	return err == nil
}

type postgresConfig struct{}

func (postgresConfig) ID() string { return postgresConfigID }

func (postgresConfig) Describe(config.Cluster) string {
	return "render conf.d, pg_hba and pg_ident, then reload or restart"
}

func (postgresConfig) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	files := confFiles(cfg, rt)
	out := make([]engine.RenderedFile, 0, len(files)+1)
	for _, f := range files {
		out = append(out, engine.RenderedFile{Path: f.Path, Content: []byte(f.Content)})
	}
	if staleBackupConf(cfg) {
		// Shown as a diff that removes every line.
		out = append(out, engine.RenderedFile{Path: BackupConfPath})
	}
	return out
}

func (postgresConfig) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	files := confFiles(cfg, rt)
	payload := make([]map[string]string, 0, len(files))
	for _, f := range files {
		payload = append(payload, map[string]string{"path": f.Path, "content": f.Content})
	}
	return canonical(map[string]any{"files": payload, "overrides": string(render.OverridesHeader())})
}

func (postgresConfig) Check(_ context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	for _, f := range confFiles(cfg, rt) {
		if !fileIsOwned(f.Path, f.Content, f.Mode, f.Owner, f.Group) {
			return incomplete(f.Path + " differs"), nil
		}
	}
	if _, err := os.Stat(PGConfD + "/90-overrides.conf"); err != nil {
		return incomplete("90-overrides.conf is missing"), nil
	}
	if staleBackupConf(cfg) {
		return incomplete(BackupConfPath + " is left from a removed [backup]"), nil
	}
	return complete(), nil
}

func (s postgresConfig) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	files := confFiles(cfg, rt)
	// Keep the previous contents so a validation failure can be rolled back.
	backups := make(map[string]string, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f.Path) //nolint:gosec // G304: managed file paths from the catalogue.
		if err == nil {
			backups[f.Path] = string(raw)
		}
	}
	if staleBackupConf(cfg) {
		raw, err := os.ReadFile(BackupConfPath)
		if err != nil {
			return engine.ApplyResult{}, fmt.Errorf("reading %s: %w", BackupConfPath, err)
		}
		if err := os.Remove(BackupConfPath); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("removing %s: %w", BackupConfPath, err)
		}
		// Restored with the others if PostgreSQL rejects the new files.
		files = append(files, managedFile{Path: BackupConfPath, Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup, removed: true})
		backups[BackupConfPath] = string(raw)
	}
	for _, f := range files {
		if f.removed {
			continue
		}
		if err := writeFileOwned(f.Path, f.Content, f.Mode, f.Owner, f.Group); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("writing the rendered configuration: %w", err)
		}
	}
	if _, err := os.Stat(PGConfD + "/90-overrides.conf"); err != nil {
		if err := writeFileOwned(PGConfD+"/90-overrides.conf", string(render.OverridesHeader()), 0o644, pgFileOwner, pgFileGroup); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("writing the rendered configuration: %w", err)
		}
	}
	if err := ensureIncludeDir(); err != nil {
		return engine.ApplyResult{}, fmt.Errorf("writing the rendered configuration: %w", err)
	}
	if err := validateConfig(ctx, cfg, rt); err != nil {
		restoreFiles(files, backups)
		return engine.ApplyResult{}, err
	}
	if err := rt.Shell.Run(ctx, s.ID(), Script("postgres-config"), map[string]string{"PG_CONF_DIR": PGConfDir}, report); err != nil {
		return engine.ApplyResult{}, err
	}

	if !stepCompleted(s.ID()) {
		if err := restartPostgres(ctx, cfg, rt, s.ID(), report); err != nil {
			return engine.ApplyResult{}, err
		}
		return engine.ApplyResult{Restart: engine.RestartNone, Notes: []string{"restarted after the first configuration"}}, nil
	}
	pending, err := pendingRestartSettings(ctx, rt)
	if err != nil {
		return engine.ApplyResult{Restart: engine.RestartReload}, nil
	}
	if len(pending) > 0 {
		return engine.ApplyResult{Restart: engine.RestartRestart, Notes: []string{"restart needed for " + strings.Join(pending, ", ")}}, nil
	}
	return engine.ApplyResult{Restart: engine.RestartReload}, nil
}

// ensureIncludeDir appends include_dir = 'conf.d' to postgresql.conf when the
// file does not already include it. Debian's own file is otherwise kept.
func ensureIncludeDir() error {
	path := PGConfDir + "/postgresql.conf"
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed cluster config path.
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "include_dir") {
			return nil
		}
	}
	content := string(raw)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "\n# managed by eru\ninclude_dir = 'conf.d'\n"
	return writeFileOwned(path, content, 0o644, pgFileOwner, pgFileGroup)
}

// validateConfig fails when PostgreSQL reports an error in the new files.
// pg_file_settings reports every changed postmaster setting as "setting could
// not be applied" because it needs a restart; those rows are skipped, and
// postgres -C parses the files as a starting server would to catch an invalid
// value among them.
func validateConfig(ctx context.Context, cfg config.Cluster, rt engine.Runtime) error {
	settings, err := rt.PG.Query(ctx, "postgres", `SELECT f.sourcefile, f.sourceline, f.name, f.error
FROM pg_file_settings f LEFT JOIN pg_settings s ON s.name = f.name
WHERE f.error IS NOT NULL AND NOT (f.error = 'setting could not be applied' AND s.context = 'postmaster')
ORDER BY f.seqno`, nil)
	if err != nil {
		return fmt.Errorf("validating the rendered configuration: %w", err)
	}
	hba, err := rt.PG.Query(ctx, "postgres", "SELECT file_name, line_number, error FROM pg_hba_file_rules WHERE error IS NOT NULL ORDER BY rule_number", nil)
	if err != nil {
		return fmt.Errorf("validating the rendered configuration: %w", err)
	}
	var problems []string
	for _, row := range settings {
		problems = append(problems, fmt.Sprintf("%s:%s %s: %s", row["sourcefile"], row["sourceline"], row["name"], row["error"]))
	}
	for _, row := range hba {
		problems = append(problems, fmt.Sprintf("%s:%s: %s", row["file_name"], row["line_number"], row["error"]))
	}
	if len(problems) == 0 {
		if _, err := Probe(ctx, "runuser", "-u", "postgres", "--",
			"/usr/lib/postgresql/"+strconv.Itoa(PGVersion)+"/bin/postgres",
			"-D", cfg.Server.DataDirectory, "-c", "config_file="+PGConfDir+"/postgresql.conf", "-C", "max_connections"); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) == 0 {
		return nil
	}
	details := map[string]any{"file_settings": settings, "hba_rules": hba, "problems": problems}
	return output.NewError(
		output.CodeStepFailed,
		"PostgreSQL rejected the rendered configuration: "+strings.Join(problems, "; "),
		output.ExitStep,
		"The previous files were restored. Fix the settings named in the message.",
	).WithDetails(details)
}

// pendingRestartSettings lists the settings that need a restart.
func pendingRestartSettings(ctx context.Context, rt engine.Runtime) ([]string, error) {
	rows, err := rt.PG.Query(ctx, "postgres", "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name", nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row["name"])
	}
	sort.Strings(names)
	return names, nil
}

// stepCompleted reports whether the journal has a completed entry for id.
func stepCompleted(id string) bool {
	j, err := engine.LoadJournal(engine.JournalPath)
	if err != nil {
		return false
	}
	entry, ok := j.Steps[id]
	return ok && entry.Status == engine.StatusCompleted
}

// restartPostgres stops PostgreSQL, sizes huge pages while it is stopped, and
// starts it again.
func restartPostgres(ctx context.Context, cfg config.Cluster, rt engine.Runtime, stepID string, report engine.Reporter) error {
	if err := rt.Shell.Run(ctx, stepID, Script("pg-stop"), nil, report); err != nil {
		return err
	}
	res := tuningFor(cfg, rt.Host)
	if hugePagesEnabled(res) {
		hp, err := hugePagesFromKernel(ctx, cfg.Server.DataDirectory)
		if err == nil && hp > 0 {
			hp = hp + hp/20
			if err := writeFile(SysctlPath, string(render.SysctlConf(rt.Host, hp)), 0o644); err != nil {
				return err
			}
			if err := rt.Shell.Run(ctx, stepID, Script("sysctl-apply"), nil, report); err != nil {
				return err
			}
		}
	}
	return rt.Shell.Run(ctx, stepID, Script("pg-start"), nil, report)
}

// hugePagesEnabled reports whether the tuning result asks for huge pages.
func hugePagesEnabled(res tune.Result) bool {
	for _, s := range res.Settings {
		if s.Name == "huge_pages" {
			return s.Value != "off"
		}
	}
	return false
}

// restoreFiles puts the previous contents back after a validation failure. It
// writes with the file's own owner, so a failed run never leaves a file the
// server cannot read.
func restoreFiles(files []managedFile, backups map[string]string) {
	for _, f := range files {
		previous, ok := backups[f.Path]
		if !ok {
			_ = os.Remove(f.Path)
			continue
		}
		_ = writeFileOwned(f.Path, previous, f.Mode, f.Owner, f.Group)
	}
}
