package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
	"github.com/mbvlabs/vaire/internal/localenv"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/spf13/cobra"
)

// psqlArgs accepts positional arguments only after --; they go to psql.
func psqlArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
		return output.NewError(output.CodeUsage, "vaire psql takes psql's arguments only after --", output.ExitUsage,
			`Put them after --: vaire psql --db <db> -- -c "select 1".`)
	}

	return nil
}

func runPsql(cmd *cobra.Command, args []string) error {
	role, err := sessionRole(cmd)
	if err != nil {
		return err
	}

	db := stringFlag(cmd, "db")
	if db == "" {
		return output.NewError(output.CodeInputRequired, "vaire psql needs --db", output.ExitInput,
			"Pass --db with one of the server's databases.").WithDetails(map[string]any{"flag": "--db"})
	}

	psqlPath, err := localenv.PsqlPath(cmd)
	if err != nil {
		return err
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	user, password, err := databaseSessionLogin(cmd, s, p, db, role, flagTrue(cmd, "write"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(commandContext(cmd), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, dir, cleanup, err := listenPsqlSocket(ctx)
	if err != nil {
		return output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Could not open a private PostgreSQL proxy socket.")
	}
	defer cleanup()

	at := databaseEndpoint(ctx, s, p, db)
	tlsCfg := pg.TLSConfig(pg.Params{ServerName: at.Hostname, TLSMode: s.ctx.TLSMode})

	go pg.ServeTLSProxy(ctx, ln, s.dialer, at.addr(pgPort), tlsCfg)
	// libpq ignores the TLS options for Unix sockets. Keep TCP verification
	// enabled for any explicit TCP endpoint psql is asked to connect to.
	env := append(os.Environ(),
		"PGHOST="+dir,
		"PGHOSTADDR=",
		"PGPORT="+strconv.Itoa(pgPort),
		"PGUSER="+user,
		"PGDATABASE="+db,
		"PGPASSWORD="+password,
		"PGSSLMODE=verify-full",
		"PGSSLROOTCERT=system",
		"PGSSLSNI=1",
		"PGOPTIONS="+loginOptions(db, role, flagTrue(cmd, "write")),
	)

	// Arguments after -- go to psql as they are; the connection comes from
	// the PG* environment.
	child := exec.CommandContext(ctx, psqlPath, args...)

	child.Env = env
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	// psql owns the terminal from here: nothing of vaire's live area may draw.
	defer ui.Suspend()()

	if err := child.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return &localenv.SilentExitError{Code: exitErr.ExitCode()}
		}

		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}

	return nil
}

func listenPsqlSocket(ctx context.Context) (net.Listener, string, func(), error) {
	dir, err := os.MkdirTemp("", "vaire-pg-")
	if err != nil {
		return nil, "", nil, err
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "unix", filepath.Join(dir, ".s.PGSQL."+strconv.Itoa(pgPort)))
	if err != nil {
		_ = os.RemoveAll(dir)

		return nil, "", nil, err
	}

	return ln, dir, func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}, nil
}
