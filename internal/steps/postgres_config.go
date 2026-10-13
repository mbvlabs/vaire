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
	"github.com/mbvlabs/eru/internal/pg"
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
}

// confFiles returns spec's conf.d files in load order, then its pg_hba and
// pg_ident. 90-overrides.conf is written only when missing.
func confFiles(cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime) []managedFile {
	res := tuningFor(cfg, spec, rt.Host)
	dir, confD := PGConfDir(spec.Name), PGConfD(spec.Name)
	conf := func(path string, content []byte, mode os.FileMode) managedFile {
		return managedFile{Path: path, Content: string(content), Mode: mode, Owner: pgFileOwner, Group: pgFileGroup}
	}
	return []managedFile{
		conf(confD+"/10-eru-tuning.conf", render.TuningConf(res), 0o644),
		conf(confD+"/20-eru-security.conf", render.SecurityConf(spec.Port), 0o644),
		conf(confD+"/30-eru-extensions.conf", render.ExtensionsConf(spec, res), 0o644),
		conf(confD+"/40-eru-backup.conf", render.BackupConf(cfg, spec), 0o644),
		conf(confD+"/50-eru-settings.conf", render.SettingsConf(cfg, spec), 0o644),
		conf(dir+"/pg_hba.conf", render.HBA(), 0o640),
		conf(dir+"/pg_ident.conf", render.Ident(cfg), 0o640),
	}
}

// postgresConfig renders one cluster's configuration from its tuning
// (RFC 7.3) and classifies the change as a reload or a restart of that
// cluster only.
type postgresConfig struct{ clusterStep }

func (s postgresConfig) ID() string { return s.id(postgresConfigID) }

func (postgresConfig) Instance(cluster string) engine.Step {
	return postgresConfig{clusterStep{cluster: cluster}}
}

func (s postgresConfig) Describe(config.Cluster) string {
	return "render " + s.cluster + "'s conf.d, pg_hba and pg_ident, then reload or restart it"
}

func (s postgresConfig) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil
	}
	files := confFiles(cfg, spec, rt)
	out := make([]engine.RenderedFile, 0, len(files))
	for _, f := range files {
		out = append(out, engine.RenderedFile{Path: f.Path, Content: []byte(f.Content)})
	}
	return out
}

func (s postgresConfig) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil, err
	}
	files := confFiles(cfg, spec, rt)
	payload := make([]map[string]string, 0, len(files))
	for _, f := range files {
		payload = append(payload, map[string]string{"path": f.Path, "content": f.Content})
	}
	return canonical(map[string]any{"files": payload, "overrides": string(render.OverridesHeader())})
}

func (s postgresConfig) Check(_ context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	for _, f := range confFiles(cfg, spec, rt) {
		if !fileIsOwned(f.Path, f.Content, f.Mode, f.Owner, f.Group) {
			return incomplete(f.Path + " differs"), nil
		}
	}
	if _, err := os.Stat(PGConfD(spec.Name) + "/90-overrides.conf"); err != nil {
		return incomplete("90-overrides.conf is missing"), nil
	}
	return complete(), nil
}

func (s postgresConfig) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	local := ClusterPG(spec)
	files := confFiles(cfg, spec, rt)
	// Keep the previous contents so a validation failure can be rolled back.
	backups := make(map[string]string, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f.Path) //nolint:gosec // G304: managed file paths from the catalogue.
		if err == nil {
			backups[f.Path] = string(raw)
		}
	}
	// None of the managed files exist yet: this is the cluster's first
	// configuration. The journal cannot tell, because the runner marks the
	// step running before Apply.
	first := len(backups) == 0
	for _, f := range files {
		if err := writeFileOwned(f.Path, f.Content, f.Mode, f.Owner, f.Group); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("writing the rendered configuration: %w", err)
		}
	}
	overrides := PGConfD(spec.Name) + "/90-overrides.conf"
	if _, err := os.Stat(overrides); err != nil {
		if err := writeFileOwned(overrides, string(render.OverridesHeader()), 0o644, pgFileOwner, pgFileGroup); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("writing the rendered configuration: %w", err)
		}
	}
	if err := ensureIncludeDir(spec.Name); err != nil {
		return engine.ApplyResult{}, fmt.Errorf("writing the rendered configuration: %w", err)
	}
	if err := validateConfig(ctx, cfg, spec); err != nil {
		restoreFiles(files, backups)
		return engine.ApplyResult{}, err
	}
	if err := rt.Shell.Run(ctx, s.ID(), Script("postgres-config"), map[string]string{"CLUSTER": spec.Name}, report); err != nil {
		return engine.ApplyResult{}, err
	}

	if first {
		if err := restartPostgres(ctx, cfg, spec, rt, s.ID(), report); err != nil {
			return engine.ApplyResult{}, err
		}
		return engine.ApplyResult{Restart: engine.RestartNone, Notes: []string{"restarted after the first configuration"}}, nil
	}
	pending, err := pendingRestartSettings(ctx, local)
	if err != nil {
		return engine.ApplyResult{Restart: engine.RestartReload}, nil
	}
	if len(pending) > 0 {
		return engine.ApplyResult{Restart: engine.RestartRestart, Notes: []string{"restart of " + spec.Name + " needed for " + strings.Join(pending, ", ")}}, nil
	}
	return engine.ApplyResult{Restart: engine.RestartReload}, nil
}

// ensureIncludeDir appends include_dir = 'conf.d' to the cluster's
// postgresql.conf when the file does not already include it. Debian's own
// file is otherwise kept.
func ensureIncludeDir(cluster string) error {
	path := PGConfDir(cluster) + "/postgresql.conf"
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
func validateConfig(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) error {
	local := ClusterPG(spec)
	settings, err := local.Query(ctx, "postgres", `SELECT f.sourcefile, f.sourceline, f.name, f.error
FROM pg_file_settings f LEFT JOIN pg_settings s ON s.name = f.name
WHERE f.error IS NOT NULL AND NOT (f.error = 'setting could not be applied' AND s.context = 'postmaster')
ORDER BY f.seqno`, nil)
	if err != nil {
		return fmt.Errorf("validating the rendered configuration: %w", err)
	}
	hba, err := local.Query(ctx, "postgres", "SELECT file_name, line_number, error FROM pg_hba_file_rules WHERE error IS NOT NULL ORDER BY rule_number", nil)
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
			"-D", cfg.Server.DataDirectory(spec.Name), "-c", "config_file="+PGConfDir(spec.Name)+"/postgresql.conf", "-C", "max_connections"); err != nil {
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

// pendingRestartSettings lists the cluster's settings that need a restart.
func pendingRestartSettings(ctx context.Context, local pg.Local) ([]string, error) {
	rows, err := local.Query(ctx, "postgres", "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name", nil)
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

// restartPostgres stops one cluster, sizes huge pages while it is stopped,
// and starts it again. The pool is the cluster's measured need plus 5%, and
// the other clusters' estimates (RFC 7.3).
func restartPostgres(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime, stepID string, report engine.Reporter) error {
	env := map[string]string{"CLUSTER": spec.Name}
	if err := rt.Shell.Run(ctx, stepID, Script("pg-stop"), env, report); err != nil {
		return err
	}
	if hugePagesEnabled(tuningFor(cfg, spec, rt.Host)) {
		hp, err := hugePagesFromKernel(ctx, spec.Name, cfg.Server.DataDirectory(spec.Name))
		if err == nil && hp > 0 {
			hp = hp + hp/20
			for _, other := range cfg.Clusters {
				if other.Name != spec.Name {
					hp += tuningFor(cfg, other, rt.Host).HugePages
				}
			}
			if err := writeFile(SysctlPath, string(render.SysctlConf(rt.Host, hp)), 0o644); err != nil {
				return err
			}
			if err := rt.Shell.Run(ctx, stepID, Script("sysctl-apply"), nil, report); err != nil {
				return err
			}
		}
	}
	return rt.Shell.Run(ctx, stepID, Script("pg-start"), env, report)
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
