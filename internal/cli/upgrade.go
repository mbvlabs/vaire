package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/mbvlabs/eru/internal/release"
	"github.com/mbvlabs/eru/internal/upgrade"
	"github.com/spf13/cobra"
)

type upgradeData struct {
	From         upgrade.Version `json:"from"`
	To           upgrade.Version `json:"to"`
	Path         string          `json:"path"`
	PreviousPath string          `json:"previous_path,omitempty"`
	Changed      bool            `json:"changed"`
	DryRun       bool            `json:"dry_run"`
	Escalated    bool            `json:"escalated"`
	Nodes        string          `json:"nodes,omitempty"`
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	version, err := cmd.Flags().GetString("version")
	if err != nil {
		return err
	}
	channelFlag, err := cmd.Flags().GetString("channel")
	if err != nil {
		return err
	}
	skipNodes, err := cmd.Flags().GetBool("skip-nodes")
	if err != nil {
		return err
	}
	dry := dryRun(cmd)
	yes := flagTrue(cmd, "yes")

	target, err := upgrade.ResolveSelf()
	if err != nil {
		return err
	}
	channel, err := upgrade.SelectChannel(channelFlag)
	if err != nil {
		return err
	}

	client := &release.Client{}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	stop := waitOn(cmd, "checking for an eru release on "+channel)
	manifest, err := client.Resolve(ctx, channel, version)
	stop()
	if err != nil {
		return err
	}
	from := upgrade.Version{Version: buildinfo.Version, Channel: buildinfo.Channel}
	plan := upgrade.Decide(from, manifest, strings.TrimSpace(version) != "")
	plan.Target = target

	data := upgradeData{
		From:      plan.From,
		To:        plan.To,
		Path:      target,
		Changed:   plan.Changed,
		DryRun:    dry,
		Escalated: flagTrue(cmd, "escalated"),
	}
	warnings := append([]output.Warning(nil), plan.Warnings...)

	if !plan.Changed {
		return output.Success(cmd, output.Result{
			Data:     data,
			Warnings: warnings,
			Message:  "already on " + plan.From.Version,
			Next:     []string{"eru version"},
		})
	}
	if dry {
		if !skipNodes {
			warnings = append(warnings, nodesWarning())
			data.Nodes = "skipped"
		} else {
			data.Nodes = "skipped"
		}
		return output.Success(cmd, output.Result{
			Data:     data,
			Warnings: warnings,
			Message:  fmt.Sprintf("Would upgrade eru %s → %s (%s) at %s", plan.From.Version, plan.To.Version, plan.To.Channel, target),
			Next:     []string{"eru version"},
		})
	}

	if !yes && !MustNotPrompt(cmd) {
		ok, err := PromptYes(cmd, fmt.Sprintf("Upgrade eru %s → %s (%s) at %s?", plan.From.Version, plan.To.Version, plan.To.Channel, target))
		if err != nil {
			return err
		}
		if !ok {
			return output.NewError(
				output.CodePrecondition,
				"upgrade was not confirmed",
				output.ExitPrecondition,
				"Re-run with --yes to skip the prompt.",
			)
		}
	}

	if !dirWritable(filepath.Dir(target)) {
		handled, err := localenv.Escalate(cmd, "replace "+target, "--version", manifest.Version, "--channel", manifest.Channel, "--yes")
		if handled {
			return err
		}
		if err != nil {
			return err
		}
	}

	stop = waitOn(cmd, "downloading eru "+manifest.Version)
	cached, err := client.Fetch(ctx, manifest, runtime.GOOS, runtime.GOARCH)
	stop()
	if err != nil {
		return err
	}
	stop = waitOn(cmd, "verifying eru "+manifest.Version)
	err = upgrade.Preflight(ctx, cached, manifest)
	stop()
	if err != nil {
		return err
	}
	stop = waitOn(cmd, "replacing "+target)
	previous, swapWarnings, err := upgrade.Swap(target, cached)
	stop()
	if err != nil {
		return err
	}
	warnings = append(warnings, swapWarnings...)
	// Idle tunnel daemons still run the old binary; busy ones are replaced
	// by the next command once they go idle.
	stopIdleTunnelDaemons()
	data.PreviousPath = previous
	data.Changed = true
	if !skipNodes {
		warnings = append(warnings, nodesWarning())
		data.Nodes = "skipped"
	} else {
		data.Nodes = "skipped"
	}

	lines := []string{
		fmt.Sprintf("eru upgraded %s → %s", plan.From.Version, plan.To.Version),
	}
	if previous != "" {
		lines = insert(lines, 0, "kept previous binary at "+previous)
	}
	return output.Success(cmd, output.Result{
		Data:     data,
		Warnings: warnings,
		Message:  strings.Join(lines, "\n"),
		Next:     []string{"eru version"},
	})
}

func nodesWarning() output.Warning {
	return output.Warning{
		Code:    "nodes_not_upgraded",
		Message: "nodes are not upgraded: run eru node upgrade, or eru apply --auto-upgrade-nodes",
	}
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".eru-writetest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func flagTrue(cmd *cobra.Command, name string) bool {
	if cmd == nil || cmd.Flags() == nil {
		return false
	}
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		return false
	}
	switch flag.Value.String() {
	case "true", "TRUE", "1":
		return true
	default:
		return false
	}
}

func insert(lines []string, i int, line string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:i]...)
	out = append(out, line)
	out = append(out, lines[i:]...)
	return out
}
