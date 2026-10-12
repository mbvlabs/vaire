package steps

import (
	"context"
	"errors"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/versions"
)

const postgresPackagesID = "postgres-packages-v1"

// pgdgScript installs the PGDG apt repository; postgresql-common provides it.
const pgdgScript = "/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh"

// pgPackages are step 8's packages. pgbackrest comes from PGDG too and is
// installed on every node, whether or not backups are configured yet.
var pgPackages = []string{"postgresql-18", "postgresql-client-18", "postgresql-contrib", "pgbouncer", "pgbackrest"}

// pgHold are the packages pinned with apt-mark hold: the major version and
// pgBackRest, which moves only with eru pg update.
var pgHold = []string{"postgresql-18", "postgresql-client-18", "pgbackrest"}

// pgbackrestTooOld is the script's exit status when PGDG offers a pgBackRest
// older than versions.PgBackRestMinVersion.
const pgbackrestTooOld = 3

type postgresPackages struct{}

func (postgresPackages) ID() string { return postgresPackagesID }

func (postgresPackages) Describe(config.Cluster) string {
	return "install PostgreSQL 18, contrib, PgBouncer and pgBackRest from PGDG"
}

func (postgresPackages) RenderedFiles(config.Cluster, engine.Runtime) []engine.RenderedFile {
	return []engine.RenderedFile{{Path: CreateClusterConfPath, Content: render.CreateClusterConf()}}
}

func (postgresPackages) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return canonical(map[string]any{
		"packages":       pgPackages,
		"hold":           pgHold,
		"pgbackrest_min": versions.PgBackRestMinVersion,
		"createcluster":  string(render.CreateClusterConf()),
	})
}

func (postgresPackages) Check(ctx context.Context, _ config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	var missing []string
	for _, pkg := range pgPackages {
		out, _ := Probe(ctx, "dpkg-query", "-W", "-f=${Status}", pkg)
		if out != "install ok installed" {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		return incomplete("missing " + strings.Join(missing, ", ")), nil
	}
	held, _ := Probe(ctx, "apt-mark", "showhold")
	for _, pkg := range pgHold {
		if !strings.Contains(held, pkg) {
			return incomplete(pkg + " is not held"), nil
		}
	}
	return complete(), nil
}

func (s postgresPackages) Apply(ctx context.Context, _ config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if err := writeFile(CreateClusterConfPath, string(render.CreateClusterConf()), 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	err := rt.Shell.Run(ctx, s.ID(), Script("postgres-packages"), map[string]string{
		"PACKAGES":       strings.Join(pgPackages, " "),
		"HOLD":           strings.Join(pgHold, " "),
		"PGDG_SCRIPT":    pgdgScript,
		"PGBACKREST_MIN": versions.PgBackRestMinVersion,
	}, report)
	var scriptErr *engine.ScriptError
	if errors.As(err, &scriptErr) && scriptErr.Status == pgbackrestTooOld {
		return engine.ApplyResult{}, output.NewError(
			output.CodePrecondition,
			"PGDG offers a pgBackRest older than "+versions.PgBackRestMinVersion,
			output.ExitPrecondition,
			"eru needs pgBackRest "+versions.PgBackRestMinVersion+" or newer, which fixes a bug in encrypted repositories. Wait for PGDG to publish it, then re-run apply.",
		).WithDetails(map[string]any{"reason": "pgbackrest_too_old", "min_version": versions.PgBackRestMinVersion, "output_tail": scriptErr.Tail})
	}
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
