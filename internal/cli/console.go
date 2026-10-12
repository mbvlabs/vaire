package cli

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/spf13/cobra"
)

// runConsole opens usql on a database through the tunnel. eru terminates TLS
// itself: usql talks plaintext to a loopback listener, and eru makes the
// verified TLS connection to Postgres over WireGuard.
func runConsole(cmd *cobra.Command, args []string) error {
	db := stringFlag(cmd, "db")
	if db == "" && len(args) > 0 {
		db = args[0]
	}
	role := stringFlag(cmd, "role")
	if role == "" {
		role = "ro"
	}
	if db == "" {
		return output.NewError(output.CodeInputRequired, "eru console needs a database", output.ExitInput,
			"Pass the database name, for example eru console app.").WithDetails(map[string]any{"flag": "--db"})
	}
	if MustNotPrompt(cmd) {
		return output.NewError(output.CodeUsage, "eru console is interactive and needs a terminal", output.ExitUsage,
			"Use eru sql for one statement.")
	}
	usql, err := localenv.UsqlPath(cmd)
	if err != nil {
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
	ctx, stop := signal.NotifyContext(commandContext(cmd), syscall.SIGTERM)
	defer stop()
	tlsCfg := pg.TLSConfig(pg.Params{ServerName: s.ctx.Domain, TLSMode: s.ctx.TLSMode})
	go pg.ServeTLSProxy(ctx, ln, s.tunnel, net.JoinHostPort(nodeWGAddr(s), strconv.Itoa(pgPort)), tlsCfg)

	if s.ctx.TLSMode == "self-signed" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: the cluster uses a self-signed certificate; the connection is encrypted but not verified")
	}
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.User(user),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:     "/" + db,
		RawQuery: "sslmode=disable&application_name=eru-console",
	}
	// Ctrl-C belongs to usql (it cancels the running query), not to eru.
	signal.Ignore(syscall.SIGINT)
	defer signal.Reset(syscall.SIGINT)
	child := exec.CommandContext(ctx, usql, dsn.String()) //nolint:gosec // G204: usql from PATH or eru's pinned copy.
	child.Env = append(os.Environ(), "PGPASSWORD="+password)
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
