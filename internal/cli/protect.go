package cli

import (
	"bufio"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/totp"
	"github.com/spf13/cobra"
)

// protectStatusData is the protect.status result.
type protectStatusData struct {
	Configured  bool     `json:"configured"`
	Pending     bool     `json:"pending"`
	ConfirmedAt string   `json:"confirmed_at,omitempty"`
	Failures    int      `json:"failures"`
	LockedUntil string   `json:"locked_until,omitempty"`
	Protected   []string `json:"protected_databases"`
	// Databases is every database with whether it is protected.
	Databases []node.ProtectedDatabase `json:"databases"`
}

func fetchProtectStatus(cmd *cobra.Command, s *session, p *progress) (protectStatusData, error) {
	reply, err := p.call(commandContext(cmd), s.node, "protect.status", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return protectStatusData{}, err
	}
	var data protectStatusData
	if err := reply.Decode(&data); err != nil {
		return protectStatusData{}, err
	}
	return data, nil
}

// protectCodeAttempts is how many codes protect init asks for before it
// gives up on confirming the new secret.
const protectCodeAttempts = 3

// runProtectInit sets up the TOTP secret that lifting a database's
// protection asks a code from. The node makes and keeps the secret; this
// shows it once as a QR code and confirms it with a code before the node
// uses it. Then it protects the databases --db or --all name, or the ones
// the operator picks, in the same run. Only a person at a terminal can do
// this.
func runProtectInit(cmd *cobra.Command, _ []string) error {
	rotate, all := flagTrue(cmd, "rotate"), flagTrue(cmd, "all")
	dbFlags, _ := cmd.Flags().GetStringArray("db")
	if all && len(dbFlags) > 0 {
		return output.NewError(output.CodeUsage, "pass --db or --all, not both", output.ExitUsage,
			"--all protects every database; --db names one and can be repeated.")
	}
	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return output.NewError(output.CodeInputRequired, "eru protect init needs a human at an interactive terminal", output.ExitInput,
			"Run it yourself in a terminal, without --yes, --no-input or structured output: it shows a QR code to scan with an authenticator app and asks for a code from it.").
			WithDetails(map[string]any{"reason": "protect_requires_human"})
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	// Check the names before the QR code, so a typo costs nothing.
	cfg, _, err := remoteConfig(cmd, s, p)
	if err != nil {
		return err
	}
	var chosen []string
	if all || len(dbFlags) > 0 {
		if chosen, err = protectTargets(dbFlags, all, databaseNames(cfg)); err != nil {
			return err
		}
	}
	var initSecrets map[string]string
	if rotate {
		code, err := readProtectCode(cmd, "Current code from the existing eru entry in the authenticator app")
		if err != nil {
			return err
		}
		initSecrets = map[string]string{node.ProtectCodeSecret: code}
	}
	reply, err := p.call(commandContext(cmd), s.node, "protect.init", map[string]any{"rotate": rotate, "label": s.ctx.Name},
		remote.CallOptions{Secrets: initSecrets, OnEvent: p.event})
	if err != nil {
		return err
	}
	var setup struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	if err := reply.Decode(&setup); err != nil {
		return err
	}
	if err := showProtectSecret(cmd, setup.URI, setup.Secret); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		code, err := readProtectCode(cmd, "Code the authenticator app now shows for eru:"+s.ctx.Name)
		if err != nil {
			return err
		}
		reply, err = p.call(commandContext(cmd), s.node, "protect.confirm", nil,
			remote.CallOptions{Secrets: map[string]string{node.ProtectCodeSecret: code}, OnEvent: p.event})
		if err == nil {
			break
		}
		if attempt >= protectCodeAttempts || !retryableCode(err) {
			return err
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "That code was not accepted ("+err.Error()+"). Check the device's clock and try the next code.")
	}
	var done struct {
		Rotated bool `json:"rotated"`
	}
	_ = reply.Decode(&done)
	message := "two-factor protection is set up: lifting a database's protection now asks for a code from eru:" + s.ctx.Name + " in the authenticator app"
	if done.Rotated {
		message = "the two-factor secret was rotated: remove the old eru:" + s.ctx.Name + " entry from the authenticator app and keep the new one"
	}

	// The secret works now, so protecting can no longer be refused for the
	// lack of it. Without --db or --all, ask which databases to protect.
	_, unprotected := splitProtected(cfg, databaseNames(cfg))
	if chosen == nil && len(unprotected) > 0 {
		if chosen, err = chooseToProtect(cmd, unprotected); err != nil {
			return err
		}
	}
	warnings := s.warnings
	_, todo := splitProtected(cfg, chosen)
	var newly []string
	if len(todo) > 0 {
		if err := setProtected(cmd, s, p, todo, true, nil); err != nil {
			warnings = append(warnings, output.Warning{Code: "protect_failed",
				Message: "two-factor protection is set up, but protecting " + strings.Join(todo, ", ") + " failed: " + err.Error() + ". Run eru db protect " + strings.Join(todo, " ") + " to retry"})
		} else {
			newly = todo
			for _, name := range todo {
				_ = cfg.Set("clusters."+name+".protected", "true")
			}
		}
	}
	protected, rest := splitProtected(cfg, databaseNames(cfg))
	tail, next := protectInitOutcome(protected, rest)
	return output.Success(cmd, output.Result{
		Data: map[string]any{"configured": true, "rotated": done.Rotated,
			"protected_databases": nonNil(protected), "newly_protected": nonNil(newly), "unprotected_databases": nonNil(rest)},
		Message:  message + "; " + tail,
		Warnings: warnings,
		Next:     next,
	})
}

// protectInitOutcome is the end of protect init's message and its next
// steps, from the databases protected after the run and the rest. The next
// steps name the databases; they never hold a <db> placeholder.
func protectInitOutcome(protected, unprotected []string) (string, []string) {
	switch {
	case len(protected) == 0 && len(unprotected) == 0:
		return "there are no databases yet; create them protected with eru db create and --protected", nil
	case len(unprotected) == 0:
		return "protected: " + strings.Join(protected, ", "), []string{"eru protect status"}
	case len(protected) == 0:
		return "no database is protected (" + strings.Join(unprotected, ", ") + " can be protected with eru db protect)",
			[]string{"eru db protect " + strings.Join(unprotected, " "), "eru protect status"}
	default:
		return "protected: " + strings.Join(protected, ", ") + "; not protected: " + strings.Join(unprotected, ", "),
			[]string{"eru db protect " + strings.Join(unprotected, " "), "eru protect status"}
	}
}

// chooseToProtect asks which of options to protect, all of them picked to
// start with. Cancelling the prompt picks none: the secret is set up
// already, and eru db protect protects later.
func chooseToProtect(cmd *cobra.Command, options []string) ([]string, error) {
	title := "Protect which databases?"
	if richOutput(cmd) {
		chosen, err := ui.MultiSelect(title, "Space toggles, Enter confirms. A protected database refuses drop, a shrinking resize, credential rotation and restore promote/undo.", options, options)
		if errors.Is(err, ui.ErrAborted) {
			return []string{}, nil
		}
		return chosen, err
	}
	resume := ui.Suspend()
	defer resume()
	reader := bufio.NewReader(cmd.InOrStdin())
	for attempt := 1; ; attempt++ {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s %s\nNames separated by spaces or commas; Enter for all, none for none: ", title, strings.Join(options, ", "))
		line, rerr := reader.ReadString('\n')
		if rerr != nil && line == "" {
			return []string{}, nil
		}
		chosen, err := parseSelection(line, options)
		if err == nil {
			return chosen, nil
		}
		if attempt >= protectCodeAttempts {
			return nil, err
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
	}
}

// parseSelection reads a typed answer to the database selection: empty for
// all of options, "none" for none, or names separated by spaces or commas.
// The result keeps the order of options.
func parseSelection(line string, options []string) ([]string, error) {
	line = strings.TrimSpace(line)
	switch strings.ToLower(line) {
	case "", "all":
		return slices.Clone(options), nil
	case "none", "-":
		return []string{}, nil
	}
	picked := map[string]bool{}
	for _, name := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if !slices.Contains(options, name) {
			return nil, output.NewError(output.CodeUsage, name+" is not one of "+strings.Join(options, ", "), output.ExitUsage, "")
		}
		picked[name] = true
	}
	out := []string{}
	for _, o := range options {
		if picked[o] {
			out = append(out, o)
		}
	}
	return out, nil
}

// protectTargets resolves the databases a protect request names: names
// (deduplicated, in the order given) or, with all, every one of databases.
// An unknown name is refused before anything changes.
func protectTargets(names []string, all bool, databases []string) ([]string, error) {
	if all && len(names) > 0 {
		return nil, output.NewError(output.CodeUsage, "pass database names or --all, not both", output.ExitUsage, "")
	}
	if all {
		return append([]string{}, databases...), nil
	}
	if len(names) == 0 {
		return nil, output.NewError(output.CodeUsage, "name at least one database, or pass --all", output.ExitUsage,
			"Run eru db list to see the databases.")
	}
	var out, unknown []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		switch {
		case name == "" || slices.Contains(out, name) || slices.Contains(unknown, name):
		case slices.Contains(databases, name):
			out = append(out, name)
		default:
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		noun := "database " + unknown[0] + " does not"
		if len(unknown) > 1 {
			noun = "databases " + strings.Join(unknown, ", ") + " do not"
		}
		return nil, output.NewError(output.CodePrecondition, noun+" exist", output.ExitPrecondition,
			"Run eru db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "databases": unknown})
	}
	return out, nil
}

// databaseNames lists the databases of cfg in cluster.toml order.
func databaseNames(cfg config.Cluster) []string {
	out := make([]string, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		out = append(out, c.Name)
	}
	return out
}

// splitProtected splits names into the ones cfg protects and the rest.
func splitProtected(cfg config.Cluster, names []string) (protected, unprotected []string) {
	for _, name := range names {
		if spec, ok := cfg.ClusterByName(name); ok && spec.Protected {
			protected = append(protected, name)
		} else {
			unprotected = append(unprotected, name)
		}
	}
	return protected, unprotected
}

// protectSet is the config.set keys that set clusters.<db>.protected to
// value for each of names, as eru config set does one at a time.
func protectSet(names []string, value bool) map[string]string {
	out := make(map[string]string, len(names))
	for _, name := range names {
		out["clusters."+name+".protected"] = strconv.FormatBool(value)
	}
	return out
}

// setProtected writes clusters.<db>.protected for names through config.set
// on a freshly read cluster.toml. code is the protect_code secret a lift
// needs. The node checks the flag live, so nothing is applied after.
func setProtected(cmd *cobra.Command, s *session, p *progress, names []string, value bool, code map[string]string) error {
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return err
	}
	_, err = p.call(commandContext(cmd), s.node, "config.set", map[string]any{
		"set":           protectSet(names, value),
		"base_revision": data.Revision,
	}, remote.CallOptions{DryRun: dryRun(cmd), Secrets: code, OnEvent: p.event})
	return err
}

func nonNil(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// runDBProtect protects the named databases, or every one with --all. It
// needs eru protect init first: a database is never protected without a
// way to lift it.
func runDBProtect(cmd *cobra.Command, args []string) error {
	all := flagTrue(cmd, "all")
	if _, err := protectTargets(args, all, args); err != nil {
		return err
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, _, err := remoteConfig(cmd, s, p)
	if err != nil {
		return err
	}
	targets, err := protectTargets(args, all, databaseNames(cfg))
	if err != nil {
		return err
	}
	already, todo := splitProtected(cfg, targets)
	data := map[string]any{"protected": nonNil(todo), "already_protected": nonNil(already)}
	if len(todo) == 0 {
		message := "there are no databases to protect"
		if len(already) > 0 {
			message = "already protected: " + strings.Join(already, ", ")
		}
		return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: s.warnings})
	}
	if err := setProtected(cmd, s, p, todo, true, nil); err != nil {
		return err
	}
	message := "protected " + strings.Join(todo, ", ") + ": db drop, a shrinking resize, credential rotation and restore promote/undo are refused from now on"
	if dryRun(cmd) {
		message = "dry run: would protect " + strings.Join(todo, ", ")
	}
	if len(already) > 0 {
		message += " (already protected: " + strings.Join(already, ", ") + ")"
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: s.warnings})
}

// runDBUnprotect lifts the protection of the named databases. It asks once
// for a current code from the authenticator app, for all of them, and
// refuses with --yes, without a terminal and in structured output, as
// eru config set clusters.<db>.protected false does.
func runDBUnprotect(cmd *cobra.Command, args []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, _, err := remoteConfig(cmd, s, p)
	if err != nil {
		return err
	}
	targets, err := protectTargets(args, false, databaseNames(cfg))
	if err != nil {
		return err
	}
	lift, notProtected := splitProtected(cfg, targets)
	data := map[string]any{"unprotected": nonNil(lift), "not_protected": nonNil(notProtected)}
	if len(lift) == 0 {
		return output.Success(cmd, output.Result{Data: data, Message: "not protected: " + strings.Join(notProtected, ", "), Warnings: s.warnings})
	}
	code, err := unprotectCode(cmd, s, p, lift)
	if err != nil {
		return err
	}
	if err := setProtected(cmd, s, p, lift, false, code); err != nil {
		return err
	}
	message := "lifted the protection of " + strings.Join(lift, ", ")
	if dryRun(cmd) {
		message = "dry run: would lift the protection of " + strings.Join(lift, ", ") + "; the real run asks for a code from the authenticator app"
	}
	if len(notProtected) > 0 {
		message += " (not protected: " + strings.Join(notProtected, ", ") + ")"
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: s.warnings,
		Next: []string{"eru db protect " + strings.Join(lift, " ")}})
}

// retryableCode reports whether err is a wrong or reused code, which a
// fresh code may fix; a lockout is not.
func retryableCode(err error) bool {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != output.CodeProtectCodeRejected {
		return false
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason == "code_invalid" || reason == "code_reused"
}

// showProtectSecret prints the otpauth URI as a QR code and the secret for
// typing in by hand, on stderr, outside the live area.
func showProtectSecret(cmd *cobra.Command, uri, secret string) error {
	code, err := ui.QR(uri)
	if err != nil {
		return err
	}
	resume := ui.Suspend()
	defer resume()
	_, err = fmt.Fprint(cmd.ErrOrStderr(),
		"Scan this QR code with an authenticator app (1Password, Aegis, Google Authenticator, ...):\n\n"+code+"\n"+
			"Or add it by hand: "+groupSecret(secret)+" (time-based, SHA1, 6 digits, 30 seconds)\n"+
			"The node keeps the secret root-only and does not show it again. Keep a backup of the authenticator entry:\n"+
			"without it, only root on the node can lift a database's protection.\n\n")
	return err
}

// groupSecret splits a base32 secret into groups of four for reading.
func groupSecret(secret string) string {
	var parts []string
	for len(secret) > 4 {
		parts = append(parts, secret[:4])
		secret = secret[4:]
	}
	return strings.Join(append(parts, secret), " ")
}

// readProtectCode asks for a 6-digit code without echo until one is well
// formed.
func readProtectCode(cmd *cobra.Command, label string) (string, error) {
	for attempt := 1; ; attempt++ {
		code, err := readSecretTTY(cmd, label)
		if err != nil {
			return "", err
		}
		if code = totp.NormalizeCode(code); totp.WellFormed(code) {
			return code, nil
		}
		if attempt >= protectCodeAttempts {
			return "", output.NewError(output.CodeProtectCodeRejected, "the code must be 6 digits", output.ExitUnsafe,
				"Type the 6-digit code the authenticator app shows for eru.").WithDetails(map[string]any{"reason": "code_invalid"})
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "The code is 6 digits.")
	}
}

func runProtectStatus(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchProtectStatus(cmd, s, p)
	if err != nil {
		return err
	}
	var message string
	switch {
	case data.Configured && data.ConfirmedAt != "":
		message = "two-factor protection is set up (since " + data.ConfirmedAt + ")"
	case data.Configured:
		message = "two-factor protection is set up"
	default:
		message = "two-factor protection is not set up"
	}
	if data.Pending {
		message += "; a new secret from eru protect init is waiting for its first code"
	}
	if data.LockedUntil != "" {
		message += "; codes are locked out until " + data.LockedUntil + " after too many wrong codes"
	} else if data.Failures > 0 {
		message += fmt.Sprintf("; %d wrong code(s) since the last accepted one", data.Failures)
	}
	message += "\n" + strings.Join(protectStatusLines(data.Databases), "\n")
	warnings := s.warnings
	var next []string
	if !data.Configured {
		next = []string{"eru protect init"}
		if len(data.Protected) > 0 {
			warnings = append(warnings, output.Warning{Code: "protect_not_initialized",
				Message: strings.Join(data.Protected, ", ") + " is protected but two-factor protection is not set up, so the protection cannot be lifted until a human runs eru protect init"})
		}
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings, Next: next})
}

// protectStatusLines is the database table of protect status: each
// database with protected yes or no.
func protectStatusLines(dbs []node.ProtectedDatabase) []string {
	if len(dbs) == 0 {
		return []string{"  no databases yet"}
	}
	width := len("DATABASE")
	for _, db := range dbs {
		width = max(width, len(db.Name))
	}
	lines := []string{fmt.Sprintf("  %-*s  %s", width, "DATABASE", "PROTECTED")}
	for _, db := range dbs {
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, db.Name, yesNo(db.Protected)))
	}
	return lines
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
