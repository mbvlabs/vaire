package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/spf13/cobra"
)

// remoteConfig fetches cluster.toml from the node and parses it.
func remoteConfig(cmd *cobra.Command, s *session, p *progress) (config.Cluster, configData, error) {
	data, err := fetchConfig(cmd, s, p)
	if err != nil {
		return config.Cluster{}, configData{}, err
	}
	cfg, err := config.ParseCluster([]byte(data.TOML))
	if err != nil {
		return config.Cluster{}, data, err
	}
	return cfg, data, nil
}

type databaseInfo struct {
	Name       string   `json:"name"`
	InConfig   bool     `json:"in_config"`
	Exists     bool     `json:"exists"`
	SizeBytes  int64    `json:"size_bytes"`
	Extensions []string `json:"extensions"`
	Roles      []string `json:"roles"`
}

func runDBList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := s.node.Call(commandContext(cmd), "db.list", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Databases []databaseInfo `json:"databases"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}
	lines := make([]string, 0, len(data.Databases))
	for _, db := range data.Databases {
		state := "configured"
		if !db.InConfig {
			state = "orphaned"
		} else if !db.Exists {
			state = "pending"
		}
		lines = append(lines, fmt.Sprintf("  %-24s %-10s %s", db.Name, state, strings.Join(db.Extensions, ",")))
	}
	return output.Success(cmd, output.Result{
		Data:     data,
		Message:  strings.Join(lines, "\n"),
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

func runDBCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	if !config.DatabasePattern.MatchString(name) {
		hint := "Use lowercase letters, digits and underscores; the name is also the prefix of its roles."
		if fixed := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.ToLower(name)); config.DatabasePattern.MatchString(fixed) {
			hint = "Try " + fixed + ". Hyphens aren't allowed because the name is also the prefix of its roles."
		}
		return output.NewError(output.CodeValidation, "database name "+name+" must match "+config.DatabasePattern.String(), output.ExitConfig, hint).
			WithDetails(map[string]any{"name": name})
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
	timeout := stringFlag(cmd, "statement-timeout")
	if timeout == "" {
		timeout = "60s"
	}
	extensions, _ := cmd.Flags().GetStringSlice("extensions")
	db := config.Database{Name: name, StatementTimeout: timeout, Extensions: extensions}
	if len(extensions) == 0 {
		db.Extensions = []string{"pg_stat_statements"}
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]any{"database": db}, Message: "dry run: would create database " + name, Warnings: s.warnings})
	}
	secret, rpc, err := newDatabaseSecret(cfg, name, steps.DatabaseRoles)
	if err != nil {
		return err
	}
	reply, err := s.node.Call(commandContext(cmd), "db.create", map[string]any{
		"name":              name,
		"statement_timeout": timeout,
		"extensions":        db.Extensions,
	}, remote.CallOptions{Secrets: rpc, OnEvent: p.event})
	if err != nil {
		return err
	}
	message := fmt.Sprintf("created database %s and its roles\n%s\n%s", name, secret, shownOnceNote)
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"database": secret},
		Message:  message,
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

// shownOnceNote follows every printed database password.
const shownOnceNote = "Store these now: eru does not keep database passwords. If they are lost, run eru db credentials <db> --rotate --role <role>."

func runDBDrop(cmd *cobra.Command, args []string) error {
	name := args[0]
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if !dryRun(cmd) {
		if err := Confirm(cmd, name); err != nil {
			return err
		}
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"database": name}, Message: "dry run: would drop " + name, Warnings: s.warnings})
	}
	reply, err := s.node.Call(commandContext(cmd), "db.drop", map[string]any{"name": name, "no_final_dump": flagTrue(cmd, "no-final-dump")},
		remote.CallOptions{Confirm: name, OnEvent: p.event})
	if err != nil {
		return err
	}
	var dropped map[string]any
	_ = reply.Decode(&dropped)
	message := "dropped database " + name
	if final, ok := dropped["final_dump"].(map[string]any); ok {
		message += fmt.Sprintf("; its final dump %v is in the bucket (eru dump get %s %v --out <file>)", final["id"], name, final["id"])
	}
	return output.Success(cmd, output.Result{
		Data:     dropped,
		Message:  message,
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

// runDBCredentials rotates one role password. eru keeps no database
// passwords, so there is nothing to print without --rotate.
func runDBCredentials(cmd *cobra.Command, args []string) error {
	name := args[0]
	role := stringFlag(cmd, "role")
	if role == "" {
		role = steps.RoleApp
	}
	if !flagTrue(cmd, "rotate") {
		return output.NewError(output.CodeUsage, "eru does not keep database passwords, so there is nothing to print", output.ExitUsage,
			"Rotate the role to get a new password: eru db credentials "+name+" --rotate --role "+role+" --confirm "+name+". Applications using the old password stop connecting.")
	}
	if !slices.Contains(steps.DatabaseRoles, role) {
		return output.NewError(output.CodeValidation, "role must be migrate, app or ro", output.ExitConfig, "")
	}
	if !dryRun(cmd) {
		if err := Confirm(cmd, name); err != nil {
			return err
		}
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"database": name, "role": role}, Message: "dry run: would rotate " + name + " role " + role})
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
	secret, rpc, err := newDatabaseSecret(cfg, name, []string{role})
	if err != nil {
		return err
	}
	if _, err := s.node.Call(commandContext(cmd), "db.credentials", map[string]any{
		"name": name, "role": role, "rotate": true,
	}, remote.CallOptions{Secrets: rpc, Confirm: name, OnEvent: p.event}); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"database": secret},
		Message:  fmt.Sprintf("rotated %s role %s\n%s\n%s", name, role, secret, shownOnceNote),
		Warnings: s.warnings,
	})
}
