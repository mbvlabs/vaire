package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/guide"
	"github.com/spf13/cobra"
)

type skillHarness struct {
	name      string
	label     string
	directory string
}

type skillInstallation struct {
	Harness string `json:"harness"`
	Path    string `json:"path"`
}

var skillHarnesses = []skillHarness{
	{name: "codex", label: "Codex", directory: ".codex/skills/eru"},
	{name: "claude", label: "Claude", directory: ".claude/skills/eru"},
	{name: "pi", label: "Pi", directory: ".pi/skills/eru"},
	{name: "opencode", label: "OpenCode", directory: ".opencode/skills/eru"},
	{name: "crush", label: "Crush", directory: ".crush/skills/eru"},
}

func runSkillInstall(cmd *cobra.Command, _ []string) error {
	requested, err := cmd.Flags().GetStringSlice("harness")
	if err != nil {
		return output.NewError(output.CodeUsage, err.Error(), output.ExitUsage, "")
	}
	dir, err := cmd.Flags().GetString("dir")
	if err != nil {
		return output.NewError(output.CodeUsage, err.Error(), output.ExitUsage, "")
	}
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return output.WrapError(output.CodeError, err, output.ExitGeneric, "")
	}
	selected, err := selectSkillHarnesses(cmd, requested)
	if err != nil {
		return err
	}
	dry := dryRun(cmd)
	installations := make([]skillInstallation, 0, len(selected))
	labels := make([]string, 0, len(selected))
	document := skillDocument()
	for _, harness := range selected {
		target := filepath.Join(root, filepath.FromSlash(harness.directory), "SKILL.md")
		if !dry {
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return output.WrapError(output.CodeError, fmt.Errorf("create skill directory: %w", err), output.ExitGeneric, "")
			}
			if err := writeSkillFile(target, document); err != nil {
				return output.WrapError(output.CodeError, fmt.Errorf("write skill: %w", err), output.ExitGeneric, "")
			}
		}
		installations = append(installations, skillInstallation{Harness: harness.name, Path: target})
		labels = append(labels, harness.label)
	}
	message := "Installed eru skill for " + strings.Join(labels, ", ")
	if dry {
		message = "Would install eru skill for " + strings.Join(labels, ", ")
	}
	data := map[string]any{
		"name":          "eru",
		"dry_run":       dry,
		"installations": installations,
	}
	if len(installations) == 1 {
		data["path"] = installations[0].Path
	}
	return output.OK(cmd, data, message)
}

func writeSkillFile(target, document string) error {
	return os.WriteFile(target, []byte(document), 0o644) //nolint:gosec // G304: path is the operator-selected skill directory.
}

func skillDocument() string {
	body := strings.TrimSpace(guide.Agents)
	return "---\nname: eru\ndescription: Operate a production PostgreSQL cluster with the eru CLI. Use --output agent, read error.code and next, and ask a human before passing destructive confirm tokens.\n---\n\n" + body + "\n"
}

func selectSkillHarnesses(cmd *cobra.Command, requested []string) ([]skillHarness, error) {
	if len(requested) > 0 {
		return parseSkillHarnessNames(requested)
	}
	if err := RequireFlag(cmd, false, "--harness", "skill install"); err != nil {
		return nil, err
	}
	return promptSkillHarnesses(cmd)
}

func promptSkillHarnesses(cmd *cobra.Command) ([]skillHarness, error) {
	writer := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(writer, "Select harnesses (comma-separated numbers):"); err != nil {
		return nil, err
	}
	for index, harness := range skillHarnesses {
		if _, err := fmt.Fprintf(writer, "  %d) %s\n", index+1, harness.label); err != nil {
			return nil, err
		}
	}
	if _, err := fmt.Fprint(writer, "Selection: "); err != nil {
		return nil, err
	}
	selection, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read harness selection: %w", err)
	}
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return nil, output.NewError(
			output.CodeInputRequired,
			"select at least one harness",
			output.ExitInput,
			"Pass --harness with one or more of: "+validSkillHarnessNames()+".",
		)
	}
	names := make([]string, 0, len(skillHarnesses))
	for _, value := range strings.Split(selection, ",") {
		number, convErr := strconv.Atoi(strings.TrimSpace(value))
		if convErr != nil || number < 1 || number > len(skillHarnesses) {
			return nil, output.NewError(
				output.CodeUsage,
				fmt.Sprintf("invalid harness selection %q", strings.TrimSpace(value)),
				output.ExitUsage,
				fmt.Sprintf("Enter comma-separated numbers from 1 to %d.", len(skillHarnesses)),
			)
		}
		names = append(names, skillHarnesses[number-1].name)
	}
	return parseSkillHarnessNames(names)
}

func parseSkillHarnessNames(names []string) ([]skillHarness, error) {
	selected := make([]skillHarness, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, value := range names {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" {
			return nil, output.NewError(
				output.CodeUsage,
				"harness name cannot be empty",
				output.ExitUsage,
				"Choose one or more of: "+validSkillHarnessNames()+".",
			)
		}
		if seen[name] {
			continue
		}
		found := false
		for _, harness := range skillHarnesses {
			if harness.name == name {
				selected = append(selected, harness)
				seen[name] = true
				found = true
				break
			}
		}
		if !found {
			return nil, output.NewError(
				output.CodeUsage,
				fmt.Sprintf("unknown harness %q", value),
				output.ExitUsage,
				"Choose one or more of: "+validSkillHarnessNames()+".",
			)
		}
	}
	if len(selected) == 0 {
		return nil, output.NewError(
			output.CodeUsage,
			"select at least one harness",
			output.ExitUsage,
			"Choose one or more of: "+validSkillHarnessNames()+".",
		)
	}
	return selected, nil
}

func validSkillHarnessNames() string {
	names := make([]string, len(skillHarnesses))
	for i, harness := range skillHarnesses {
		names[i] = harness.name
	}
	return strings.Join(names, ", ")
}
