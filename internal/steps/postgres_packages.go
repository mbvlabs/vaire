package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/versions"
)

const postgresPackagesID = "postgres-packages-v1"

// pgdgScript installs the PGDG apt repository; postgresql-common provides it.
const pgdgScript = "/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh"

// pgPackages are step 8's packages. pgbackrest comes from PGDG too and is
// installed on every node, whether or not backups are configured yet.
var pgPackages = []string{"postgresql-18", "postgresql-client-18", "postgresql-contrib", "pgbouncer", "pgbackrest"}

// pgHold are the packages pinned with apt-mark hold: the major version and
// pgBackRest, which moves only with vaire pg update.
var pgHold = []string{"postgresql-18", "postgresql-client-18", "pgbackrest"}

// pgbackrestTooOld is the script's exit status when PGDG offers a pgBackRest
// older than versions.PgBackRestMinVersion.
const pgbackrestTooOld = 3

// packageProbe is the read-only command seam for Check. Tests replace it to
// model installed and held package facts.
var packageProbe = Probe

type postgresPackages struct{}

func (postgresPackages) ID() string { return postgresPackagesID }

// Recheck keeps the real package/control/library facts current even when the
// input hash is unchanged, so a missing preinstalled file or a removed hold is
// repaired by the next apply. A healthy host only runs read-only probes.
func (postgresPackages) Recheck() bool { return true }

func (postgresPackages) Describe(config.Cluster) string {
	return "install PostgreSQL 18, contrib, PgBouncer, pgBackRest and resolved extension packages"
}

func (postgresPackages) RenderedFiles(config.Cluster, engine.Runtime) []engine.RenderedFile {
	return []engine.RenderedFile{{Path: CreateClusterConfPath, Content: render.CreateClusterConf()}}
}

func (postgresPackages) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	extensionPackages, err := ExtensionPackages(cfg)
	if err != nil {
		return nil, err
	}
	inputs := map[string]any{
		"packages":       pgPackages,
		"hold":           pgHold,
		"pgbackrest_min": versions.PgBackRestMinVersion,
		"createcluster":  string(render.CreateClusterConf()),
	}
	// Only when in use, so an unchanged host with no APT extension
	// requirements keeps its previous input hash and step result.
	if len(extensionPackages) > 0 {
		inputs["extension_packages"] = extensionPackages
	}
	return canonical(inputs)
}

func (postgresPackages) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	extensionPackages, err := ExtensionPackages(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	var missing []string
	for _, pkg := range append(append([]string(nil), pgPackages...), extensionPackages...) {
		out, _ := packageProbe(ctx, "dpkg-query", "-W", "-f=${Status}", pkg)
		if out != "install ok installed" {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		return incomplete("missing " + strings.Join(missing, ", ")), nil
	}
	held, _ := packageProbe(ctx, "apt-mark", "showhold")
	heldSet := map[string]bool{}
	for _, pkg := range strings.Fields(held) {
		heldSet[pkg] = true
	}
	for _, pkg := range append(append([]string(nil), pgHold...), extensionPackages...) {
		if !heldSet[pkg] {
			return incomplete(pkg + " is not held"), nil
		}
	}
	reqs, err := ExtensionPrerequisites(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if missingFiles := missingExtensionFiles(reqs); len(missingFiles) > 0 {
		return incomplete(strings.Join(missingFiles, "; ")), nil
	}
	return complete(), nil
}

func (s postgresPackages) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if err := writeFile(CreateClusterConfPath, string(render.CreateClusterConf()), 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	err := installPostgresPackages(ctx, cfg, s.ID(), rt, report)
	var scriptErr *engine.ScriptError
	if errors.As(err, &scriptErr) && scriptErr.Status == pgbackrestTooOld {
		return engine.ApplyResult{}, output.NewError(
			output.CodePrecondition,
			"PGDG offers a pgBackRest older than "+versions.PgBackRestMinVersion,
			output.ExitPrecondition,
			"vaire needs pgBackRest "+versions.PgBackRestMinVersion+" or newer, which fixes a bug in encrypted repositories. Wait for PGDG to publish it, then re-run apply.",
		).WithDetails(map[string]any{"reason": "pgbackrest_too_old", "min_version": versions.PgBackRestMinVersion, "output_tail": scriptErr.Tail})
	}
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

// postgresPackagesEnv is the package step's script environment. The base
// package list and its hold set are unchanged; the resolved APT extension
// union is installed and held separately.
func postgresPackagesEnv(extensionPackages []string) map[string]string {
	return map[string]string{
		"PACKAGES":           strings.Join(pgPackages, " "),
		"HOLD":               strings.Join(pgHold, " "),
		"EXTENSION_PACKAGES": strings.Join(extensionPackages, " "),
		"PGDG_SCRIPT":        pgdgScript,
		"PGBACKREST_MIN":     versions.PgBackRestMinVersion,
		"STOP_PGBOUNCER":     "1",
	}
}

// installPostgresPackages runs the package script and then verifies the real
// PG18 control/library files. It never uninstalls anything: a failure can
// leave packages installed, and the error says so rather than claiming
// nothing changed.
func installPostgresPackages(ctx context.Context, cfg config.Cluster, stepID string, rt engine.Runtime, report engine.Reporter) error {
	extensionPackages, err := ExtensionPackages(cfg)
	if err != nil {
		return err
	}
	if err := runExtensionPackageScript(ctx, stepID, postgresPackagesEnv(extensionPackages), rt, report); err != nil {
		var scriptErr *engine.ScriptError
		if errors.As(err, &scriptErr) && len(scriptErr.Tail) > 0 {
			return fmt.Errorf("installing PostgreSQL and extension packages: %w: %s; installed packages are not removed", err, strings.Join(scriptErr.Tail, " | "))
		}
		return fmt.Errorf("installing PostgreSQL and extension packages: %w; installed packages are not removed", err)
	}
	reqs, err := ExtensionPrerequisites(cfg)
	if err != nil {
		return fmt.Errorf("resolving extension prerequisites after package installation: %w; installed packages are not removed", err)
	}
	if err := VerifyExtensionRequirements(reqs); err != nil {
		return fmt.Errorf("extension prerequisites are not met after package installation: %w; installed packages are not removed", err)
	}
	return nil
}
