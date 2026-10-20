package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/tune"
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
	Name           string   `json:"name"`
	InConfig       bool     `json:"in_config"`
	Exists         bool     `json:"exists"`
	SizeBytes      int64    `json:"size_bytes"`
	Extensions     []string `json:"extensions"`
	Roles          []string `json:"roles"`
	Node           string   `json:"node,omitempty"`
	Host           string   `json:"host,omitempty"`
	Port           int      `json:"port,omitempty"`
	Stanza         string   `json:"stanza,omitempty"`
	Size           string   `json:"size,omitempty"`
	MemoryMiB      int      `json:"memory_mib,omitempty"`
	MemoryMaxMiB   int      `json:"memory_max_mib,omitempty"`
	CPUWeight      int      `json:"cpu_weight,omitempty"`
	CPUQuota       string   `json:"cpu_quota,omitempty"`
	IOWeight       int      `json:"io_weight,omitempty"`
	MaxConnections int      `json:"max_connections,omitempty"`
	Protected      bool     `json:"protected"`
	State          string   `json:"state"`
}

// limits is the database's limits in one column: cpu and io weights,
// connections, and the opt-in hard caps.
func (db databaseInfo) limits() string {
	if !db.InConfig {
		return "-"
	}
	out := fmt.Sprintf("cpu %d io %d conns %d", db.CPUWeight, db.IOWeight, db.MaxConnections)
	if db.CPUQuota != "" {
		out += " quota " + db.CPUQuota
	}
	if db.MemoryMaxMiB > 0 {
		out += " max " + tune.FormatMiB(db.MemoryMaxMiB)
	}
	return out
}

func runDBList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "db.list", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Databases []databaseInfo `json:"databases"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}
	lines := dbListLines(data.Databases)
	return output.Success(cmd, output.Result{
		Data:     data,
		Message:  strings.Join(lines, "\n"),
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

// dbListLines is the db list table: a header, then a row per database.
func dbListLines(dbs []databaseInfo) []string {
	const row = "  %-20s %-6s %-24s %-7s %-9s %-34s %-20s %-9s %s"
	lines := make([]string, 0, len(dbs)+1)
	if len(dbs) > 0 {
		lines = append(lines, fmt.Sprintf(row, "NAME", "NODE", "HOST", "SIZE", "MEMORY", "LIMITS", "STANZA", "PROTECTED", "STATE"))
	}
	for _, db := range dbs {
		memory, size, nodeName, host, stanza, protected := "-", "-", "-", "-", "-", "-"
		if db.InConfig {
			memory, size, nodeName, host, stanza, protected = tune.FormatMiB(db.MemoryMiB), db.Size, db.Node, db.Host, db.Stanza, yesNo(db.Protected)
		}
		lines = append(lines, fmt.Sprintf(row, db.Name, nodeName, host, size, memory, db.limits(), stanza, protected, db.State))
	}
	return lines
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
	db := config.ClusterSpec{Name: name, StatementTimeout: timeout, Extensions: extensions,
		Size: stringFlag(cmd, "size"), Memory: stringFlag(cmd, "memory"), Profile: stringFlag(cmd, "profile"), Protected: flagTrue(cmd, "protected")}
	if err := checkSizeFlags(cfg, db); err != nil {
		return err
	}
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
	service, creds, generated, err := serviceSecrets(s.ctx.Name)
	if err != nil {
		return err
	}
	for k, v := range service {
		rpc[k] = v
	}
	// With backups on, the new database's stanza needs the bucket keys and
	// cipher pass on the node; the kit rides along for a node without them.
	addRecoveryKit(rpc, creds.Backup)
	s.warnings = append(s.warnings, staleWarning(generated)...)
	params := map[string]any{
		"name":              name,
		"statement_timeout": timeout,
		"extensions":        db.Extensions,
		"size":              db.Size,
		"memory":            db.Memory,
		"profile":           db.Profile,
	}
	// Sent only when set, so a node without protection still takes the
	// request (it decodes params strictly).
	if db.Protected {
		params["protected"] = true
	}
	if flagTrue(cmd, "allow-overcommit") {
		params["allow_overcommit"] = true
	}
	reply, err := p.call(commandContext(cmd), s.node, "db.create", params, remote.CallOptions{Secrets: rpc, OnEvent: p.event})
	if err != nil {
		return createInterrupted(cmd, err, name, name+" ("+sizeSummary(cfg, db)+")", secret)
	}
	var created struct {
		Database config.ClusterSpec `json:"database"`
	}
	_ = reply.Decode(&created)
	summary := sizeSummary(cfg, created.Database)
	rich := "created database " + name + " and its roles"
	if created.Database.Protected {
		summary += ", protected"
		rich = "created protected database " + name + " and its roles"
	}
	message, human := credentialsMessage(cmd, fmt.Sprintf("created database %s (%s) and its roles", name, summary), rich, name+" ("+summary+")", secret)
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"database": secret, "cluster": created.Database},
		Message:  message,
		Human:    human,
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

// checkSizeFlags refuses a --size, --memory or --profile the node would
// refuse anyway, before any password is generated.
func checkSizeFlags(cfg config.Cluster, db config.ClusterSpec) error {
	if _, err := cfg.Resources(db); err != nil {
		return output.NewError(output.CodeValidation, err.Error(), output.ExitConfig,
			"Sizes are "+strings.Join(config.SizeNames(), ", ")+"; --memory takes an amount such as 768MiB or 2GiB, and custom needs it.")
	}
	if db.Profile != "" && !slices.Contains([]string{tune.ProfileOLTP, tune.ProfileMixed, tune.ProfileAnalytics}, db.Profile) {
		return output.NewError(output.CodeValidation, "profile must be oltp, mixed or analytics", output.ExitConfig, "")
	}
	return nil
}

// sizeSummary is "medium, 1 GiB" for spec.
func sizeSummary(cfg config.Cluster, spec config.ClusterSpec) string {
	res, err := cfg.Resources(spec)
	if err != nil {
		return spec.Size
	}
	return res.Size + ", " + tune.FormatMiB(res.MemoryMiB)
}

func runDBResize(cmd *cobra.Command, args []string) error {
	name := args[0]
	size, memory := stringFlag(cmd, "size"), stringFlag(cmd, "memory")
	if size == "" && memory == "" {
		return output.NewError(output.CodeUsage, "db resize needs --size or --memory", output.ExitUsage,
			"Sizes are "+strings.Join(config.SizeNames(), ", ")+"; --memory takes an amount such as 768MiB or 2GiB.")
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
	spec, ok := cfg.ClusterByName(name)
	if !ok {
		return output.NewError(output.CodePrecondition, "database "+name+" does not exist", output.ExitPrecondition,
			"Run vaire db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": name})
	}
	if size != "" {
		spec.Size, spec.Memory = size, ""
	}
	if memory != "" {
		spec.Memory = memory
	}
	if err := checkSizeFlags(cfg, spec); err != nil {
		return err
	}
	if old, _ := cfg.ClusterByName(name); old.Protected && node.ResizeShrinks(cfg, old, spec) {
		return node.CheckNotProtected(cfg, name, "shrinking it")
	}
	deferRestart := flagTrue(cmd, "defer-restart")
	confirm, confirms := confirmOptions(cmd)
	params := map[string]any{"name": name, "size": size, "memory": memory, "defer_restart": deferRestart}
	// Sent only when set, so an older node still takes the request.
	if flagTrue(cmd, "allow-overcommit") {
		params["allow_overcommit"] = true
	}
	reply, err := p.call(commandContext(cmd), s.node, "db.resize", params, remote.CallOptions{DryRun: dryRun(cmd), Confirm: confirm, Confirms: confirms, OnEvent: p.event})
	if err != nil {
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	var data struct {
		Database config.ClusterSpec `json:"database"`
		Plan     *engine.PlanData   `json:"plan,omitempty"`
		Apply    *engine.ApplyData  `json:"apply,omitempty"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}
	summary := sizeSummary(cfg, data.Database)
	if data.Plan != nil {
		return output.Success(cmd, output.Result{Data: data, Warnings: warnings,
			Message: fmt.Sprintf("dry run: would resize %s to %s; %d of %d steps would apply", name, summary, data.Plan.WouldApply, data.Plan.Total)})
	}
	message := fmt.Sprintf("resized %s to %s", name, summary)
	var next []string
	if a := data.Apply; a != nil {
		if a.Restarted() {
			message += "; restarted " + restartSummary(a.Restarts)
		}
		if len(a.RestartPending) > 0 {
			message += "; restart pending: " + strings.Join(a.RestartPending, ", ")
			next = []string{"vaire apply --db " + name + " " + confirmFlags(a.ConfirmTokens)}
		}
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings, Next: next})
}

// shownOnceNote follows every printed database password.
const shownOnceNote = "Store these now: vaire does not keep database passwords. If they are lost, run vaire db credentials <db> --rotate --role <role>."

func runDBDrop(cmd *cobra.Command, args []string) error {
	name := args[0]
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	// Refuse a protected database before asking for the token; the node
	// refuses it too.
	if cfg, _, err := remoteConfig(cmd, s, p); err == nil {
		if err := node.CheckNotProtected(cfg, name, "db drop"); err != nil {
			return err
		}
	}
	if !dryRun(cmd) {
		if err := Confirm(cmd, name); err != nil {
			return err
		}
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"database": name}, Message: "dry run: would drop " + name, Warnings: s.warnings})
	}
	reply, err := p.call(commandContext(cmd), s.node, "db.drop", map[string]any{"name": name, "no_final_dump": flagTrue(cmd, "no-final-dump")},
		remote.CallOptions{Confirm: name, OnEvent: p.event})
	if err != nil {
		return err
	}
	var dropped map[string]any
	_ = reply.Decode(&dropped)
	message := "dropped database " + name
	if stanza, ok := dropped["stanza_kept"].(string); ok {
		message += "; its stanza " + stanza + " is kept, so its backups stay restorable until they expire"
	}
	if final, ok := dropped["final_dump"].(map[string]any); ok {
		message += fmt.Sprintf("; its final dump %v is in the bucket (vaire dump get %s %v --out <file>)", final["id"], name, final["id"])
	}
	return output.Success(cmd, output.Result{
		Data:     dropped,
		Message:  message,
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

// runDBCredentials rotates one role password. vaire keeps no database
// passwords, so there is nothing to print without --rotate.
func runDBCredentials(cmd *cobra.Command, args []string) error {
	name := args[0]
	role := stringFlag(cmd, "role")
	if role == "" {
		role = steps.RoleApp
	}
	if !flagTrue(cmd, "rotate") {
		return output.NewError(output.CodeUsage, "vaire does not keep database passwords, so there is nothing to print", output.ExitUsage,
			"Rotate the role to get a new password: vaire db credentials "+name+" --rotate --role "+role+" --confirm "+name+". Applications using the old password stop connecting.")
	}
	if !slices.Contains(steps.DatabaseRoles, role) {
		return output.NewError(output.CodeValidation, "role must be migrate, app or ro", output.ExitConfig, "")
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
	if err := node.CheckNotProtected(cfg, name, "rotating its credentials"); err != nil {
		return err
	}
	if !dryRun(cmd) {
		if err := Confirm(cmd, name); err != nil {
			return err
		}
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]string{"database": name, "role": role}, Message: "dry run: would rotate " + name + " role " + role})
	}
	secret, rpc, err := newDatabaseSecret(cfg, name, []string{role})
	if err != nil {
		return err
	}
	if _, err := p.call(commandContext(cmd), s.node, "db.credentials", map[string]any{
		"name": name, "role": role, "rotate": true,
	}, remote.CallOptions{Secrets: rpc, Confirm: name, OnEvent: p.event}); err != nil {
		return err
	}
	message, human := credentialsMessage(cmd, "rotated "+name+" role "+role, "rotated "+name+" role "+role, name, secret)
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"database": secret},
		Message:  message,
		Human:    human,
		Warnings: s.warnings,
	})
}
