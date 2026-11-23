package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/localenv"
	"github.com/mbvlabs/vaire/internal/release"
	"github.com/mbvlabs/vaire/internal/upgrade"
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

// upgradeOptions are the flags of one vaire upgrade run.
type upgradeOptions struct {
	version   string
	channel   string
	skipNodes bool
	dry       bool
	yes       bool
}

// upgradeFlags reads the command's flags.
func upgradeFlags(cmd *cobra.Command) (upgradeOptions, error) {
	var opts upgradeOptions

	version, err := cmd.Flags().GetString("version")
	if err != nil {
		return opts, err
	}

	channel, err := cmd.Flags().GetString("channel")
	if err != nil {
		return opts, err
	}

	skipNodes, err := cmd.Flags().GetBool("skip-nodes")
	if err != nil {
		return opts, err
	}

	opts.version = version
	opts.channel = channel
	opts.skipNodes = skipNodes
	opts.dry = dryRun(cmd)
	opts.yes = flagTrue(cmd, "yes")

	return opts, nil
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	opts, err := upgradeFlags(cmd)
	if err != nil {
		return err
	}

	manifest, plan, data, warnings, err := planUpgrade(cmd, opts)
	if err != nil {
		return err
	}

	return applyUpgrade(cmd, opts, manifest, plan, data, warnings)
}

// planUpgrade resolves the release and decides what vaire upgrade changes.
func planUpgrade(cmd *cobra.Command, opts upgradeOptions) (release.Manifest, upgrade.Plan, upgradeData, []output.Warning, error) {
	target, err := upgrade.ResolveSelf()
	if err != nil {
		return release.Manifest{}, upgrade.Plan{}, upgradeData{}, nil, err
	}

	channel, err := upgrade.SelectVersionChannel(opts.channel, opts.version)
	if err != nil {
		return release.Manifest{}, upgrade.Plan{}, upgradeData{}, nil, err
	}

	client := &release.Client{}
	stop := waitOn(cmd, "checking for an vaire release on "+channel)
	manifest, err := client.Resolve(commandContext(cmd), channel, opts.version)

	stop()

	if err != nil {
		return release.Manifest{}, upgrade.Plan{}, upgradeData{}, nil, err
	}

	from := upgrade.Version{Version: buildinfo.Version, Channel: buildinfo.Channel}
	plan := upgrade.Decide(from, manifest, strings.TrimSpace(opts.version) != "")
	plan.Target = target

	data := upgradeData{
		From:      plan.From,
		To:        plan.To,
		Path:      target,
		Changed:   plan.Changed,
		DryRun:    opts.dry,
		Escalated: flagTrue(cmd, "escalated"),
	}

	return manifest, plan, data, append([]output.Warning(nil), plan.Warnings...), nil
}

// applyUpgrade installs the planned release and renders the result. A child
// re-executed under sudo renders its own result, so when one ran the parent
// stops with its error and prints nothing.
func applyUpgrade(cmd *cobra.Command, opts upgradeOptions, manifest release.Manifest, plan upgrade.Plan, data upgradeData, warnings []output.Warning) error {
	next := []string{"vaire version"}
	if !opts.skipNodes && (manifest.Channel == release.ChannelRC || manifest.Channel == release.ChannelStable) {
		next = append(next, "vaire node upgrade")
	}

	if !plan.Changed {
		return output.Success(cmd, output.Result{
			Data:     data,
			Warnings: warnings,
			Message:  "already on " + plan.From.Version,
			Next:     next,
		})
	}

	if opts.dry {
		return upgradeDryRun(cmd, data, warnings, plan, opts.skipNodes)
	}

	if err := confirmUpgrade(cmd, plan, opts); err != nil {
		return err
	}

	if handled, err := escalateUpgrade(cmd, manifest, plan.Target); handled || err != nil {
		return err
	}

	previous, swapWarnings, err := installUpgrade(cmd, manifest, plan.Target)
	if err != nil {
		return err
	}

	warnings = append(warnings, swapWarnings...)
	data.PreviousPath = previous
	data.Changed = true
	data.Nodes = "skipped"

	if !opts.skipNodes {
		warnings = append(warnings, nodesWarning())
	}

	lines := []string{
		fmt.Sprintf("vaire upgraded %s → %s", plan.From.Version, plan.To.Version),
	}
	if previous != "" {
		lines = insert(lines, 0, "kept previous binary at "+previous)
	}

	return output.Success(cmd, output.Result{
		Data:     data,
		Warnings: warnings,
		Message:  strings.Join(lines, "\n"),
		Next:     next,
	})
}

// upgradeDryRun reports what would change without touching the binary.
func upgradeDryRun(cmd *cobra.Command, data upgradeData, warnings []output.Warning, plan upgrade.Plan, skipNodes bool) error {
	data.Nodes = "skipped"

	if !skipNodes {
		warnings = append(warnings, nodesWarning())
	}

	return output.Success(cmd, output.Result{
		Data:     data,
		Warnings: warnings,
		Message:  fmt.Sprintf("Would upgrade vaire %s → %s (%s) at %s", plan.From.Version, plan.To.Version, plan.To.Channel, plan.Target),
		Next:     []string{"vaire version"},
	})
}

// confirmUpgrade asks before the binary is replaced, unless --yes or a run
// that must not prompt.
func confirmUpgrade(cmd *cobra.Command, plan upgrade.Plan, opts upgradeOptions) error {
	if opts.yes || MustNotPrompt(cmd) {
		return nil
	}

	ok, err := PromptYes(cmd, fmt.Sprintf("Upgrade vaire %s → %s (%s) at %s?", plan.From.Version, plan.To.Version, plan.To.Channel, plan.Target))
	if err != nil {
		return err
	}

	if ok {
		return nil
	}

	return output.NewError(
		output.CodePrecondition,
		"upgrade was not confirmed",
		output.ExitPrecondition,
		"Re-run with --yes to skip the prompt.",
	)
}

// escalateUpgrade re-executes the upgrade under sudo when the binary's
// directory is not writable. handled is true when a child ran: the parent
// must exit with err and render nothing, because the child did.
func escalateUpgrade(cmd *cobra.Command, manifest release.Manifest, target string) (bool, error) {
	if dirWritable(filepath.Dir(target)) {
		return false, nil
	}

	return localenv.Escalate(cmd, "replace "+target, "--version", manifest.Version, "--channel", manifest.Channel, "--yes")
}

// installUpgrade downloads, verifies and swaps in the new binary, returning
// the previous binary's path.
func installUpgrade(cmd *cobra.Command, manifest release.Manifest, target string) (string, []output.Warning, error) {
	ctx := commandContext(cmd)
	client := &release.Client{}
	stop := waitOn(cmd, "downloading vaire "+manifest.Version)
	cached, err := client.Fetch(ctx, manifest, runtime.GOOS, runtime.GOARCH)

	stop()

	if err != nil {
		return "", nil, err
	}

	stop = waitOn(cmd, "verifying vaire "+manifest.Version)
	err = upgrade.Preflight(ctx, cached, manifest)

	stop()

	if err != nil {
		return "", nil, err
	}

	stop = waitOn(cmd, "replacing "+target)
	previous, swapWarnings, err := upgrade.Swap(target, cached)

	stop()

	if err != nil {
		return "", nil, err
	}
	// Idle tunnel daemons still run the old binary; busy ones are replaced
	// by the next command once they go idle.
	stopIdleTunnelDaemons()

	return previous, swapWarnings, nil
}

func nodesWarning() output.Warning {
	return output.Warning{
		Code:    "nodes_not_upgraded",
		Message: "this command upgrades only the CLI; run vaire node upgrade for a supported node",
	}
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".vaire-writetest-*")
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
