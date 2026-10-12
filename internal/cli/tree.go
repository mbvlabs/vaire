package cli

import (
	"github.com/spf13/cobra"
)

// cmdSpec is one node in the Phase 1 command tree.
// Commands that land in a later milestone use notImplemented until then.
type cmdSpec struct {
	use      string
	hidden   bool
	args     cobra.PositionalArgs
	run      func(cmd *cobra.Command, args []string) error
	flags    []fl
	meta     CommandMeta
	children []cmdSpec
}

func addSpecs(parent *cobra.Command, specs []cmdSpec) {
	for _, spec := range specs {
		addSpec(parent, spec)
	}
}

func addSpec(parent *cobra.Command, spec cmdSpec) {
	cmd := &cobra.Command{
		Use:           spec.use,
		Short:         spec.meta.Summary,
		Long:          spec.meta.Summary,
		Example:       joinLines(spec.meta.Examples),
		Hidden:        spec.hidden,
		Args:          spec.args,
		RunE:          spec.run,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	if cmd.Args == nil {
		cmd.Args = cobra.NoArgs
	}
	if cmd.RunE == nil {
		if len(spec.children) > 0 {
			cmd.RunE = parentRun
		} else {
			cmd.RunE = notImplemented
		}
	}
	if spec.meta.WhenToUse != nil {
		cmd.Long = joinLines(spec.meta.WhenToUse)
	}
	applyFlags(cmd, spec.flags)
	if spec.meta.SupportsDryRun {
		cmd.Flags().Bool("dry-run", false, "Report what would change and do not apply it")
	}
	if spec.meta.Destructive || spec.meta.ConfirmToken != "" {
		cmd.Flags().String("confirm", "", "Confirmation token. --yes never satisfies this check")
	}
	registerMeta(cmd, spec.meta)
	parent.AddCommand(cmd)
	addSpecs(cmd, spec.children)
}

func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}

func phase1Commands() []cmdSpec {
	return []cmdSpec{
		{
			use: "version",
			run: runVersion,
			meta: CommandMeta{
				Summary:   "Print the eru version",
				WhenToUse: []string{"Check which eru binary is running before an upgrade or a bug report."},
				Examples:  []string{"eru version", "eru version --output json"},
				Next:      []string{"eru commands --json"},
			},
		},
		{
			use:  "commands",
			run:  runCommands,
			args: cobra.NoArgs,
			flags: []fl{
				flagBool("check-meta", "Fail if any command is missing metadata"),
				flagString("path", "", "Show one command, such as \"backup list\""),
				flagHiddenBool("check-steps", "Validate the step catalogue and print its size"),
			},
			meta: CommandMeta{
				Summary:   "Show the command catalog and its safety metadata",
				WhenToUse: []string{"Discover commands, flags, and confirm tokens before choosing an action."},
				Examples:  []string{"eru commands --json", "eru commands --path \"backup list\"", "eru commands --check-meta"},
			},
		},
		{
			use: "guide",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Print operator and agent guides",
				WhenToUse: []string{"Read how to operate eru before running mutating commands."},
				Examples:  []string{"eru guide agents"},
				Next:      []string{"eru skill install"},
			},
			children: []cmdSpec{{
				use: "agents",
				run: runGuideAgents,
				meta: CommandMeta{
					Summary:   "Print the agent operating guide",
					WhenToUse: []string{"Load the rules for structured output, confirm tokens, and exit codes."},
					Examples:  []string{"eru guide agents", "eru guide agents --output json"},
					Next:      []string{"eru skill install", "eru commands --json"},
				},
			}},
		},
		{
			use: "skill",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Install the eru agent skill",
				WhenToUse: []string{"Copy the agent guide into a coding-agent harness."},
				Examples:  []string{"eru skill install --harness pi"},
			},
			children: []cmdSpec{{
				use: "install",
				run: runSkillInstall,
				flags: []fl{
					flagStrings("harness", "Harnesses to install: codex, claude, pi, opencode, crush"),
					flagString("dir", "", "Directory to install into (default: the current directory)"),
				},
				meta: CommandMeta{
					Summary:          "Write the eru skill into harness directories",
					WhenToUse:        []string{"Install the agent guide into Codex, Claude, Pi, OpenCode, or Crush."},
					Examples:         []string{"eru skill install --harness pi", "eru skill install --harness claude,pi --dir /srv/app --dry-run"},
					Mutating:         true,
					SupportsDryRun:   true,
					Prompts:          true,
					InteractiveFlags: []string{"--harness"},
					Next:             []string{"eru guide agents"},
				},
			}},
		},
		{
			use:  "login <secrets.age>",
			args: cobra.ExactArgs(1),
			run:  runLogin,
			flags: []fl{
				flagString("name", "", "Context name (default: the cluster name in the document)"),
				flagBool("passphrase-stdin", "Read the passphrase from stdin"),
				flagString("passphrase-file", "", "Read the passphrase from a file"),
			},
			meta: CommandMeta{
				Summary:          "Create a local context from a secrets document",
				WhenToUse:        []string{"Open a cluster on a machine that has the age secrets document."},
				Examples:         []string{"eru login eru-prod.secrets.age", "eru login eru-prod.secrets.age --passphrase-file /dev/stdin"},
				Mutating:         true,
				SkipDryRunCheck:  true,
				SkipDryRunReason: "login writes a local context from a secrets document and has no host change to preview",
				Prompts:          true,
				InteractiveFlags: []string{"--passphrase-stdin", "--passphrase-file"},
				ErrorCodes:       []string{"secrets_error", "precondition_failed", "input_required", "validation_failed"},
				Next:             []string{"eru plan", "eru context show"},
			},
		},
		contextCommands(),
		clusterCommands(),
		{
			use: "plan",
			run: runPlan,
			meta: CommandMeta{
				Summary:   "Show the steps and file diffs an apply would run",
				WhenToUse: []string{"Review pending step changes before eru apply."},
				Examples:  []string{"eru plan", "eru plan --output json"},
				Remote:    true,
				ErrorCodes: []string{
					"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
					"node_version_mismatch", "precondition_failed",
				},
				Next: []string{"eru apply"},
			},
		},
		{
			use: "apply",
			run: runApply,
			flags: []fl{
				flagStrings("step", "Run only these step ids"),
			},
			meta: CommandMeta{
				Summary:          "Converge the node to cluster.toml",
				WhenToUse:        []string{"Apply pending steps after plan, a config change, or an upgrade."},
				Examples:         []string{"eru apply", "eru apply --dry-run", "eru apply --step postgres-config-v1"},
				Mutating:         true,
				SupportsDryRun:   true,
				Prompts:          true,
				InteractiveFlags: []string{"--confirm", "--step"},
				Remote:           true,
				ConfirmToken:     "{cluster}/restart",
				NeedsRestart:     "maybe",
				ErrorCodes: []string{
					"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
					"node_version_mismatch", "precondition_failed", "step_failed", "unsafe_action_requires_confirmation",
				},
				Next: []string{"eru plan"},
			},
		},
		configCommands(),
		{
			use: "status",
			run: notImplemented,
			flags: []fl{
				flagBool("watch", "Refresh until interrupted"),
			},
			meta: CommandMeta{
				Summary:   "Show cluster health, backups, and capacity",
				WhenToUse: []string{"Check whether the cluster is serving traffic and backups are current."},
				Examples:  []string{"eru status", "eru status --output agent"},
				Remote:    true,
				Next:      []string{"eru doctor", "eru backup list"},
			},
		},
		{
			use: "doctor",
			run: notImplemented,
			flags: []fl{
				flagString("only", "", "Limit checks to security, backups, host, postgres, or tls"),
			},
			meta: CommandMeta{
				Summary:   "Run health checks and print a fix command when one exists",
				WhenToUse: []string{"Explain a failing or warning condition from status."},
				Examples:  []string{"eru doctor", "eru doctor --only backups"},
				Remote:    true,
				Next:      []string{"eru status"},
			},
		},
		{
			use: "top",
			run: notImplemented,
			meta: CommandMeta{
				Summary:   "Show live Postgres and PgBouncer activity",
				WhenToUse: []string{"Watch sessions, waits, and cache hit ratio on a live cluster."},
				Examples:  []string{"eru top"},
				Next:      []string{"eru queries", "eru maint report long-queries"},
			},
		},
		{
			use: "queries",
			run: notImplemented,
			flags: []fl{
				flagInt("limit", 20, "How many statements to return"),
				flagString("order", "total", "Sort by total, mean, or calls"),
				flagBool("reset", "Reset pg_stat_statements. Requires --confirm"),
			},
			meta: CommandMeta{
				Summary:          "Show statement statistics, or reset them",
				WhenToUse:        []string{"Find expensive statements. Pass --reset only to clear pg_stat_statements."},
				Examples:         []string{"eru queries --limit 20", "eru queries --reset --confirm prod/stats"},
				MutatingFlags:    []string{"--reset"},
				DestructiveFlags: []string{"--reset"},
				SkipDryRunCheck:  true,
				SkipDryRunReason: "--reset is the only mutation and requires a confirm token",
				ConfirmToken:     "{cluster}/stats",
				Next:             []string{"eru maint report missing-indexes"},
			},
		},
		{
			use:  "logs [unit]",
			args: cobra.MaximumNArgs(1),
			run:  runLogs,
			flags: []fl{
				flagString("since", "", "Journal window, such as 1h or a timestamp"),
				flagBool("follow", "Stream new lines"),
				flagInt("lines", 100, "How many lines to show"),
			},
			meta: CommandMeta{
				Summary:   "Read systemd journal lines for an eru-managed unit",
				WhenToUse: []string{"Inspect Postgres, PgBouncer, Caddy, or backup job logs."},
				Examples:  []string{"eru logs postgresql@18-main", "eru logs --since 1h"},
				Remote:    true,
			},
		},
		{
			use:  "sql [query]",
			args: cobra.MaximumNArgs(1),
			run:  runSQL,
			flags: []fl{
				flagString("db", "", "Database name"),
				flagString("file", "", "Read the query from a file"),
				flagBool("write", "Allow a statement that modifies data"),
				flagString("role", "ro", "Connect as ro, app, or admin"),
			},
			meta: CommandMeta{
				Summary:          "Run SQL. Read-only unless --write is set",
				WhenToUse:        []string{"Inspect data or, with --write, change it as the selected role."},
				WhenNotToUse:     []string{"Interactive exploration; use eru psql."},
				Examples:         []string{"eru sql \"select 1\"", "eru sql --write --role admin \"alter role app_app set statement_timeout = '30s'\""},
				Mutating:         true,
				SkipDryRunCheck:  true,
				SkipDryRunReason: "--write runs SQL immediately and is not a host step",
			},
		},
		{
			use: "psql",
			run: runPsql,
			flags: []fl{
				flagString("db", "", "Database name"),
				flagString("role", "ro", "Connect as ro, app, or admin"),
			},
			meta: CommandMeta{
				Summary:       "Open psql through the WireGuard tunnel",
				WhenToUse:     []string{"Use an interactive SQL session with verify-full TLS."},
				Prerequisites: []string{"psql 16 or newer on PATH"},
				Examples:      []string{"eru psql --db app --role app"},
			},
		},
		{
			use: "tunnel",
			run: runTunnel,
			flags: []fl{
				flagInt("local-port", 0, "Local listen port. 0 picks the default for the target"),
				flagString("target", "pgbouncer", "Forward to pgbouncer or postgres"),
			},
			meta: CommandMeta{
				Summary:   "Forward a local port to PgBouncer or Postgres",
				WhenToUse: []string{"Point a GUI or library at 127.0.0.1 through the tunnel."},
				Examples:  []string{"eru tunnel", "eru tunnel --target postgres --local-port 6544"},
			},
		},
		{
			use: "ssh",
			run: runSSH,
			flags: []fl{
				flagString("command", "", "Run one remote command instead of a shell"),
			},
			meta: CommandMeta{
				Summary:   "Open an SSH session to the node over WireGuard",
				WhenToUse: []string{"Reach the admin user when a task has no eru command yet."},
				Examples:  []string{"eru ssh", "eru ssh --command uptime"},
			},
		},
		dbCommands(),
		extCommands(),
		tuneCommands(),
		backupCommands(),
		restoreCommands(),
		{
			use:  "dump <db>",
			args: cobra.ExactArgs(1),
			run:  notImplemented,
			flags: []fl{
				flagString("to", "local", "Write to local or bucket"),
				flagString("out", "", "Local output path"),
			},
			meta: CommandMeta{
				Summary:          "Write a pg_dump custom-format archive of one database",
				WhenToUse:        []string{"Export one database for portability. Disaster recovery uses pgBackRest."},
				Examples:         []string{"eru dump app --out app.dump", "eru dump app --to bucket"},
				Mutating:         true,
				Remote:           true,
				SkipDryRunCheck:  true,
				SkipDryRunReason: "dump writes an archive and does not change the cluster",
			},
		},
		vpsCommands(),
		maintCommands(),
		{
			use: "secrets",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Export a replacement secrets document",
				WhenToUse: []string{"Rebuild the age document after credentials change."},
				Examples:  []string{"eru secrets export"},
			},
			children: []cmdSpec{{
				use: "export",
				run: runSecretsExport,
				flags: []fl{
					flagString("out", "", "Output path for the age file"),
					flagString("passphrase-out", "", "Write the new passphrase here, or - for stdout"),
				},
				meta: CommandMeta{
					Summary:          "Rebuild the secrets document from the local context",
					WhenToUse:        []string{"Replace a secrets document after a password rotation."},
					Examples:         []string{"eru secrets export --out eru-prod.secrets.age --passphrase-out -"},
					Mutating:         true,
					Prompts:          true,
					InteractiveFlags: []string{"--out", "--passphrase-out"},
					SkipDryRunCheck:  true,
					SkipDryRunReason: "export writes a new secrets document and has no host step to preview",
				},
			}},
		},
		{
			use: "wg",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Export the operator WireGuard configuration",
				WhenToUse: []string{"Install a system WireGuard interface instead of the userspace tunnel."},
				Examples:  []string{"eru wg export"},
			},
			children: []cmdSpec{{
				use: "export",
				run: notImplemented,
				flags: []fl{
					flagString("out", "", "Write the wg-quick config here"),
					flagBool("install", "Install the config with wireguard-tools and root"),
				},
				meta: CommandMeta{
					Summary:          "Write or install a system WireGuard config for this operator",
					WhenToUse:        []string{"Use the kernel WireGuard interface instead of the embedded userspace tunnel."},
					Examples:         []string{"eru wg export --out prod.conf", "eru wg export --install --dry-run"},
					Mutating:         true,
					SupportsDryRun:   true,
					Prompts:          true,
					InteractiveFlags: []string{"--out", "--install"},
					LinuxOnlyFlags:   []string{"--install"},
				},
			}},
		},
		{
			use: "upgrade",
			run: runUpgrade,
			flags: []fl{
				flagString("version", "", "Release version to install"),
				flagString("channel", "", "Release channel, stable or edge"),
				flagBool("skip-nodes", "Update only the local binary"),
			},
			meta: CommandMeta{
				Summary:          "Install a signed eru release and upgrade nodes",
				WhenToUse:        []string{"Move the operator CLI and its nodes to a newer signed release."},
				Examples:         []string{"eru upgrade", "eru upgrade --version 1.2.0 --dry-run"},
				Mutating:         true,
				SupportsDryRun:   true,
				Prompts:          true,
				InteractiveFlags: []string{"--version", "--channel", "--yes"},
				Remote:           true,
				ErrorCodes: []string{
					"remote_unreachable",
					"release_verification_failed",
					"validation_failed",
					"precondition_failed",
					"input_required",
				},
				Next: []string{"eru version"},
			},
		},
		{
			use: "pg",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Update PostgreSQL packages",
				WhenToUse: []string{"Install held PostgreSQL, PgBouncer, and pgBackRest package updates."},
				Examples:  []string{"eru pg update"},
			},
			children: []cmdSpec{{
				use: "update",
				run: notImplemented,
				flags: []fl{
					flagBool("now", "Restart Postgres immediately instead of waiting for the maintenance window"),
				},
				meta: CommandMeta{
					Summary:        "Upgrade held Postgres-related packages",
					WhenToUse:      []string{"Apply a minor package update after a VPS backup and an incremental database backup."},
					Examples:       []string{"eru pg update --dry-run", "eru pg update --now --confirm prod/restart"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/restart",
					NeedsRestart:   "maybe",
				},
			}},
		},
		nodeCommands(),
	}
}

func contextCommands() cmdSpec {
	return cmdSpec{
		use: "context",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "List and switch local cluster contexts",
			WhenToUse: []string{"Choose which cluster later commands talk to."},
			Examples:  []string{"eru context list", "eru context use prod"},
		},
		children: []cmdSpec{
			{
				use: "list",
				run: runContextList,
				meta: CommandMeta{
					Summary:   "List local contexts",
					WhenToUse: []string{"See which clusters this machine can operate."},
					Examples:  []string{"eru context list"},
				},
			},
			{
				use:  "show [name]",
				args: cobra.MaximumNArgs(1),
				run:  runContextShow,
				meta: CommandMeta{
					Summary:   "Show one context, or the current context",
					WhenToUse: []string{"Read addresses, keys' public parts, and the operator name for a context."},
					Examples:  []string{"eru context show", "eru context show prod"},
				},
			},
			{
				use:  "use <name>",
				args: cobra.ExactArgs(1),
				run:  runContextUse,
				meta: CommandMeta{
					Summary:          "Select the current context",
					WhenToUse:        []string{"Point later commands at a different cluster."},
					Examples:         []string{"eru context use prod"},
					Mutating:         true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "use switches the local current-context pointer",
				},
			},
			{
				use:  "remove <name>",
				args: cobra.ExactArgs(1),
				run:  runContextRemove,
				meta: CommandMeta{
					Summary:          "Delete a local context",
					WhenToUse:        []string{"Remove operator credentials for a cluster from this machine."},
					Examples:         []string{"eru context remove prod --confirm prod"},
					Mutating:         true,
					Destructive:      true,
					ConfirmToken:     "{name}",
					SkipDryRunCheck:  true,
					SkipDryRunReason: "remove deletes a local context and is guarded by the confirm token",
				},
			},
		},
	}
}

func clusterCommands() cmdSpec {
	return cmdSpec{
		use: "cluster",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Create a cluster",
			WhenToUse: []string{"Bootstrap a new node or rebuild one from a secrets document."},
			Examples:  []string{"eru cluster init --host root@203.0.113.10"},
			Remote:    true,
		},
		children: []cmdSpec{{
			use: "init",
			run: runClusterInit,
			flags: []fl{
				flagString("host", "", "First-contact SSH target, user@host[:port]"),
				flagString("identity", "", "SSH private key for first contact"),
				flagBool("password-stdin", "Read the SSH password from stdin"),
				flagString("host-key-fingerprint", "", "Expected SHA256 host key fingerprint"),
				flagBool("accept-new-host-key", "Trust the server host key on first contact"),
				flagString("answers", "", "Answers file in cluster.toml shape plus bootstrap credentials"),
				flagString("name", "", "Cluster name"),
				flagString("domain", "", "Client-facing FQDN"),
				flagString("admin-user", "", "Admin login user created on the server (default eru)"),
				flagString("timezone", "", "Server timezone (default UTC)"),
				flagString("data-directory", "", "PostgreSQL data directory (default /var/lib/postgresql/18/main)"),
				flagString("acme-email", "", "ACME account email"),
				flagString("profile", "", "Workload profile: oltp, mixed, or analytics"),
				flagInt("client-connections", 1000, "PgBouncer max client connections"),
				flagStrings("database", "Database to create (repeatable)"),
				flagStrings("allow", "Client CIDR allowed to reach the database ports (repeatable)"),
				flagString("backup-provider", "", "Backup provider preset: r2, s3, b2, hetzner, minio, or custom"),
				flagString("backup-endpoint", "", "S3-compatible endpoint URL"),
				flagString("backup-region", "", "Bucket region"),
				flagString("backup-bucket", "", "Bucket name"),
				flagString("backup-prefix", "", "Key prefix inside the bucket"),
				flagString("backup-key-id", "", "Bucket access key id"),
				flagBool("backup-secret-stdin", "Read the bucket secret from stdin"),
				flagString("backup-secret-file", "", "Read the bucket secret from a file"),
				flagString("vps-backup-schedule", "", "systemd OnCalendar for VPS backups"),
				flagString("db-full-schedule", "", "systemd OnCalendar for full database backups"),
				flagString("db-diff-schedule", "", "systemd OnCalendar for differential database backups"),
				flagStrings("extensions", "Extensions to enable (repeatable)"),
				flagString("tls-mode", "", "acme or self-signed"),
				flagBool("local", "Initialise the cluster on this machine"),
				flagString("restore", "", "Secrets document to rebuild a cluster from"),
				flagString("restore-time", "", "Point-in-time target for a disaster-recovery restore"),
				flagString("secrets-out", "", "Where to write the age secrets document"),
				flagString("passphrase-out", "", "Where to write the generated passphrase, or - for stdout"),
				flagBool("confirm-secrets-stored", "Confirm the secrets document and passphrase were stored"),
				flagBool("dns-ready", "Assert the DNS records already exist"),
				flagBool("skip-dns-check", "Continue even if the DNS records do not resolve yet"),
				flagBool("resume", "Continue an unfinished init"),
			},
			meta: CommandMeta{
				Summary:   "Bootstrap a cluster on a new server, locally, or from a backup",
				WhenToUse: []string{"Turn a fresh Debian or Ubuntu server into the central command, or rebuild one from a secrets document."},
				Examples: []string{
					"eru cluster init --host root@203.0.113.10 --answers answers.toml --no-input --confirm prod",
					"eru cluster init --local",
				},
				Mutating:       true,
				SupportsDryRun: true,
				Prompts:        true,
				InteractiveFlags: []string{
					"--host",
					"--answers",
					"--name",
					"--domain",
					"--acme-email",
					"--tls-mode",
					"--admin-user",
					"--timezone",
					"--data-directory",
					"--allow",
					"--profile",
					"--client-connections",
					"--database",
					"--backup-provider",
					"--backup-bucket",
					"--backup-key-id",
					"--backup-secret-file",
					"--secrets-out",
					"--passphrase-out",
					"--confirm-secrets-stored",
				},
				Remote:         true,
				ConfirmToken:   "{cluster}",
				LinuxOnlyFlags: []string{"--local"},
				ErrorCodes: []string{
					"input_required", "unsafe_action_requires_confirmation", "validation_failed", "remote_unreachable",
					"transport_failed", "host_key_mismatch", "node_version_mismatch", "precondition_failed", "step_failed",
					"lockdown_failed", "release_verification_failed",
				},
				Next: []string{"eru plan"},
			},
		}},
	}
}

func configCommands() cmdSpec {
	return cmdSpec{
		use: "config",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Show and edit cluster.toml",
			WhenToUse: []string{"Read or change desired state, then apply it."},
			Examples:  []string{"eru config show", "eru config set postgres.settings.work_mem 64MB"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "show",
				run: runConfigShow,
				meta: CommandMeta{
					Summary:   "Print cluster.toml",
					WhenToUse: []string{"Read the desired state stored on the node."},
					Examples:  []string{"eru config show"},
					Remote:    true,
				},
			},
			{
				use:  "get <key>",
				args: cobra.ExactArgs(1),
				run:  runConfigGet,
				meta: CommandMeta{
					Summary:   "Print one cluster.toml value",
					WhenToUse: []string{"Read a dotted key such as backup.vps.schedule."},
					Examples:  []string{"eru config get backup.vps.schedule"},
					Remote:    true,
				},
			},
			{
				use:  "set <key> <value>",
				args: cobra.ExactArgs(2),
				run:  runConfigSet,
				flags: []fl{
					flagBool("apply", "Apply the change after writing it"),
				},
				meta: CommandMeta{
					Summary:        "Set one cluster.toml key",
					WhenToUse:      []string{"Change a single setting without opening an editor."},
					Examples:       []string{"eru config set postgres.settings.work_mem 64MB", "eru config set postgres.settings.work_mem 64MB --apply --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					NeedsRestart:   "maybe",
					Next:           []string{"eru apply"},
				},
			},
			{
				use: "edit",
				run: runConfigEdit,
				flags: []fl{
					flagBool("apply", "Apply the configuration after the editor saves"),
				},
				meta: CommandMeta{
					Summary:          "Edit cluster.toml in a local editor",
					WhenToUse:        []string{"Change several settings together. Non-interactive edits use eru config set."},
					Examples:         []string{"eru config edit", "eru config edit --apply --dry-run"},
					Mutating:         true,
					SupportsDryRun:   true,
					Prompts:          true,
					InteractiveFlags: []string{"--apply"},
					Remote:           true,
					NeedsRestart:     "maybe",
				},
			},
			{
				use: "validate",
				run: runConfigValidate,
				meta: CommandMeta{
					Summary:   "Validate cluster.toml",
					WhenToUse: []string{"Check names, schedules, CIDRs, and the extension catalog before apply."},
					Examples:  []string{"eru config validate"},
					Remote:    true,
				},
			},
		},
	}
}

func dbCommands() cmdSpec {
	return cmdSpec{
		use: "db",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Create, drop, and rotate application databases",
			WhenToUse: []string{"Manage databases declared for the cluster."},
			Examples:  []string{"eru db list", "eru db create app"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "list",
				run: runDBList,
				meta: CommandMeta{
					Summary:   "List application databases",
					WhenToUse: []string{"See databases eru manages and their extensions."},
					Examples:  []string{"eru db list"},
					Remote:    true,
				},
			},
			{
				use:  "create <name>",
				args: cobra.ExactArgs(1),
				run:  runDBCreate,
				flags: []fl{
					flagString("statement-timeout", "", "statement_timeout for the app role"),
					flagStrings("extensions", "Extensions to create in the database"),
				},
				meta: CommandMeta{
					Summary:        "Create a database and its migrate, app, and read-only roles",
					WhenToUse:      []string{"Add an application database and persist it in cluster.toml."},
					Examples:       []string{"eru db create app --statement-timeout 60s", "eru db create app --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
			{
				use:  "drop <name>",
				args: cobra.ExactArgs(1),
				run:  runDBDrop,
				meta: CommandMeta{
					Summary:        "Drop a database and its roles after an incremental backup",
					WhenToUse:      []string{"Remove an application database the human has approved dropping."},
					Examples:       []string{"eru db drop app --confirm app", "eru db drop app --dry-run"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}",
				},
			},
			{
				use:  "credentials <name>",
				args: cobra.ExactArgs(1),
				run:  runDBCredentials,
				flags: []fl{
					flagBool("rotate", "Generate a new password for --role"),
					flagString("role", "app", "Role to print or rotate: migrate, app, or ro"),
				},
				meta: CommandMeta{
					Summary:          "Print connection credentials, or rotate one role password",
					WhenToUse:        []string{"Read a role password, or rotate it with --rotate --confirm <db>."},
					Examples:         []string{"eru db credentials app", "eru db credentials app --rotate --role app --confirm app"},
					MutatingFlags:    []string{"--rotate"},
					DestructiveFlags: []string{"--rotate"},
					SupportsDryRun:   true,
					Remote:           true,
					ConfirmToken:     "{db}",
					Next:             []string{"eru secrets export"},
				},
			},
		},
	}
}

func extCommands() cmdSpec {
	return cmdSpec{
		use: "ext",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "List and change PostgreSQL extensions",
			WhenToUse: []string{"Install a catalog extension into one or more databases."},
			Examples:  []string{"eru ext list", "eru ext add vector --db app"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "list",
				run: notImplemented,
				flags: []fl{
					flagString("db", "", "Limit the installed view to one database"),
				},
				meta: CommandMeta{
					Summary:   "List catalog extensions and where they are installed",
					WhenToUse: []string{"See available, installed, and outdated extensions."},
					Examples:  []string{"eru ext list", "eru ext list --db app"},
					Remote:    true,
				},
			},
			{
				use:  "add <name>",
				args: cobra.ExactArgs(1),
				run:  notImplemented,
				flags: []fl{
					flagString("db", "", "Database to install into (default: all declared databases)"),
				},
				meta: CommandMeta{
					Summary:        "Install an extension and persist it in cluster.toml",
					WhenToUse:      []string{"Add a catalog extension. Preload extensions need a Postgres restart."},
					Examples:       []string{"eru ext add vector --db app", "eru ext add pg_cron --confirm prod/restart"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/restart",
					NeedsRestart:   "maybe",
				},
			},
			{
				use:  "remove <name>",
				args: cobra.ExactArgs(1),
				run:  notImplemented,
				flags: []fl{
					flagString("db", "", "Database to drop the extension from"),
					flagBool("cascade", "Drop dependent objects too"),
				},
				meta: CommandMeta{
					Summary:        "Drop an extension from a database",
					WhenToUse:      []string{"Remove an extension after the human approves the database confirm token."},
					Examples:       []string{"eru ext remove vector --db app --confirm app"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}",
					NeedsRestart:   "maybe",
				},
			},
		},
	}
}

func tuneCommands() cmdSpec {
	return cmdSpec{
		use: "tune",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Show and apply hardware-derived Postgres settings",
			WhenToUse: []string{"Inspect or install the tuning calculator's settings."},
			Examples:  []string{"eru tune explain"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "show",
				run: runTuneShow,
				meta: CommandMeta{
					Summary:   "Print the calculated settings",
					WhenToUse: []string{"See the values eru would write for this host."},
					Examples:  []string{"eru tune show"},
					Remote:    true,
				},
			},
			{
				use: "explain",
				run: runTuneExplain,
				meta: CommandMeta{
					Summary:   "Print tuning inputs and the reason for each setting",
					WhenToUse: []string{"Understand why a memory or WAL setting has its value."},
					Examples:  []string{"eru tune explain"},
					Remote:    true,
				},
			},
			{
				use: "apply",
				run: runTuneApply,
				meta: CommandMeta{
					Summary:        "Write calculated settings and restart when required",
					WhenToUse:      []string{"Install tuning after a resize. A restart needs --confirm <cluster>/restart."},
					Examples:       []string{"eru tune apply --dry-run", "eru tune apply --confirm prod/restart"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/restart",
					NeedsRestart:   "maybe",
				},
			},
		},
	}
}

func backupCommands() cmdSpec {
	return cmdSpec{
		use: "backup",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Take, list, and verify pgBackRest backups",
			WhenToUse: []string{"Operate the database backup repository."},
			Examples:  []string{"eru backup list", "eru backup now --type full"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "now",
				run: notImplemented,
				flags: []fl{
					flagString("type", "", "full, diff, or incr"),
				},
				meta: CommandMeta{
					Summary:        "Run a pgBackRest backup now",
					WhenToUse:      []string{"Take a full, differential, or incremental backup outside the timer."},
					Examples:       []string{"eru backup now --type full", "eru backup now --type incr --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					Next:           []string{"eru backup verify", "eru backup list"},
				},
			},
			{
				use: "list",
				run: notImplemented,
				meta: CommandMeta{
					Summary:   "List backups and the PITR window",
					WhenToUse: []string{"Choose a restore target or check that backups exist."},
					Examples:  []string{"eru backup list"},
					Remote:    true,
					Next:      []string{"eru backup verify"},
				},
			},
			{
				use: "verify",
				run: notImplemented,
				meta: CommandMeta{
					Summary:          "Verify the latest backup set",
					WhenToUse:        []string{"Check that the repository can be read after a backup."},
					Examples:         []string{"eru backup verify"},
					Mutating:         true,
					Remote:           true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "verify records a check and does not change cluster data",
				},
			},
		},
	}
}

func restoreCommands() cmdSpec {
	return cmdSpec{
		use: "restore",
		run: notImplemented,
		flags: []fl{
			flagBool("latest", "Restore the latest backup and promote"),
			flagString("time", "", "Point-in-time target"),
			flagString("set", "", "pgBackRest backup label"),
			flagBool("promote", "Promote immediately after recovery pauses"),
			flagBool("delta", "Restore into the existing data directory"),
			flagString("db", "", "Restore one database into a new name instead of the whole cluster"),
		},
		meta: CommandMeta{
			Summary:          "Restore the cluster or a single database from pgBackRest",
			WhenToUse:        []string{"Recover to a backup, a timestamp, or a backup label. Whole-cluster restore pauses for inspection."},
			Examples:         []string{"eru restore --time 2026-10-09T12:00:00Z --confirm prod/restore", "eru restore --db app --time 2026-10-09T12:00:00Z"},
			Mutating:         true,
			Destructive:      true,
			SupportsDryRun:   true,
			Prompts:          true,
			InteractiveFlags: []string{"--latest", "--time", "--set", "--promote", "--db", "--confirm"},
			Remote:           true,
			ConfirmToken:     "{cluster}/restore",
			Next:             []string{"eru restore promote", "eru restore abort"},
		},
		children: []cmdSpec{
			{
				use: "status",
				run: notImplemented,
				meta: CommandMeta{
					Summary:   "Show an in-progress restore",
					WhenToUse: []string{"See whether recovery is paused, promoted, or idle."},
					Examples:  []string{"eru restore status"},
					Remote:    true,
				},
			},
			{
				use: "promote",
				run: notImplemented,
				meta: CommandMeta{
					Summary:        "Promote a paused restore and take a new full backup",
					WhenToUse:      []string{"Make a recovered cluster writable after inspection."},
					Examples:       []string{"eru restore promote --confirm prod/restore"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/restore",
				},
			},
			{
				use: "abort",
				run: notImplemented,
				meta: CommandMeta{
					Summary:        "Put the pre-restore data directory back",
					WhenToUse:      []string{"Cancel a paused restore that still has a pre-restore data directory."},
					Examples:       []string{"eru restore abort --confirm prod/restore"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/restore",
				},
			},
			{
				use: "cleanup",
				run: notImplemented,
				meta: CommandMeta{
					Summary:        "Delete data directories left behind by a restore",
					WhenToUse:      []string{"Remove restore leftovers after the human approves the deletion."},
					Examples:       []string{"eru restore cleanup --confirm prod/restore"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/restore",
				},
			},
			{
				use: "drill",
				run: notImplemented,
				flags: []fl{
					flagString("time", "", "Optional point-in-time target for the drill"),
				},
				meta: CommandMeta{
					Summary:        "Restore into a scratch directory and run checks",
					WhenToUse:      []string{"Prove that a backup can be opened without touching the live data directory."},
					Examples:       []string{"eru restore drill", "eru restore drill --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
		},
	}
}

func vpsCommands() cmdSpec {
	return cmdSpec{
		use: "vps",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Back up and restore the server filesystem with restic",
			WhenToUse: []string{"Operate the weekly VPS backup, which is separate from the database backups."},
			Examples:  []string{"eru vps backup list"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "backup",
				run: parentRun,
				meta: CommandMeta{
					Summary:   "Take or list VPS backups",
					WhenToUse: []string{"Run the restic backup of the server outside the timer."},
					Examples:  []string{"eru vps backup now"},
					Remote:    true,
				},
				children: []cmdSpec{
					{
						use: "now",
						run: notImplemented,
						meta: CommandMeta{
							Summary:        "Take a restic snapshot of the server",
							WhenToUse:      []string{"Capture /etc, Caddy state, and operator homes before a risky change."},
							Examples:       []string{"eru vps backup now", "eru vps backup now --dry-run"},
							Mutating:       true,
							SupportsDryRun: true,
							Remote:         true,
							Next:           []string{"eru vps backup list"},
						},
					},
					{
						use: "list",
						run: notImplemented,
						meta: CommandMeta{
							Summary:   "List VPS snapshots",
							WhenToUse: []string{"Choose a snapshot to restore."},
							Examples:  []string{"eru vps backup list"},
							Remote:    true,
						},
					},
				},
			},
			{
				use: "restore",
				run: notImplemented,
				flags: []fl{
					flagString("snapshot", "", "Restic snapshot id"),
					flagStrings("path", "Path to restore (repeatable)"),
					flagString("to", "", "Restore into this directory instead of the live path"),
					flagBool("in-place", "Restore over the live paths. Requires --confirm"),
				},
				meta: CommandMeta{
					Summary:        "Restore paths from a VPS snapshot",
					WhenToUse:      []string{"Copy files out of a snapshot, or restore them in place with a confirm token."},
					Examples:       []string{"eru vps restore --snapshot latest --path /etc/eru --to /tmp/eru-restore"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/vps-restore",
				},
			},
		},
	}
}

func maintCommands() cmdSpec {
	return cmdSpec{
		use: "maint",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Run maintenance and capacity reports",
			WhenToUse: []string{"Vacuum, reindex, repack, or report on bloat and wraparound."},
			Examples:  []string{"eru maint report bloat --db app"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "vacuum",
				run: notImplemented,
				flags: []fl{
					flagString("db", "", "Database, or all databases when omitted"),
					flagString("table", "", "Vacuum one table"),
					flagBool("analyze", "Run ANALYZE with the vacuum"),
					flagBool("freeze", "Freeze tuples"),
					flagInt("parallel", 0, "Parallel workers. 0 lets eru choose"),
				},
				meta: CommandMeta{
					Summary:        "Vacuum a table or every database",
					WhenToUse:      []string{"Reclaim space or freeze tuples outside the daily maintenance job."},
					Examples:       []string{"eru maint vacuum --db app --analyze", "eru maint vacuum --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
			{
				use: "analyze",
				run: notImplemented,
				flags: []fl{
					flagString("db", "", "Database, or all databases when omitted"),
					flagString("table", "", "Analyze one table"),
				},
				meta: CommandMeta{
					Summary:        "Refresh planner statistics",
					WhenToUse:      []string{"Update statistics after a large load."},
					Examples:       []string{"eru maint analyze --db app"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
			{
				use: "reindex",
				run: notImplemented,
				flags: []fl{
					flagString("db", "", "Database name"),
					flagString("table", "", "Reindex one table"),
					flagString("index", "", "Reindex one index"),
					flagBoolDefault("concurrently", true, "Rebuild indexes without a long lock"),
					flagBool("cleanup-invalid", "Drop leftover invalid _ccnew indexes"),
				},
				meta: CommandMeta{
					Summary:        "Rebuild indexes concurrently",
					WhenToUse:      []string{"Repair or compact indexes without blocking writes."},
					Examples:       []string{"eru maint reindex --db app --table events --concurrently"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
			{
				use: "repack",
				run: notImplemented,
				flags: []fl{
					flagString("db", "", "Database name"),
					flagString("table", "", "Table to repack"),
					flagBool("now", "Run outside the maintenance window"),
				},
				meta: CommandMeta{
					Summary:        "Repack a bloated table with pg_repack",
					WhenToUse:      []string{"Rewrite a table online after the pg_repack extension is installed."},
					Examples:       []string{"eru maint repack --db app --table events --confirm app"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}",
				},
			},
			{
				use:  "report <kind>",
				args: cobra.ExactArgs(1),
				run:  notImplemented,
				flags: []fl{
					flagString("db", "", "Database name"),
					flagInt("min-bytes", 0, "Minimum estimated bloat to include"),
					flagString("older-than", "5m", "Age threshold for long-query reports"),
				},
				meta: CommandMeta{
					Summary:   "Report bloat, wraparound, indexes, long queries, or locks",
					WhenToUse: []string{"Read a maintenance report. kind is bloat, wraparound, unused-indexes, missing-indexes, long-queries, or locks."},
					Examples:  []string{"eru maint report bloat --db app", "eru maint report wraparound"},
					Remote:    true,
				},
			},
			{
				use: "kill",
				run: notImplemented,
				flags: []fl{
					flagBool("idle-in-tx", "Terminate idle-in-transaction sessions"),
					flagInt("pid", 0, "Terminate this backend pid"),
					flagString("older-than", "", "Only sessions older than this duration"),
				},
				meta: CommandMeta{
					Summary:        "Terminate backends that match the filter",
					WhenToUse:      []string{"Stop a stuck session after the human approves the confirm token."},
					Examples:       []string{"eru maint kill --idle-in-tx --older-than 10m --confirm prod/kill"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{cluster}/kill",
				},
			},
		},
	}
}

func nodeCommands() cmdSpec {
	return cmdSpec{
		use:    "node",
		hidden: true,
		run:    parentRun,
		meta: CommandMeta{
			Summary:   "Node-side operations, hidden from top-level help",
			WhenToUse: []string{"Run the server half of eru. Operator commands call these over SSH."},
			Examples:  []string{"eru node version", "sudo eru node rpc"},
			LinuxOnly: true,
		},
		children: []cmdSpec{
			{
				use: "upgrade",
				run: notImplemented,
				flags: []fl{
					flagString("version", "", "Version to install on the node"),
					flagBool("rollback", "Restore the previous binary"),
				},
				meta: CommandMeta{
					Summary:          "Upgrade or roll back the node binary from the operator",
					WhenToUse:        []string{"Install a node release, or roll back with --rollback and a confirm token."},
					Examples:         []string{"eru node upgrade", "eru node upgrade --rollback --confirm prod/rollback"},
					Mutating:         true,
					DestructiveFlags: []string{"--rollback"},
					SupportsDryRun:   true,
					Remote:           true,
					ConfirmToken:     "{cluster}/rollback",
					NeedsRestart:     "maybe",
					Audience:         "operator",
				},
			},
			{
				use: "rpc",
				run: runNodeRPC,
				meta: CommandMeta{
					Summary:   "Read one RPC request on stdin and write NDJSON events",
					WhenToUse: []string{"The operator entry point. Timers and people use the other node commands."},
					WhenNotToUse: []string{
						"Do not call it by hand to change a node. Use the operator commands, which run plan first.",
					},
					Prerequisites: []string{
						"root, or sudo via /etc/sudoers.d/eru",
						"the request's client_version, client_channel and, for dev builds, client_commit match eru node version",
						"/etc/eru/cluster.toml for plan and apply",
					},
					Examples:         []string{"sudo eru node rpc < request.json"},
					Mutating:         true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "dry-run is the request's dry_run field, not a flag",
					ErrorCodes: []string{
						"usage_error",
						"cluster_not_initialized",
						"unsafe_action_requires_confirmation",
						"step_failed",
						"config_error",
						"validation_failed",
						"node_version_mismatch",
						"precondition_failed",
						"release_verification_failed",
					},
					LinuxOnly: true,
				},
			},
			{
				use: "version",
				run: runVersion,
				meta: CommandMeta{
					Summary:   "Print the node binary version",
					WhenToUse: []string{"The operator handshake compares this with its own version."},
					Examples:  []string{"eru node version --output json"},
					LinuxOnly: true,
				},
			},
			{
				use: "selfcheck",
				run: runNodeSelfcheck,
				meta: CommandMeta{
					Summary:       "Check that the node binary can read its config and tools",
					WhenToUse:     []string{"Run after a node upgrade, before apply."},
					Prerequisites: []string{"root"},
					Examples:      []string{"sudo eru node selfcheck", "sudo eru node selfcheck --output json"},
					ErrorCodes:    []string{"precondition_failed"},
					LinuxOnly:     true,
				},
			},
			{
				use:  "run <job>",
				args: cobra.ExactArgs(1),
				run:  runNodeRun,
				meta: CommandMeta{
					Summary:        "Run one timer job",
					WhenToUse:      []string{"systemd timers call this. Phase 2 implements certs-sync; other jobs arrive later."},
					Examples:       []string{"sudo eru node run certs-sync", "sudo eru node run metrics-refresh --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					LinuxOnly:      true,
				},
			},
			{
				use: "ssh-revert",
				run: runNodeSSHRevert,
				flags: []fl{
					flagBool("lockdown-only", "Restore the locked-down SSH rules instead of the pre-lockdown snapshot"),
				},
				meta: CommandMeta{
					Summary:          "Restore SSH and firewall rules from the commit-confirm snapshot",
					WhenToUse:        []string{"A transient timer calls this when lockdown was not confirmed."},
					Examples:         []string{"sudo eru node ssh-revert"},
					Mutating:         true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "ssh-revert is invoked by systemd and must restore the snapshot",
					LinuxOnly:        true,
				},
			},
			{
				use: "unlock-ssh",
				run: runNodeUnlockSSH,
				flags: []fl{
					flagInt("minutes", 30, "How long public SSH stays open"),
				},
				meta: CommandMeta{
					Summary:        "Open public SSH for a limited time from the provider console",
					WhenToUse:      []string{"Break glass when WireGuard is unreachable. Run as root on the server."},
					Examples:       []string{"sudo eru node unlock-ssh --minutes 30"},
					Mutating:       true,
					SupportsDryRun: true,
					LinuxOnly:      true,
				},
			},
		},
	}
}
