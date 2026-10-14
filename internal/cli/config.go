package cli

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/totp"
	"github.com/spf13/cobra"
)

// configData is the config.get result.
type configData struct {
	TOML     string `json:"toml"`
	Revision string `json:"revision"`
}

func fetchConfig(cmd *cobra.Command, s *session, p *progress) (configData, error) {
	reply, err := p.call(commandContext(cmd), s.node, "config.get", nil, remote.CallOptions{OnEvent: p.event})
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
	code, err := unprotectCode(cmd, s, p, lifted)
	if err != nil {
		return err
	}
	reply, err := p.call(commandContext(cmd), s.node, "config.set", map[string]any{
		"set":           map[string]string{key: value},
		"base_revision": data.Revision,
	}, remote.CallOptions{DryRun: dryRun(cmd), Secrets: code, OnEvent: p.event})
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
		Message:  fmt.Sprintf("set %s = %s; ", key, value) + pendingSteps(res.Plan),
		Warnings: warnings,
		Next:     applyNext(res.Plan),
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
	reply, err := p.call(commandContext(cmd), s.node, "config.validate", nil, remote.CallOptions{OnEvent: p.event})
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
	preview, err := p.call(commandContext(cmd), s.node, "config.set", map[string]any{
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
	code, err := unprotectCode(cmd, s, p, lifted)
	if err != nil {
		return err
	}
	reply, err := p.call(commandContext(cmd), s.node, "config.set", map[string]any{
		"toml":          string(edited),
		"base_revision": data.Revision,
	}, remote.CallOptions{Secrets: code, OnEvent: p.event})
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
		Message:  "saved; " + pendingSteps(res.Plan),
		Warnings: warnings,
		Next:     applyNext(res.Plan),
	})
}

// unprotectCode asks for a current code from the authenticator app that
// eru protect init set up when the change lifts a database's protection, and
// returns it as the request's protect_code secret: secrets are redacted and
// never logged. The node checks the code; a typed word would prove nothing,
// since an agent at a terminal can type it too. It refuses without an
// interactive terminal, with --yes and in structured output. A dry run lifts
// nothing, so it asks nothing.
func unprotectCode(cmd *cobra.Command, s *session, p *progress, lifted []string) (map[string]string, error) {
	if len(lifted) == 0 || dryRun(cmd) {
		return nil, nil
	}
	names := strings.Join(lifted, ", ")
	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return nil, output.NewError(output.CodeDatabaseProtected,
			"lifting the protection of "+names+" needs a code from the authenticator app, typed at a terminal", output.ExitUnsafe,
			"A person runs this command in an interactive terminal, without --yes or structured output, and types the current 6-digit code from the authenticator app set up with eru protect init.").
			WithDetails(map[string]any{"reason": "unprotect_requires_human", "databases": lifted})
	}
	// Ask the node first, so nobody types a code that cannot work.
	status, err := fetchProtectStatus(cmd, s, p)
	if err != nil {
		return nil, err
	}
	if !status.Configured {
		return nil, output.NewError(output.CodeDatabaseProtected,
			"lifting the protection of "+names+" needs two-factor protection, which this node does not have set up", output.ExitUnsafe,
			"A human runs eru protect init first, at a terminal, and scans the QR code with an authenticator app. Lifting a protection then needs a current code from it.").
			WithDetails(map[string]any{"reason": "protect_not_initialized", "databases": lifted})
	}
	if status.LockedUntil != "" {
		return nil, output.NewError(output.CodeProtectCodeRejected, "too many wrong codes; codes are locked out until "+status.LockedUntil, output.ExitUnsafe,
			"Wait until the lockout ends, then try again with a fresh code.").
			WithDetails(map[string]any{"reason": "locked", "locked_until": status.LockedUntil})
	}
	p.note(liftNotice(names))
	code, err := readSecretTTY(cmd, "Current code from the authenticator app")
	if err != nil {
		return nil, err
	}
	code = totp.NormalizeCode(code)
	if !totp.WellFormed(code) {
		return nil, output.NewError(output.CodeProtectCodeRejected, "the code must be 6 digits; the protection of "+names+" was kept", output.ExitUnsafe,
			"Type the 6-digit code the authenticator app shows for eru.").
			WithDetails(map[string]any{"reason": "code_invalid", "databases": lifted})
	}
	return map[string]string{node.ProtectCodeSecret: code}, nil
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
	// The editor owns the terminal: nothing of eru's live area may draw.
	resume := ui.Suspend()
	err := child.Run()
	resume()
	if err != nil {
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

// liftNotice is printed before the code prompt of a change that lifts a
// protection.
func liftNotice(names string) string {
	return "Lifting the protection of " + names + " needs a code from the authenticator app set up with eru protect init."
}

// pendingSteps says what the next eru apply would do after a config change.
func pendingSteps(plan engine.PlanData) string {
	if plan.WouldApply == 0 {
		return "nothing to apply"
	}
	return fmt.Sprintf("%d of %d steps would apply on the next eru apply", plan.WouldApply, plan.Total)
}

// applyNext suggests eru apply only when a step would apply.
func applyNext(plan engine.PlanData) []string {
	if plan.WouldApply == 0 {
		return nil
	}
	return []string{"eru apply"}
}
