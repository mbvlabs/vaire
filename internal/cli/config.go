package cli

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/spf13/cobra"
)

// configData is the config.get result.
type configData struct {
	TOML     string `json:"toml"`
	Revision string `json:"revision"`
}

func fetchConfig(cmd *cobra.Command, s *session, p *progress) (configData, error) {
	reply, err := s.node.Call(commandContext(cmd), "config.get", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return configData{}, err
	}
	var data configData
	if err := reply.Decode(&data); err != nil {
		return configData{}, err
	}
	return data, nil
}

func runConfigShow(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return err
	}
	opts, _ := output.ParseOptions(cmd)
	if opts.Mode == output.ModeHuman || opts.Mode == output.ModeMarkdown {
		if opts.Quiet {
			return nil
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), data.TOML)
		return err
	}
	return output.Success(cmd, output.Result{Data: data, Message: "cluster.toml (" + data.Revision[:12] + ")", Warnings: s.warnings})
}

func runConfigGet(cmd *cobra.Command, args []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return err
	}
	cfg, err := config.ParseCluster([]byte(data.TOML))
	if err != nil {
		return err
	}
	value, err := cfg.Get(args[0])
	if err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: map[string]any{"key": args[0], "value": value}, Message: fmt.Sprintf("%s = %v", args[0], value), Warnings: s.warnings})
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	key, value := args[0], args[1]
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return err
	}
	var lifted []string
	if old, err := config.ParseCluster([]byte(data.TOML)); err == nil {
		updated := old
		updated.Clusters = slices.Clone(old.Clusters)
		if updated.Set(key, value) == nil {
			lifted = node.Unprotected(old, updated)
		}
	}
	confirm, confirms, err := confirmUnprotect(cmd, lifted)
	if err != nil {
		return err
	}
	reply, err := s.node.Call(commandContext(cmd), "config.set", map[string]any{
		"set":           map[string]string{key: value},
		"base_revision": data.Revision,
	}, remote.CallOptions{DryRun: dryRun(cmd), Confirm: confirm, Confirms: confirms, OnEvent: p.event})
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		var planned struct {
			Plan engine.PlanData `json:"plan"`
		}
		if err := reply.Decode(&planned); err != nil {
			return err
		}
		return output.Success(cmd, output.Result{
			Data:     map[string]any{"key": key, "value": value, "plan": planned.Plan},
			Message:  fmt.Sprintf("dry run: would set %s = %s; %d of %d steps would then apply", key, value, planned.Plan.WouldApply, planned.Plan.Total),
			Warnings: append(s.warnings, reply.Warnings...),
		})
	}
	var res struct {
		Revision string          `json:"revision"`
		Plan     engine.PlanData `json:"plan"`
	}
	if err := reply.Decode(&res); err != nil {
		return err
	}
	if flagTrue(cmd, "apply") {
		return runApply(cmd, nil)
	}
	warnings := append(append(s.warnings, reply.Warnings...), syncContextFromNode(cmd, s, p)...)
	return output.Success(cmd, output.Result{
		Data:     res,
		Message:  fmt.Sprintf("set %s = %s; %d of %d steps would apply on the next eru apply", key, value, res.Plan.WouldApply, res.Plan.Total),
		Warnings: warnings,
		Next:     []string{"eru apply"},
	})
}

func runConfigValidate(cmd *cobra.Command, _ []string) error {
	if path := stringFlag(cmd, "file"); path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this file.
		if err != nil {
			return output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		cfg, err := config.ParseCluster(raw)
		if err != nil {
			return output.NewError(output.CodeConfig, path+" does not parse: "+err.Error(), output.ExitConfig, "")
		}
		if err := cfg.Validate(); err != nil {
			return config.ValidationError(err)
		}
		return output.Success(cmd, output.Result{Data: map[string]any{"valid": true}, Message: path + " is valid"})
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := s.node.Call(commandContext(cmd), "config.validate", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Valid    bool     `json:"valid"`
		Problems []string `json:"problems"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}
	if !data.Valid {
		return config.ValidationError(fmt.Errorf("%s", strings.Join(data.Problems, "; ")))
	}
	return output.Success(cmd, output.Result{Data: data, Message: "cluster.toml is valid", Warnings: s.warnings})
}

func runConfigEdit(cmd *cobra.Command, _ []string) error {
	if MustNotPrompt(cmd) {
		return output.NewError(output.CodeInputRequired, "eru config edit needs a terminal", output.ExitInput,
			"Use eru config set <key> <value> instead.").WithDetails(map[string]any{"flag": "--file"})
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "eru-cluster-*.toml")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if _, err := tmp.WriteString(data.TOML); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	edited, err := editUntilValid(cmd, path)
	if err != nil {
		return err
	}
	if string(edited) == data.TOML {
		return output.Success(cmd, output.Result{Data: map[string]any{"changed": false}, Message: "no changes", Warnings: s.warnings})
	}
	preview, err := s.node.Call(commandContext(cmd), "config.set", map[string]any{
		"toml":          string(edited),
		"base_revision": data.Revision,
	}, remote.CallOptions{DryRun: true, OnEvent: p.event})
	if err != nil {
		return err
	}
	var planned struct {
		Plan engine.PlanData `json:"plan"`
	}
	if err := preview.Decode(&planned); err != nil {
		return err
	}
	p.note(render.DiffBytes("cluster.toml", []byte(data.TOML), edited) +
		fmt.Sprintf("%d of %d steps would apply on the next eru apply.", planned.Plan.WouldApply, planned.Plan.Total))
	ok, err := PromptYes(cmd, "Push this configuration?")
	if err != nil {
		return err
	}
	if !ok {
		return output.NewError(output.CodeUnsafeAction, "the edit was discarded", output.ExitUnsafe, "")
	}
	var lifted []string
	if old, oerr := config.ParseCluster([]byte(data.TOML)); oerr == nil {
		if updated, uerr := config.ParseCluster(edited); uerr == nil {
			lifted = node.Unprotected(old, updated)
		}
	}
	confirm, confirms, err := confirmUnprotect(cmd, lifted)
	if err != nil {
		return err
	}
	reply, err := s.node.Call(commandContext(cmd), "config.set", map[string]any{
		"toml":          string(edited),
		"base_revision": data.Revision,
	}, remote.CallOptions{Confirm: confirm, Confirms: confirms, OnEvent: p.event})
	if err != nil {
		return err
	}
	var res struct {
		Revision string          `json:"revision"`
		Plan     engine.PlanData `json:"plan"`
	}
	if err := reply.Decode(&res); err != nil {
		return err
	}
	if flagTrue(cmd, "apply") {
		return runApply(cmd, nil)
	}
	warnings := append(s.warnings, reply.Warnings...)
	if cfg, err := config.ParseCluster(edited); err == nil {
		synced, err := syncContext(s, cfg)
		if err != nil {
			synced = contextSyncFailed(s, err)
		}
		warnings = append(warnings, synced...)
	}
	return output.Success(cmd, output.Result{
		Data:     res,
		Message:  fmt.Sprintf("saved %d of %d steps would apply on the next eru apply", res.Plan.WouldApply, res.Plan.Total),
		Warnings: warnings,
		Next:     []string{"eru apply"},
	})
}

// confirmUnprotect has a human type <db>/unprotect for each database whose
// protection the change lifts, and returns the tokens for the request. It
// refuses without an interactive terminal, with --yes and in structured
// output: an agent or a script must not be able to lift a protection it is
// meant to be stopped by. A dry run lifts nothing, so it asks nothing.
func confirmUnprotect(cmd *cobra.Command, lifted []string) (string, []string, error) {
	if len(lifted) == 0 || dryRun(cmd) {
		return "", nil, nil
	}
	tokens := make([]string, 0, len(lifted))
	for _, name := range lifted {
		tokens = append(tokens, node.UnprotectToken(name))
	}
	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return "", nil, output.NewError(output.CodeDatabaseProtected,
			"lifting the protection of "+strings.Join(lifted, ", ")+" needs a human at a terminal", output.ExitUnsafe,
			"A person runs this command in an interactive terminal, without --yes or structured output, and types "+strings.Join(tokens, ", ")+". Agents and scripts cannot lift it.").
			WithDetails(map[string]any{"reason": "unprotect_requires_human", "databases": lifted, "confirm_tokens": tokens})
	}
	for i, name := range lifted {
		if err := typeWord(cmd, "Type '"+tokens[i]+"' to lift the protection of "+name,
			name+" is protected: db drop, a shrinking resize, credential rotation and restore promote/undo are refused while it is set.", tokens[i]); err != nil {
			return "", nil, output.NewError(output.CodeDatabaseProtected, "the protection of "+name+" was kept", output.ExitUnsafe, "").
				WithDetails(map[string]any{"reason": "unprotect_not_confirmed", "database": name})
		}
	}
	return tokens[0], tokens[1:], nil
}

// editUntilValid opens path in the editor until the result parses and
// validates, or the operator gives up.
func editUntilValid(cmd *cobra.Command, path string) ([]byte, error) {
	for {
		if err := runEditor(cmd, path); err != nil {
			return nil, err
		}
		edited, err := os.ReadFile(path) //nolint:gosec // G304: our own temp file.
		if err != nil {
			return nil, err
		}
		problem := editProblem(edited)
		if problem == nil {
			return edited, nil
		}
		again, err := PromptYes(cmd, problem.Error()+"\nEdit again?")
		if err != nil {
			return nil, err
		}
		if !again {
			return nil, problem
		}
	}
}

// editProblem returns why an edited cluster.toml is not acceptable.
func editProblem(raw []byte) error {
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return output.NewError(output.CodeConfig, "the edited file does not parse: "+err.Error(), output.ExitConfig, "")
	}
	if err := cfg.Validate(); err != nil {
		return config.ValidationError(err)
	}
	return nil
}

// runEditor opens path in $VISUAL or $EDITOR, falling back to vi.
func runEditor(cmd *cobra.Command, path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	args := append(parts[1:], path)
	child := exec.CommandContext(commandContext(cmd), parts[0], args...) //nolint:gosec // G204: the operator's chosen editor.
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "Set $EDITOR to a working editor.")
	}
	return nil
}

// syncContext refreshes the local context from cfg: the domain and TLS mode,
// each cached node's hostname, and the database placement map. eru sql,
// psql, console, tunnel and secrets export verify the server certificate
// against the master's hostname, so it has to follow a domain change made
// with config set, config edit, or by another operator.
func syncContext(s *session, cfg config.Cluster) ([]output.Warning, error) {
	previous, previousMode := s.ctx.MasterHostname(), s.ctx.TLSMode
	changed := s.ctx.Domain != cfg.Domain || s.ctx.TLSMode != cfg.TLS.Mode
	s.ctx.Domain, s.ctx.TLSMode = cfg.Domain, cfg.TLS.Mode
	for i, n := range s.ctx.Nodes {
		if h := cfg.Hostname(n.Name); h != n.Hostname {
			s.ctx.Nodes[i].Hostname = h
			changed = true
		}
	}
	if placement := placementOf(cfg); !maps.Equal(placement, s.ctx.Databases) {
		s.ctx.Databases = placement
		changed = true
	}
	if !changed {
		return nil, nil
	}
	if err := contexts.Save(s.ctx); err != nil {
		return nil, err
	}
	hostname := s.ctx.MasterHostname()
	if hostname == previous && cfg.TLS.Mode == previousMode {
		return nil, nil
	}
	message := fmt.Sprintf("context %s now verifies the server as %s (tls.mode %s)", s.ctx.Name, hostname, cfg.TLS.Mode)
	if previous != hostname {
		message += "; until the node holds a certificate for " + hostname +
			", connections from eru fail with a hostname mismatch. eru-certs.timer issues it within the hour, or now with: eru ssh --command 'sudo eru node run certs-renew'"
	}
	return []output.Warning{{Code: "context_tls_updated", Message: message}}, nil
}

// placementOf is the database placement map of cfg: database name to node.
func placementOf(cfg config.Cluster) map[string]string {
	out := map[string]string{}
	for _, cl := range cfg.Clusters {
		out[cl.Name] = cl.Node
	}
	return out
}

// syncContextFromNode fetches cluster.toml and runs syncContext on it.
// The command it follows already succeeded, so a failure here is a warning.
func syncContextFromNode(cmd *cobra.Command, s *session, p *progress) []output.Warning {
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return contextSyncFailed(s, err)
	}
	cfg, err := config.ParseCluster([]byte(data.TOML))
	if err != nil {
		return contextSyncFailed(s, err)
	}
	warnings, err := syncContext(s, cfg)
	if err != nil {
		return contextSyncFailed(s, err)
	}
	return warnings
}

func contextSyncFailed(s *session, err error) []output.Warning {
	return []output.Warning{{
		Code:    "context_sync_failed",
		Message: "could not update the domain and TLS mode of context " + s.ctx.Name + ": " + err.Error() + ". Run eru apply to retry.",
	}}
}
