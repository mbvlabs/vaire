package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
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

	if cmd.CommandPath() == "vaire node upgrade" {
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
	// vaire node upgrade is operator-audience under the hidden node parent.
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
	commands := walkCommands(root)

	failures := make([]string, 0, len(commands))
	for _, cmd := range commands {
		failures = append(failures, checkOneCommand(cmd)...)
	}

	sort.Strings(failures)

	return commandMetaCheckReport{OK: len(failures) == 0, Failures: failures}
}

// commandMetaCheck collects the metadata failures of one command.
type commandMetaCheck struct {
	cmd      *cobra.Command
	meta     CommandMeta
	path     string
	failures []string
}

// fail records one failure, prefixed with the command's path.
func (c *commandMetaCheck) fail(msg string) {
	c.failures = append(c.failures, c.path+": "+msg)
}

func checkOneCommand(cmd *cobra.Command) []string {
	path := cmd.CommandPath()

	meta, ok := metaFor(cmd)
	if !ok {
		return []string{path + ": missing CommandMeta"}
	}

	c := &commandMetaCheck{cmd: cmd, meta: meta, path: path}
	c.checkDocs()
	c.checkDryRun()
	c.checkPrompts()
	c.checkDestructive()
	c.checkFlags()
	c.checkAudience()
	c.checkErrorCodes()

	return c.failures
}

// checkDocs checks the text every command documents.
func (c *commandMetaCheck) checkDocs() {
	if strings.TrimSpace(c.meta.Summary) == "" {
		c.fail("missing summary")
	}

	if len(c.meta.WhenToUse) == 0 {
		c.fail("missing when_to_use")
	}

	if len(c.meta.Examples) == 0 {
		c.fail("missing examples")
	}
}

// checkDryRun checks that a mutation can be previewed or explains why not.
func (c *commandMetaCheck) checkDryRun() {
	if (c.meta.Mutating || len(c.meta.MutatingFlags) > 0) && !c.meta.SupportsDryRun && !c.meta.SkipDryRunCheck {
		c.fail("mutating command must support dry-run or skip the check")
	}

	if c.meta.SkipDryRunCheck && strings.TrimSpace(c.meta.SkipDryRunReason) == "" {
		c.fail("SkipDryRunCheck requires a reason")
	}
}

// checkPrompts checks that the prompts a command asks are declared.
func (c *commandMetaCheck) checkPrompts() {
	if c.meta.Prompts && len(c.meta.InteractiveFlags) == 0 {
		c.fail("prompts without interactive_flags")
	}

	for _, name := range c.meta.InteractiveFlags {
		if !flagDefined(c.cmd, name) {
			c.fail("interactive flag " + name + " is not defined")
		}
	}
}

// checkDestructive checks the confirmation a destructive command needs.
func (c *commandMetaCheck) checkDestructive() {
	if c.meta.Destructive && c.meta.ConfirmToken == "" {
		c.fail("destructive command requires confirm_token")
	}

	if c.meta.Destructive && len(c.meta.DestructiveFlags) > 0 {
		c.fail("set destructive or destructive_flags, not both")
	}

	if len(c.meta.DestructiveFlags) > 0 && c.meta.ConfirmToken == "" {
		c.fail("destructive_flags requires confirm_token")
	}

	if c.meta.ConfirmToken != "" && c.cmd.Flags().Lookup("confirm") == nil {
		c.fail("confirm_token set without --confirm flag")
	}
}

// checkFlags checks every flag a command's metadata names.
func (c *commandMetaCheck) checkFlags() {
	for _, name := range c.meta.MutatingFlags {
		if !flagDefined(c.cmd, name) {
			c.fail("mutating flag " + name + " is not defined")
		}
	}

	for _, name := range c.meta.DestructiveFlags {
		if !flagDefined(c.cmd, name) {
			c.fail("destructive flag " + name + " is not defined")
		}

		if !c.meta.Mutating && !containsFlag(c.meta.MutatingFlags, name) {
			c.fail("destructive flag " + name + " must also be mutating")
		}
	}

	for _, name := range c.meta.LinuxOnlyFlags {
		if !flagDefined(c.cmd, name) {
			c.fail("linux-only flag " + name + " is not defined")
		}
	}

	if c.meta.SupportsDryRun && c.cmd.Flags().Lookup("dry-run") == nil {
		c.fail("supports_dry_run set without --dry-run flag")
	}
}

// checkAudience checks where a command is exposed and when it restarts.
func (c *commandMetaCheck) checkAudience() {
	if c.meta.Audience == "node" && !c.meta.LinuxOnly {
		c.fail("node audience requires linuxOnly")
	}

	switch c.meta.Audience {
	case "operator", "node":
	default:
		c.fail("audience must be operator or node")
	}

	switch c.meta.NeedsRestart {
	case "never", "maybe", "always":
	default:
		c.fail("needs_restart must be never, maybe, or always")
	}

	if group := groupFromCommand(c.cmd); c.meta.Group != group {
		c.fail(fmt.Sprintf("group %q does not match cobra parent %q", c.meta.Group, group))
	}

	if c.meta.Audience == "node" && !strings.HasPrefix(c.path, "vaire node") {
		c.fail("node audience is only valid under vaire node")
	}

	if strings.HasPrefix(c.path, "vaire node") && c.path != "vaire node upgrade" && c.meta.Audience != "node" {
		c.fail("command under vaire node must have node audience")
	}
}

// checkErrorCodes checks the error codes a command documents.
func (c *commandMetaCheck) checkErrorCodes() {
	for _, code := range c.meta.ErrorCodes {
		if !output.IsKnownCode(code) {
			c.fail("unknown error code " + code)
		}
	}
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
