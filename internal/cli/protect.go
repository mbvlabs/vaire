package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/totp"
	"github.com/spf13/cobra"
)

// protectStatusData is the protect.status result.
type protectStatusData struct {
	Protected       []string                   `json:"protected_databases"`
	ApprovalFactors *node.ExposureFactorStatus `json:"approval_factors"`
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

// protectCodeAttempts is the number of attempts to enter a well-formed code.
const protectCodeAttempts = 3

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
			"Run vaire db list to see the databases.")
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
			"Run vaire db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "databases": unknown})
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
// value for each of names, as vaire config set does one at a time.
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
	data, err := fetchConfig(commandContext(cmd), s, p)
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
// needs an approval factor: a database is never protected without a way to
// lift it.
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

	cfg, err := remoteConfig(cmd, s, p)
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
// vaire config set clusters.<db>.protected false does.
func runDBUnprotect(cmd *cobra.Command, args []string) error {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	cfg, err := remoteConfig(cmd, s, p)
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
		message = "dry run: would lift the protection of " + strings.Join(lift, ", ") + "; the real run asks for an approval factor"
	}

	if len(notProtected) > 0 {
		message += " (not protected: " + strings.Join(notProtected, ", ") + ")"
	}

	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: s.warnings,
		Next: []string{"vaire db protect " + strings.Join(lift, " ")}})
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
				"Type the 6-digit code the authenticator app shows for vaire.").WithDetails(map[string]any{"reason": "code_invalid"})
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

	message := "no approval factor is available to this operator"
	if hasApprovalFactor(data.ApprovalFactors) {
		message = "an approval factor is available to this operator"
	}

	message += "\n" + strings.Join(protectStatusLines(data.Databases), "\n")
	warnings := s.warnings

	var next []string
	if !hasApprovalFactor(data.ApprovalFactors) {
		next = []string{"vaire auth setup"}

		if len(data.Protected) > 0 {
			warnings = append(warnings, output.Warning{Code: "protect_not_initialized",
				Message: strings.Join(data.Protected, ", ") + " is protected but no approval factor is available to this operator. Run vaire auth setup to set one up."})
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
