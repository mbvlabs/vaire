package cli

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/spf13/cobra"
)

// psqlArgs accepts positional arguments only after --; they go to psql.
func psqlArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
		return output.NewError(output.CodeUsage, "eru psql takes psql's arguments only after --", output.ExitUsage,
			`Put them after --: eru psql --db <db> -- -c "select 1".`)
	}
	return nil
}

func runPsql(cmd *cobra.Command, args []string) error {
	db := stringFlag(cmd, "db")
	if db == "" {
		return output.NewError(output.CodeInputRequired, "eru psql needs --db", output.ExitInput,
			"Pass --db with one of the server's databases.").WithDetails(map[string]any{"flag": "--db"})
	}
	if err := localenv.RequireTool(cmd, localenv.ToolPsql); err != nil {
		return err
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	user, password, err := adminCredentials(s.ctx)
	if err != nil {
		return err
	}
	ln, port, err := listenLocal(defaultPostgresPort)
	if err != nil {
		return output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "No free local port.")
	}
	defer func() { _ = ln.Close() }()
	ctx, stop := signal.NotifyContext(commandContext(cmd), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	at := databaseEndpoint(cmd, s, p, db)
	go proxy(ctx, ln, s.tunnel, at.addr(pgPort))

	psqlPath, err := exec.LookPath("psql")
	if err != nil {
		return output.NewError(output.CodeMissingTool, "psql is not on PATH", output.ExitMissingTool, "Install postgresql-client.")
	}
	sslMode := "verify-full"
	env := []string{
		"PGHOST=" + at.Hostname,
		"PGHOSTADDR=127.0.0.1",
		"PGPORT=" + strconv.Itoa(port),
		"PGUSER=" + user,
		"PGDATABASE=" + db,
		"PGPASSWORD=" + password,
		"PGSSLMODE=" + sslMode,
		"PGSSLROOTCERT=system",
		"PGSSLSNI=1",
	}
	if s.ctx.TLSMode == "self-signed" {
		env[6] = "PGSSLMODE=require"
		env = env[:7]
	}
	env = append(env, "PGOPTIONS="+sessionOptions(db, flagTrue(cmd, "write")))
	// Arguments after -- go to psql as they are; the connection comes from
	// the PG* environment.
	child := exec.CommandContext(ctx, psqlPath, args...) //nolint:gosec // G204: fixed psql path; args are the operator's own.
	child.Env = append(os.Environ(), env...)
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	// psql owns the terminal from here: nothing of eru's live area may draw.
	defer ui.Suspend()()
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &localenv.SilentExit{Code: exitErr.ExitCode()}
		}
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	return nil
}
