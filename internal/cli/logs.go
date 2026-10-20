package cli

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

func runLogs(cmd *cobra.Command, args []string) error {
	unit := "postgres"
	if len(args) > 0 && args[0] != "" {
		unit = args[0]
	}
	since := stringFlag(cmd, "since")
	lines, err := cmd.Flags().GetInt("lines")
	if err != nil {
		return err
	}
	follow := flagTrue(cmd, "follow")

	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(commandContext(cmd), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	onEvent := func(ev output.Event) {
		if ev.Type == "log" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), ev.Line)
			return
		}
		p.event(ev)
	}
	params := map[string]any{"unit": unit, "since": since, "lines": lines, "follow": follow}
	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}
	reply, err := s.node.Call(ctx, "logs", params, remote.CallOptions{OnEvent: onEvent})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	var data map[string]any
	if err := reply.Decode(&data); err != nil {
		return err
	}
	return output.OK(cmd, data, "logs for "+unit)
}
