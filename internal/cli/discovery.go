package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	agentNotesAnnotation    = "agent.notes"
	agentCategoryAnnotation = "agent.category"
)

type commandDiscovery struct {
	Name           string             `json:"name"`
	Path           string             `json:"path"`
	Hidden         bool               `json:"hidden,omitempty"`
	Aliases        []string           `json:"aliases,omitempty"`
	Short          string             `json:"short,omitempty"`
	Long           string             `json:"long,omitempty"`
	Usage          string             `json:"usage"`
	Examples       string             `json:"examples,omitempty"`
	LocalFlags     []flagDiscovery    `json:"local_flags,omitempty"`
	InheritedFlags []flagDiscovery    `json:"inherited_flags,omitempty"`
	Subcommands    []commandSummary   `json:"subcommands,omitempty"`
	AgentNotes     string             `json:"agent_notes,omitempty"`
	Category       string             `json:"category,omitempty"`
	Meta           *CommandMeta       `json:"meta,omitempty"`
	Commands       []commandDiscovery `json:"commands,omitempty"`
}

type commandSummary struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Hidden   bool     `json:"hidden,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
	Short    string   `json:"short,omitempty"`
	Category string   `json:"category,omitempty"`
}

type flagDiscovery struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Usage     string `json:"usage,omitempty"`
	Default   string `json:"default,omitempty"`
	Type      string `json:"type,omitempty"`
}

type catalogRecord struct {
	CommandMeta
	Name    string          `json:"name"`
	Path    string          `json:"path"`
	Hidden  bool            `json:"hidden,omitempty"`
	Aliases []string        `json:"aliases,omitempty"`
	Usage   string          `json:"usage"`
	Flags   []flagDiscovery `json:"flags,omitempty"`
}

type commandsDiscoveryPayload struct {
	commandDiscovery
	Catalog []catalogRecord `json:"catalog"`
}

func runCommands(cmd *cobra.Command, _ []string) error {
	root := cmd.Root()
	check, err := cmd.Flags().GetBool("check-meta")
	if err != nil {
		return output.NewError(output.CodeUsage, err.Error(), output.ExitUsage, "")
	}
	checkSteps, err := cmd.Flags().GetBool("check-steps")
	if err != nil {
		return output.NewError(output.CodeUsage, err.Error(), output.ExitUsage, "")
	}
	if checkSteps {
		return runCheckSteps(cmd)
	}
	path, err := cmd.Flags().GetString("path")
	if err != nil {
		return output.NewError(output.CodeUsage, err.Error(), output.ExitUsage, "")
	}
	if check && strings.TrimSpace(path) != "" {
		return output.NewError(
			output.CodeUsage,
			"choose only one of --check-meta or --path",
			output.ExitUsage,
			"Run vaire commands --json, --path, or --check-meta.",
		)
	}
	if check {
		return runCommandsCheck(cmd, root)
	}
	opts, err := output.ParseOptions(cmd)
	if err != nil {
		return err
	}
	if strings.TrimSpace(path) != "" {
		found, ok := findCommand(root, path)
		if !ok {
			return output.NewError(
				output.CodeUsage,
				"unknown command "+path,
				output.ExitUsage,
				"Run vaire commands --json to list commands.",
			)
		}
		if opts.Mode == output.ModeHuman || opts.Mode == output.ModeMarkdown {
			return printCommand(cmd, found)
		}
		return output.OK(cmd, discoverCommand(found), "Discovered "+found.CommandPath())
	}
	if opts.Mode == output.ModeHuman || opts.Mode == output.ModeMarkdown {
		table := renderCatalogMarkdown(flattenCatalog(root))
		if _, err := fmt.Fprint(cmd.OutOrStdout(), table); err != nil {
			return err
		}
		return nil
	}
	payload := commandsDiscoveryPayload{
		commandDiscovery: discoverCommandTree(root),
		Catalog:          flattenCatalog(root),
	}
	return output.OK(cmd, payload, "Discovered vaire commands")
}

func findCommand(root *cobra.Command, path string) (*cobra.Command, bool) {
	want := strings.TrimSpace(path)
	for _, cmd := range walkCommands(root) {
		if cmd.CommandPath() == want || commandPathID(cmd) == want {
			return cmd, true
		}
	}
	return nil, false
}

func commandPathID(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	fields := strings.Fields(cmd.CommandPath())
	if len(fields) <= 1 {
		if len(fields) == 1 {
			return fields[0]
		}
		return ""
	}
	return strings.Join(fields[1:], " ")
}

func discoverCommandTree(root *cobra.Command) commandDiscovery {
	discovery := discoverCommand(root)
	for _, cmd := range childCommands(root) {
		discovery.Commands = append(discovery.Commands, discoverCommandTree(cmd))
	}
	return discovery
}

func discoverCommand(cmd *cobra.Command) commandDiscovery {
	discovery := commandDiscovery{
		Name:           cmd.Name(),
		Path:           cmd.CommandPath(),
		Hidden:         commandHidden(cmd),
		Aliases:        append([]string(nil), cmd.Aliases...),
		Short:          strings.TrimSpace(cmd.Short),
		Long:           strings.TrimSpace(cmd.Long),
		Usage:          cmd.UseLine(),
		Examples:       strings.TrimSpace(cmd.Example),
		LocalFlags:     discoverFlags(cmd.LocalFlags()),
		InheritedFlags: discoverFlags(cmd.InheritedFlags()),
		Subcommands:    discoverSubcommands(cmd),
	}
	if cmd.Annotations != nil {
		discovery.AgentNotes = cmd.Annotations[agentNotesAnnotation]
		discovery.Category = cmd.Annotations[agentCategoryAnnotation]
	}
	if meta, ok := metaFor(cmd); ok {
		copied := meta
		discovery.Meta = &copied
	}
	return discovery
}

func discoverSubcommands(cmd *cobra.Command) []commandSummary {
	children := childCommands(cmd)
	summaries := make([]commandSummary, 0, len(children))
	for _, sub := range children {
		summary := commandSummary{
			Name:    sub.Name(),
			Path:    sub.CommandPath(),
			Hidden:  commandHidden(sub),
			Aliases: append([]string(nil), sub.Aliases...),
			Short:   strings.TrimSpace(sub.Short),
		}
		if sub.Annotations != nil {
			summary.Category = sub.Annotations[agentCategoryAnnotation]
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func childCommands(cmd *cobra.Command) []*cobra.Command {
	out := make([]*cobra.Command, 0)
	for _, sub := range cmd.Commands() {
		if sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		out = append(out, sub)
	}
	return out
}

func discoverFlags(flags *pflag.FlagSet) []flagDiscovery {
	if flags == nil {
		return nil
	}
	discovered := make([]flagDiscovery, 0)
	flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		discovered = append(discovered, flagDiscovery{
			Name:      flag.Name,
			Shorthand: flag.Shorthand,
			Usage:     strings.TrimSpace(flag.Usage),
			Default:   flag.DefValue,
			Type:      flag.Value.Type(),
		})
	})
	sort.SliceStable(discovered, func(i, j int) bool {
		return discovered[i].Name < discovered[j].Name
	})
	return discovered
}

func flattenCatalog(root *cobra.Command) []catalogRecord {
	commands := walkCommands(root)
	records := make([]catalogRecord, 0, len(commands))
	for _, cmd := range commands {
		meta, _ := metaFor(cmd)
		records = append(records, catalogRecord{
			CommandMeta: meta,
			Name:        cmd.Name(),
			Path:        cmd.CommandPath(),
			Hidden:      commandHidden(cmd),
			Aliases:     append([]string(nil), cmd.Aliases...),
			Usage:       cmd.UseLine(),
			Flags:       discoverFlags(cmd.LocalFlags()),
		})
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Path < records[j].Path
	})
	return records
}

func renderCatalogMarkdown(records []catalogRecord) string {
	var b strings.Builder
	b.WriteString("| Command | Summary | Audience | Mutating | Dry-run | Confirm |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, record := range records {
		dry := "no"
		if record.SupportsDryRun {
			dry = "yes"
		} else if record.SkipDryRunCheck {
			dry = "n/a"
		}
		mutating := "no"
		if record.Mutating {
			mutating = "yes"
		} else if len(record.MutatingFlags) > 0 {
			mutating = "with " + strings.Join(record.MutatingFlags, ", ")
		}
		fmt.Fprintf(
			&b,
			"| `%s` | %s | %s | %s | %s | %s |\n",
			record.Path,
			escapeCell(record.Summary),
			record.Audience,
			mutating,
			dry,
			escapeCell(record.ConfirmToken),
		)
	}
	return b.String()
}

func printCommand(cmd *cobra.Command, found *cobra.Command) error {
	meta, _ := metaFor(found)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\nUsage: %s\n", found.CommandPath(), meta.Summary, found.UseLine())
	if len(meta.Examples) > 0 {
		b.WriteString("\nExamples:\n")
		for _, example := range meta.Examples {
			fmt.Fprintf(&b, "  %s\n", example)
		}
	}
	_, err := fmt.Fprint(cmd.OutOrStdout(), b.String())
	return err
}

func escapeCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func setAgentMetadata(cmd *cobra.Command, category, notes string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[agentCategoryAnnotation] = category
	cmd.Annotations[agentNotesAnnotation] = notes
}
