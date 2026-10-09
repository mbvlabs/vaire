package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/spf13/cobra"
)

// CommandMeta is the per-command contract for humans and agents.
type CommandMeta struct {
	Group            string   `json:"group,omitempty"`
	Summary          string   `json:"summary"`
	Args             string   `json:"args,omitempty"`
	WhenToUse        []string `json:"when_to_use,omitempty"`
	WhenNotToUse     []string `json:"when_not_to_use,omitempty"`
	Prerequisites    []string `json:"prerequisites,omitempty"`
	Next             []string `json:"next,omitempty"`
	Examples         []string `json:"examples,omitempty"`
	Mutating         bool     `json:"mutating"`
	Destructive      bool     `json:"destructive,omitempty"`
	MutatingFlags    []string `json:"mutating_flags,omitempty"`    // flags that make an otherwise read-only command mutate
	DestructiveFlags []string `json:"destructive_flags,omitempty"` // flags that make the command destructive; need ConfirmToken
	SupportsDryRun   bool     `json:"supports_dry_run,omitempty"`
	SkipDryRunCheck  bool     `json:"skip_dry_run_check,omitempty"`
	SkipDryRunReason string   `json:"skip_dry_run_reason,omitempty"`
	Prompts          bool     `json:"prompts,omitempty"`
	InteractiveFlags []string `json:"interactive_flags,omitempty"`
	Audience         string   `json:"audience"`
	Remote           bool     `json:"remote"`
	ConfirmToken     string   `json:"confirm_token,omitempty"`
	NeedsRestart     string   `json:"needs_restart"`
	ErrorCodes       []string `json:"error_codes,omitempty"`
	LinuxOnly        bool     `json:"-"`
	LinuxOnlyFlags   []string `json:"-"`
	Platforms        []string `json:"platforms,omitempty"`
}

type commandMetaCheckReport struct {
	OK       bool     `json:"ok"`
	Failures []string `json:"failures,omitempty"`
}

var commandMetaByCmd = map[*cobra.Command]CommandMeta{}

func init() {
	output.CommandNext = func(cmd *cobra.Command) []string {
		meta, ok := metaFor(cmd)
		if !ok {
			return nil
		}
		return append([]string(nil), meta.Next...)
	}
}

func registerMeta(cmd *cobra.Command, meta CommandMeta) {
	if cmd == nil {
		return
	}
	if meta.LinuxOnly {
		meta.Platforms = []string{"linux"}
	} else {
		meta.Platforms = nil
	}
	commandMetaByCmd[cmd] = meta
}

func metaFor(cmd *cobra.Command) (CommandMeta, bool) {
	if cmd == nil {
		return CommandMeta{}, false
	}
	meta, ok := commandMetaByCmd[cmd]
	return meta, ok
}

func finalizeMeta(root *cobra.Command) {
	for _, cmd := range walkCommands(root) {
		meta, ok := metaFor(cmd)
		if !ok {
			continue
		}
		meta.Group = groupFromCommand(cmd)
		if meta.Audience == "" {
			meta.Audience = defaultAudience(cmd)
		}
		if meta.NeedsRestart == "" {
			meta.NeedsRestart = "never"
		}
		registerMeta(cmd, meta)
		notes := strings.Join(meta.WhenToUse, " ")
		if notes == "" {
			notes = meta.Summary
		}
		group := meta.Group
		if group == "" {
			group = "root"
		}
		setAgentMetadata(cmd, group, notes)
	}
}

func groupFromCommand(cmd *cobra.Command) string {
	if cmd == nil || cmd.Parent() == nil {
		return ""
	}
	current := cmd
	for current.Parent() != nil && current.Parent().Parent() != nil {
		current = current.Parent()
	}
	return current.Name()
}

func defaultAudience(cmd *cobra.Command) string {
	if cmd == nil {
		return "operator"
	}
	if cmd.CommandPath() == "eru node upgrade" {
		return "operator"
	}
	for cur := cmd; cur != nil; cur = cur.Parent() {
		if cur.Name() == "node" && cur.Parent() != nil && cur.Parent().Parent() == nil {
			return "node"
		}
	}
	return "operator"
}

func walkCommands(root *cobra.Command) []*cobra.Command {
	out := make([]*cobra.Command, 0)
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Name() == "help" || cmd.Name() == "completion" {
			return
		}
		out = append(out, cmd)
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	return out
}

func commandHidden(cmd *cobra.Command) bool {
	// eru node upgrade is operator-audience under the hidden node parent.
	if meta, ok := metaFor(cmd); ok && meta.Audience == "operator" && cmd.Parent() != nil && cmd.Parent().Hidden {
		return false
	}
	for cur := cmd; cur != nil && cur.Parent() != nil; cur = cur.Parent() {
		if cur.Hidden {
			return true
		}
	}
	return false
}

func checkCommandMetadata(root *cobra.Command) commandMetaCheckReport {
	failures := make([]string, 0)
	for _, cmd := range walkCommands(root) {
		failures = append(failures, checkOneCommand(cmd)...)
	}
	sort.Strings(failures)
	return commandMetaCheckReport{OK: len(failures) == 0, Failures: failures}
}

func checkOneCommand(cmd *cobra.Command) []string {
	path := cmd.CommandPath()
	meta, ok := metaFor(cmd)
	if !ok {
		return []string{path + ": missing CommandMeta"}
	}
	failures := make([]string, 0)
	if strings.TrimSpace(meta.Summary) == "" {
		failures = append(failures, path+": missing summary")
	}
	if len(meta.WhenToUse) == 0 {
		failures = append(failures, path+": missing when_to_use")
	}
	if len(meta.Examples) == 0 {
		failures = append(failures, path+": missing examples")
	}
	mutating := meta.Mutating || len(meta.MutatingFlags) > 0
	if mutating && !meta.SupportsDryRun && !meta.SkipDryRunCheck {
		failures = append(failures, path+": mutating command must support dry-run or skip the check")
	}
	if meta.SkipDryRunCheck && strings.TrimSpace(meta.SkipDryRunReason) == "" {
		failures = append(failures, path+": SkipDryRunCheck requires a reason")
	}
	if meta.Prompts && len(meta.InteractiveFlags) == 0 {
		failures = append(failures, path+": prompts without interactive_flags")
	}
	for _, name := range meta.InteractiveFlags {
		if !flagDefined(cmd, name) {
			failures = append(failures, path+": interactive flag "+name+" is not defined")
		}
	}
	if meta.Destructive && meta.ConfirmToken == "" {
		failures = append(failures, path+": destructive command requires confirm_token")
	}
	if meta.Destructive && len(meta.DestructiveFlags) > 0 {
		failures = append(failures, path+": set destructive or destructive_flags, not both")
	}
	if len(meta.DestructiveFlags) > 0 && meta.ConfirmToken == "" {
		failures = append(failures, path+": destructive_flags requires confirm_token")
	}
	for _, name := range meta.MutatingFlags {
		if !flagDefined(cmd, name) {
			failures = append(failures, path+": mutating flag "+name+" is not defined")
		}
	}
	for _, name := range meta.DestructiveFlags {
		if !flagDefined(cmd, name) {
			failures = append(failures, path+": destructive flag "+name+" is not defined")
		}
		if !meta.Mutating && !containsFlag(meta.MutatingFlags, name) {
			failures = append(failures, path+": destructive flag "+name+" must also be mutating")
		}
	}
	for _, name := range meta.LinuxOnlyFlags {
		if !flagDefined(cmd, name) {
			failures = append(failures, path+": linux-only flag "+name+" is not defined")
		}
	}
	if meta.Audience == "node" && !meta.LinuxOnly {
		failures = append(failures, path+": node audience requires linuxOnly")
	}
	if meta.ConfirmToken != "" && cmd.Flags().Lookup("confirm") == nil {
		failures = append(failures, path+": confirm_token set without --confirm flag")
	}
	if meta.SupportsDryRun && cmd.Flags().Lookup("dry-run") == nil {
		failures = append(failures, path+": supports_dry_run set without --dry-run flag")
	}
	switch meta.Audience {
	case "operator", "node":
	default:
		failures = append(failures, path+": audience must be operator or node")
	}
	switch meta.NeedsRestart {
	case "never", "maybe", "always":
	default:
		failures = append(failures, path+": needs_restart must be never, maybe, or always")
	}
	if meta.Group != groupFromCommand(cmd) {
		failures = append(failures, fmt.Sprintf("%s: group %q does not match cobra parent %q", path, meta.Group, groupFromCommand(cmd)))
	}
	if meta.Audience == "node" && !strings.HasPrefix(path, "eru node") {
		failures = append(failures, path+": node audience is only valid under eru node")
	}
	if strings.HasPrefix(path, "eru node") && path != "eru node upgrade" && meta.Audience != "node" {
		failures = append(failures, path+": command under eru node must have node audience")
	}
	for _, code := range meta.ErrorCodes {
		if !output.IsKnownCode(code) {
			failures = append(failures, path+": unknown error code "+code)
		}
	}
	return failures
}

func containsFlag(names []string, name string) bool {
	want := strings.TrimLeft(name, "-")
	for _, candidate := range names {
		if strings.TrimLeft(candidate, "-") == want {
			return true
		}
	}
	return false
}

func flagDefined(cmd *cobra.Command, name string) bool {
	name = strings.TrimLeft(name, "-")
	if cmd.Flags() != nil && cmd.Flags().Lookup(name) != nil {
		return true
	}
	if cmd.InheritedFlags() != nil && cmd.InheritedFlags().Lookup(name) != nil {
		return true
	}
	return false
}

func runCommandsCheck(cmd *cobra.Command, root *cobra.Command) error {
	report := checkCommandMetadata(root)
	if report.OK {
		return output.OK(cmd, report, "Command metadata is complete")
	}
	return output.NewError(
		output.CodeUsage,
		"command metadata check failed",
		output.ExitUsage,
		strings.Join(report.Failures, "\n"),
	).WithDetails(report)
}
