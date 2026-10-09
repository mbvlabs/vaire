//go:build e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/e2e/harness"
)

func TestVersionJSON(t *testing.T) {
	env := harness.Eru(t, "version")
	if env.Exit != 0 || !env.OK {
		t.Fatalf("version failed: %s", env.Stdout)
	}
	if env.Command != "version" {
		t.Fatalf("command %q", env.Command)
	}
	var data map[string]string
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["version"] == "" || data["commit"] == "" || data["channel"] == "" {
		t.Fatalf("version data %#v", data)
	}
	if data["release_base_url"] != "https://eru.mbvlabs.com" {
		t.Fatalf("release base %q", data["release_base_url"])
	}
}

func TestVersionHumanAndQuiet(t *testing.T) {
	stdout, stderr, code := harness.Run(t, "version")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "eru ") {
		t.Fatalf("human version code %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = harness.Run(t, "version", "--quiet")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("quiet version code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestAgentModeIncludesNext(t *testing.T) {
	env := harness.Eru(t, "version", "--output", "agent")
	if env.Exit != 0 || env.Message == "" || len(env.Next) == 0 {
		t.Fatalf("agent envelope: %+v", env)
	}
	stdout, _, code := harness.Run(t, "version", "--output", "json")
	if code != 0 {
		t.Fatal(code)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["message"]; ok {
		t.Fatalf("json mode should omit message: %s", stdout)
	}
}

func TestStreamResult(t *testing.T) {
	stdout, stderr, code := harness.Run(t, "version", "--stream")
	if code != 0 || stderr != "" {
		t.Fatalf("stream code %d stderr %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines %#v", lines)
	}
	var event struct {
		Schema   string `json:"schema"`
		Type     string `json:"type"`
		Envelope struct {
			OK bool `json:"ok"`
		} `json:"envelope"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event.Schema != "eru-event/v1" || event.Type != "result" || !event.Envelope.OK {
		t.Fatalf("event %+v", event)
	}
}

func TestJQ(t *testing.T) {
	stdout, stderr, code := harness.Run(t, "version", "--jq", ".ok")
	if code != 0 || stderr != "" || strings.TrimSpace(stdout) != "true" {
		t.Fatalf("jq code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestOutputModeErrors(t *testing.T) {
	stdout, stderr, code := harness.Run(t, "version", "--output", "yaml")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid_output_mode") {
		t.Fatalf("bad mode code %d stdout %q stderr %q", code, stdout, stderr)
	}
	_, stderr, code = harness.Run(t, "version", "--json", "--output", "agent")
	if code != 1 || !strings.Contains(stderr, "invalid_output_mode") {
		t.Fatalf("conflict code %d stderr %q", code, stderr)
	}
}

func TestParseErrorsHonourOutputMode(t *testing.T) {
	for _, args := range [][]string{
		{"bogus", "--json"},
		{"version", "--bogus", "--json"},
		{"version", "--bogus", "--output=agent"},
		{"--quiet", "bogus", "--output", "json"},
	} {
		stdout, stderr, code := harness.Run(t, args...)
		if code != 1 || stderr != "" {
			t.Fatalf("%v: code %d stderr %q", args, code, stderr)
		}
		env := harness.ParseEnvelope(t, stdout)
		if env.OK || env.Error == nil || env.Error.Code != "usage_error" {
			t.Fatalf("%v: %s", args, stdout)
		}
	}
	stdout, stderr, code := harness.Run(t, "db", "drop", "--stream")
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if code != 1 || stderr != "" || len(lines) != 1 || !strings.Contains(lines[0], `"type":"result"`) {
		t.Fatalf("stream: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestJQErrorKeepsEnvelopeOutOfHint(t *testing.T) {
	stdout, stderr, code := harness.Run(t, "version", "--jq", "error")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "jq filter failed") || strings.Contains(stderr, "release_base_url") {
		t.Fatalf("jq error: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestRootRequiresCommand(t *testing.T) {
	env := harness.Eru(t)
	if env.Exit != 1 || env.OK || env.Error == nil || env.Error.Code != "usage_error" {
		t.Fatalf("root: %+v", env)
	}
	if env.Stderr != "" {
		t.Fatalf("structured failure leaked to stderr: %q", env.Stderr)
	}
}

func TestHelpHidesNode(t *testing.T) {
	stdout, _, code := harness.Run(t, "--help")
	if code != 0 {
		t.Fatal(code)
	}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "node" {
			t.Fatalf("node is visible in top-level help:\n%s", stdout)
		}
	}
}

func TestNodeVersion(t *testing.T) {
	env := harness.Eru(t, "node", "version")
	if env.Exit != 0 || !env.OK || env.Command != "node version" {
		t.Fatalf("node version: %+v %s", env, env.Stdout)
	}
}

func TestCheckMeta(t *testing.T) {
	env := harness.Eru(t, "commands", "--check-meta")
	if env.Exit != 0 || !env.OK {
		t.Fatalf("check-meta: %s", env.Stdout)
	}
}

func TestCatalogAudiences(t *testing.T) {
	env := harness.Eru(t, "commands")
	if env.Exit != 0 {
		t.Fatal(env.Stdout)
	}
	var payload struct {
		Catalog []struct {
			Path             string   `json:"path"`
			Audience         string   `json:"audience"`
			Hidden           bool     `json:"hidden"`
			Mutating         bool     `json:"mutating"`
			Destructive      bool     `json:"destructive"`
			DestructiveFlags []string `json:"destructive_flags"`
		} `json:"catalog"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatal(err)
	}
	type entry struct {
		Audience         string
		Hidden           bool
		Mutating         bool
		Destructive      bool
		DestructiveFlags []string
	}
	found := map[string]entry{}
	for _, record := range payload.Catalog {
		found[record.Path] = entry{record.Audience, record.Hidden, record.Mutating, record.Destructive, record.DestructiveFlags}
	}
	nodeVersion, ok := found["eru node version"]
	if !ok || nodeVersion.Audience != "node" || !nodeVersion.Hidden {
		t.Fatalf("node version catalog %#v", nodeVersion)
	}
	nodeUpgrade, ok := found["eru node upgrade"]
	if !ok || nodeUpgrade.Audience != "operator" || nodeUpgrade.Hidden || nodeUpgrade.Destructive {
		t.Fatalf("node upgrade catalog %#v", nodeUpgrade)
	}
	for path, flag := range map[string]string{
		"eru queries":        "--reset",
		"eru db credentials": "--rotate",
	} {
		record := found[path]
		if record.Mutating || record.Destructive || len(record.DestructiveFlags) != 1 || record.DestructiveFlags[0] != flag {
			t.Fatalf("%s catalog %#v", path, record)
		}
	}
	if _, ok := found["eru db drop"]; !ok {
		t.Fatal("missing db drop")
	}
}

func TestCommandsPath(t *testing.T) {
	env := harness.Eru(t, "commands", "--path", "version")
	if env.Exit != 0 || env.Command != "commands" {
		t.Fatalf("%+v", env)
	}
	var discovered struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(env.Data, &discovered); err != nil {
		t.Fatal(err)
	}
	if discovered.Path != "eru version" {
		t.Fatalf("path %q", discovered.Path)
	}
	missing := harness.Eru(t, "commands", "--path", "not-a-command")
	if missing.Exit != 1 || missing.Error == nil || missing.Error.Code != "usage_error" {
		t.Fatalf("missing path: %+v", missing)
	}
}

func TestGuideAgents(t *testing.T) {
	stdout, stderr, code := harness.Run(t, "guide", "agents")
	if code != 0 || stderr != "" {
		t.Fatalf("guide code %d stderr %s", code, stderr)
	}
	for _, needle := range []string{
		"--output agent",
		"eru commands --json",
		"confirm_token",
		"needs_restart",
		"usage_error",
		"invalid_output_mode",
		"context_not_found",
		"cluster_not_initialized",
		"missing_tool",
		"unsafe_action_requires_confirmation",
		"step_failed",
		"partial_failure",
		"external_command_failed",
		"config_error",
		"validation_failed",
		"ambiguous_input",
		"input_required",
		"remote_unreachable",
		"transport_failed",
		"host_key_mismatch",
		"node_version_mismatch",
		"update_required",
		"precondition_failed",
		"lockdown_failed",
		"secrets_error",
		"release_verification_failed",
		"must never be retried",
		"--sudo",
		"--install-missing",
		"db drop",
		"restore cleanup",
	} {
		if !strings.Contains(stdout, needle) {
			t.Errorf("guide missing %s", needle)
		}
	}
}

func TestSkillInstall(t *testing.T) {
	dir := t.TempDir()
	dry := harness.Eru(t, "skill", "install", "--harness", "pi", "--dir", dir, "--dry-run")
	if dry.Exit != 0 || !dry.OK {
		t.Fatalf("dry-run: %s", dry.Stdout)
	}
	skill := filepath.Join(dir, ".pi", "skills", "eru", "SKILL.md")
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote %s: %v", skill, err)
	}
	installed := harness.Eru(t, "skill", "install", "--harness", "pi", "--dir", dir)
	if installed.Exit != 0 || !installed.OK {
		t.Fatalf("install: %s", installed.Stdout)
	}
	body, err := os.ReadFile(skill) //nolint:gosec // G304: path is under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "name: eru") || !strings.Contains(string(body), "confirm_token") {
		t.Fatalf("skill body:\n%s", body)
	}
	info, err := os.Stat(skill)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	missing := harness.Eru(t, "skill", "install")
	if missing.Exit != 8 || missing.Error == nil || missing.Error.Code != "input_required" {
		t.Fatalf("missing harness: %+v", missing)
	}
}

func TestDestructiveConfirm(t *testing.T) {
	dropped := harness.Eru(t, "db", "drop", "app", "--yes")
	if dropped.Exit != 4 || dropped.Error == nil || dropped.Error.Code != "unsafe_action_requires_confirmation" {
		t.Fatalf("db drop --yes: exit %d %+v", dropped.Exit, dropped.Error)
	}
	var details struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := json.Unmarshal(dropped.Error.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details.ConfirmToken != "app" {
		t.Fatalf("token %q", details.ConfirmToken)
	}
	rotated := harness.Eru(t, "db", "credentials", "app", "--rotate", "--yes")
	if rotated.Exit != 4 || rotated.Error == nil || rotated.Error.Code != "unsafe_action_requires_confirmation" {
		t.Fatalf("rotate --yes: %+v", rotated.Error)
	}
	removed := harness.Eru(t, "context", "remove", "prod", "--confirm", "other")
	if removed.Exit != 4 {
		t.Fatalf("wrong token exit %d %s", removed.Exit, removed.Stdout)
	}
}

func TestHarnessHelpers(t *testing.T) {
	// Keep the Phase 1 helper surface referenced until later scenarios call it.
	if harness.Image() == "" {
		t.Fatal("missing image")
	}
	_ = harness.SQL
	_ = harness.KillTunnel
	_ = harness.Snapshot
	_ = harness.Restore
}
