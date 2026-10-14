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
	"strings"
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
	user, password, err := adminCredentials(s.ctx)
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
	at := databaseEndpoint(cmd, s, p, db)
	tlsCfg := pg.TLSConfig(pg.Params{ServerName: at.Hostname, TLSMode: s.ctx.TLSMode})
	go pg.ServeTLSProxy(ctx, ln, s.tunnel, at.addr(pgPort), tlsCfg)

	if s.ctx.TLSMode == "self-signed" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: the cluster uses a self-signed certificate; the connection is encrypted but not verified")
	}
	query := url.Values{
		"sslmode":          {"disable"},
		"application_name": {"eru-console"},
		"options":          {sessionOptions(db, flagTrue(cmd, "write"))},
	}
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.User(user),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:     "/" + db,
		RawQuery: query.Encode(),
	}
	// Ctrl-C belongs to usql (it cancels the running query), not to eru.
	signal.Ignore(syscall.SIGINT)
	defer signal.Reset(syscall.SIGINT)
	// usql's default prompt shows the host up to its first dot, which for the
	// loopback listener is "127"; name the node instead.
	child := exec.CommandContext(ctx, usql, "--set", consolePrompt(at.Hostname, s.ctx.Name), dsn.String()) //nolint:gosec // G204: usql from PATH or eru's pinned copy.
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

// consolePrompt is usql's PROMPT1 variable: its default (%S%N%m%/%R%# ) with
// the literal node name in place of %m, so pg:user@<node>/<db>=>, with the node's
// hostname cut at its first dot (db1.example.com is db1), or the context name
// when the hostname is unknown. % is usql's escape, so it is dropped from the
// literal.
func consolePrompt(hostname, contextName string) string {
	node, _, _ := strings.Cut(hostname, ".")
	if node == "" {
		node = contextName
	}
	node = strings.ReplaceAll(node, "%", "")
	return "PROMPT1=%S%N" + node + "/%/%R%# "
}
