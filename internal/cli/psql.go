package cli

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/spf13/cobra"
)

func runPsql(cmd *cobra.Command, _ []string) error {
	db := stringFlag(cmd, "db")
	role := stringFlag(cmd, "role")
	if role == "" {
		role = "ro"
	}
	if db == "" {
		return output.NewError(output.CodeInputRequired, "eru psql needs --db", output.ExitInput,
			"Pass --db with one of the cluster's databases.").WithDetails(map[string]any{"flag": "--db"})
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
	user, password, err := dbCredentials(s.ctx, db, role)
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
	go proxy(ctx, ln, s.tunnel, net.JoinHostPort(nodeWGAddr(s), strconv.Itoa(pgPort)))

	psqlPath, err := exec.LookPath("psql")
	if err != nil {
		return output.NewError(output.CodeMissingTool, "psql is not on PATH", output.ExitMissingTool, "Install postgresql-client.")
	}
	sslMode := "verify-full"
	env := []string{
		"PGHOST=" + s.ctx.Domain,
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
	child := exec.CommandContext(ctx, psqlPath) //nolint:gosec // G204: fixed psql path.
	child.Env = append(os.Environ(), env...)
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &localenv.SilentExit{Code: exitErr.ExitCode()}
		}
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	return nil
}
