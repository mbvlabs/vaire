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
	// A command that takes several tokens declares its own repeatable
	// --confirm.
	if (spec.meta.Destructive || spec.meta.ConfirmToken != "") && cmd.Flags().Lookup("confirm") == nil {
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
			use:  "login [bundle-file|-]",
			args: cobra.MaximumNArgs(1),
			run:  runLogin,
			flags: []fl{
				flagString("name", "", "Context name (default: the server name in the document)"),
				flagBool("passphrase-stdin", "Read the passphrase from stdin"),
				flagString("passphrase-file", "", "Read the passphrase from a file"),
			},
			meta: CommandMeta{
				Summary:          "Create a local context from a transfer bundle",
				WhenToUse:        []string{"Open a server on a machine that has the transfer bundle and its passphrase from the secrets handoff."},
				Examples:         []string{"eru login", "eru login bundle.txt --passphrase-file pass.txt", "eru login - --passphrase-file pass.txt < bundle.txt"},
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
		serverCommands(),
		{
			use: "plan",
			run: runPlan,
			flags: []fl{
				flagString("db", "", "Only this database's cluster steps"),
			},
			meta: CommandMeta{
				Summary:   "Show the steps and file diffs an apply would run",
				WhenToUse: []string{"Review pending step changes before eru apply. Per-database steps show as <step>@<db>."},
				Examples:  []string{"eru plan", "eru plan --db app", "eru plan --output json"},
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
				flagStrings("step", "Run only these step ids: <step> for every database, <step>@<db> for one"),
				flagString("db", "", "Only this database's cluster steps"),
				flagStringArray("confirm", "Confirm restarts: <db>/restart for one database, {server}/restart for every one; repeatable. --yes never satisfies this check"),
			},
			meta: CommandMeta{
				Summary: "Converge the node to cluster.toml",
				WhenToUse: []string{
					"Apply pending steps after plan, a config change, or an upgrade.",
					"A restart a step needs waits for --confirm <db>/restart, or --confirm {server}/restart, which restarts every database one at a time.",
				},
				Examples: []string{
					"eru apply", "eru apply --dry-run", "eru apply --db app",
					"eru apply --step postgres-config-v1@app", "eru apply --confirm app/restart --confirm web/restart",
				},
				Mutating:         true,
				SupportsDryRun:   true,
				Prompts:          true,
				InteractiveFlags: []string{"--confirm", "--step"},
				Remote:           true,
				ConfirmToken:     "{db}/restart",
				NeedsRestart:     "maybe",
				ErrorCodes: []string{
					"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
					"node_version_mismatch", "precondition_failed", "step_failed", "unsafe_action_requires_confirmation",
					"validation_failed",
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
				ConfirmToken:     "{server}/stats",
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
				flagString("db", "", "The database whose PostgreSQL log to read; needed when there are several"),
			},
			meta: CommandMeta{
				Summary:   "Read systemd journal lines for an eru-managed unit",
				WhenToUse: []string{"Inspect Postgres (one database's cluster, postgresql@18-<db>), the transaction pooler (pgbouncer), the session pooler on 5432 (pgbouncer-session), nginx, certificate renewal, or eru run logs."},
				Examples:  []string{"eru logs postgres --db app", "eru logs pgbouncer --since 1h"},
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
			},
			meta: CommandMeta{
				Summary:          "Run SQL as eru_admin. Read-only unless --write is set",
				WhenToUse:        []string{"Inspect data or, with --write, change it. eru keeps no application database passwords, so it connects as eru_admin through the session pooler on 5432 of the node the database runs on, which routes by --db."},
				WhenNotToUse:     []string{"Interactive exploration; use eru psql."},
				Examples:         []string{"eru sql --db app \"select 1\"", "eru sql --db app --write \"alter role app_app set statement_timeout = '30s'\""},
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
				flagBool("write", "Allow statements that modify data"),
			},
			meta: CommandMeta{
				Summary:       "Open psql as eru_admin through the WireGuard tunnel, read-only unless --write",
				WhenToUse:     []string{"Use an interactive SQL session with verify-full TLS, through the session pooler on 5432, which routes by --db."},
				Prerequisites: []string{"psql 16 or newer on PATH"},
				Examples:      []string{"eru psql --db app", "eru psql --db app --write"},
			},
		},
		{
			use:  "console [db]",
			args: cobra.MaximumNArgs(1),
			run:  runConsole,
			flags: []fl{
				flagString("db", "", "Database name (or pass it as the argument)"),
				flagBool("write", "Allow statements that modify data"),
			},
			meta: CommandMeta{
				Summary:       "Open a usql console on a database as eru_admin through the WireGuard tunnel, read-only unless --write",
				WhenToUse:     []string{"Explore a database interactively without installing psql. eru verifies the server certificate itself."},
				WhenNotToUse:  []string{"Agents and scripts; use eru sql."},
				Prerequisites: []string{"usql on PATH, or let eru download its pinned copy"},
				Examples:      []string{"eru console app", "eru console app --write"},
			},
		},
		{
			use: "tunnel",
			run: runTunnel,
			flags: []fl{
				flagInt("local-port", 0, "Local listen port. 0 picks the default for the target"),
				flagString("target", "pgbouncer", "Forward to pgbouncer (the transaction pooler, 6432) or postgres (the session pooler, 5432)"),
				flagString("db", "", "Forward to the node this database runs on and put it in the DSN; default the master"),
			},
			meta: CommandMeta{
				Summary: "Forward a local port to the transaction or session pooler",
				WhenToUse: []string{
					"Point a GUI or library at 127.0.0.1 through the tunnel. Both poolers route by the database name in the DSN.",
					"Run migrations as <db>_migrate with --target postgres, the session pooler on 5432: the transaction pooler refuses the migrate role, and 5432 is closed to the internet unless network.expose_postgres is set.",
				},
				Examples: []string{"eru tunnel", "eru tunnel --db app --target postgres --local-port 6544"},
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
		dumpCommands(),
		jobCommands(),
		vpsCommands(),
		maintCommands(),
		{
			use: "secrets",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Show a replacement secrets handoff",
				WhenToUse: []string{"Rebuild the handoff and transfer bundle after credentials change."},
				Examples:  []string{"eru secrets export"},
			},
			children: []cmdSpec{{
				use: "export",
				run: runSecretsExport,
				flags: []fl{
					flagString("out", "", "Also write the armored transfer bundle here (required without a terminal)"),
					flagBool("reveal-passphrase", "Put the bundle passphrase in structured output (required without a terminal)"),
				},
				meta: CommandMeta{
					Summary:          "Rebuild the secrets handoff from the local context",
					WhenToUse:        []string{"Replace the stored secrets after a password rotation."},
					Examples:         []string{"eru secrets export", "eru secrets export --out bundle.txt --reveal-passphrase --output json"},
					Mutating:         true,
					Prompts:          true,
					InteractiveFlags: []string{"--out", "--reveal-passphrase"},
					SkipDryRunCheck:  true,
					SkipDryRunReason: "export builds a new transfer bundle and has no host step to preview",
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
					ConfirmToken:   "{server}/restart",
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
			Summary:   "List and switch local server contexts",
			WhenToUse: []string{"Choose which server later commands talk to."},
			Examples:  []string{"eru context list", "eru context use prod"},
		},
		children: []cmdSpec{
			{
				use: "list",
				run: runContextList,
				meta: CommandMeta{
					Summary:   "List local contexts",
					WhenToUse: []string{"See which servers this machine can operate."},
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
					WhenToUse:        []string{"Point later commands at a different server."},
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
					WhenToUse:        []string{"Remove operator credentials for a server from this machine."},
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

func serverCommands() cmdSpec {
	return cmdSpec{
		use: "server",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Set up a server",
			WhenToUse: []string{"Bootstrap a new server or rebuild one from a transfer bundle."},
			Examples:  []string{"eru server init --host root@203.0.113.10"},
			Remote:    true,
		},
		children: []cmdSpec{{
			use: "init",
			run: runServerInit,
			flags: []fl{
				flagString("host", "", "First-contact SSH target, user@host[:port] or a Host from ~/.ssh/config"),
				flagString("identity", "", "SSH private key for first contact (default: ~/.ssh/config IdentityFile, ssh-agent, ~/.ssh/id_*)"),
				flagBool("password-stdin", "Read the SSH password from stdin"),
				flagString("host-key-fingerprint", "", "Expected SHA256 host key fingerprint"),
				flagBool("accept-new-host-key", "Trust the server host key on first contact"),
				flagString("answers", "", "Answers file in cluster.toml shape plus bootstrap credentials"),
				flagString("name", "", "Server name"),
				flagString("node-name", "", "The master's node name; its hostname is <node-name>.<domain> (default db1)"),
				flagString("domain", "", "Base domain: each node is <node>.<domain>"),
				flagString("admin-user", "", "Admin login user created on the server (default eru)"),
				flagString("timezone", "", "Server timezone (default UTC)"),
				flagString("data-root", "", "Directory holding the PostgreSQL data directories (default /var/lib/postgresql/18)"),
				flagString("acme-email", "", "ACME account contact email (optional)"),
				flagString("profile", "", "Workload profile: oltp, mixed, or analytics"),
				flagString("default-size", "", "Default database size: nano, small, medium, large, or xlarge (default medium)"),
				flagInt("client-connections", 1000, "PgBouncer max client connections"),
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
				flagString("tls-mode", "", "acme or self-signed"),
				flagBool("local", "Initialise this machine as the server"),
				flagString("restore", "", "Transfer bundle to rebuild a server from (a file, or - for stdin)"),
				flagString("restore-time", "", "Point-in-time target for a disaster-recovery restore"),
				flagString("secrets-out", "", "Also write the armored transfer bundle here (required without a terminal)"),
				flagBool("reveal-passphrase", "Put the bundle passphrase in structured output (required without a terminal)"),
				flagBool("confirm-secrets-stored", "Confirm the secrets handoff was stored"),
				flagBool("dns-ready", "Skip the pause for creating the DNS records"),
				flagBool("skip-dns-check", "Deprecated: a DNS mismatch is only a warning now"),
				flagBool("resume", "Continue an unfinished init"),
				flagBool("discard", "Delete an unfinished init's local state (with --name)"),
			},
			meta: CommandMeta{
				Summary:   "Bootstrap a new server, this machine, or a rebuild from a backup",
				WhenToUse: []string{"Turn a fresh Debian or Ubuntu server into the central command, or rebuild one from a transfer bundle."},
				Examples: []string{
					"eru server init --host root@203.0.113.10 --answers answers.toml --no-input --confirm prod",
					"eru server init --local",
					"eru server init --discard --name prod",
				},
				Mutating:       true,
				SupportsDryRun: true,
				Prompts:        true,
				InteractiveFlags: []string{
					"--host",
					"--answers",
					"--name",
					"--node-name",
					"--domain",
					"--acme-email",
					"--tls-mode",
					"--admin-user",
					"--timezone",
					"--data-root",
					"--allow",
					"--profile",
					"--default-size",
					"--client-connections",
					"--backup-provider",
					"--backup-bucket",
					"--backup-key-id",
					"--backup-secret-file",
					"--secrets-out",
					"--reveal-passphrase",
					"--confirm-secrets-stored",
				},
				Remote:         true,
				ConfirmToken:   "{server}",
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
					Examples:  []string{"eru config get backup.vps.schedule", "eru config get clusters.app.memory"},
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
					Summary:   "Set one cluster.toml key",
					WhenToUse: []string{"Change a single setting without opening an editor."},
					Examples: []string{
						"eru config set postgres.settings.work_mem 64MB",
						"eru config set clusters.app.size large",
						"eru config set clusters.app.settings.work_mem 32MB --apply --dry-run",
						"eru config set clusters.app.backup.retention_full 1",
					},
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
			Summary:   "Create, list, resize, fork, drop, and rotate application databases",
			WhenToUse: []string{"Manage databases declared for the cluster."},
			Examples:  []string{"eru db list", "eru db create app"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "list",
				run: runDBList,
				meta: CommandMeta{
					Summary:   "List application databases with their node, host, size, limits, stanza and state",
					WhenToUse: []string{"See the databases eru manages: where each runs, its URL host, its size and limits, its pgBackRest stanza, and whether its cluster is running, stopped, pending or orphaned (a cluster on the server that cluster.toml doesn't list)."},
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
					flagString("size", "", "Size of the database's cluster: nano, small, medium, large, xlarge or custom (default [postgres] default_size, stepped down to what fits)"),
					flagString("memory", "", "Memory for the cluster, such as 768MiB or 2GiB; overrides the size's (custom needs it)"),
					flagString("profile", "", "Workload profile for its tuning: oltp, mixed or analytics (default [postgres] profile)"),
				},
				meta: CommandMeta{
					Summary:        "Create a database in its own PostgreSQL cluster, with its migrate, app, and read-only roles",
					WhenToUse:      []string{"Add an application database and persist it in cluster.toml. Each database is its own cluster with a size (memory, CPU weight, connections). Without --size or --memory, the default size steps down to the largest that fits the server's free budget (warning size_stepped_down); a server already full gets memory_overcommitted. A cluster that couldn't start is refused (would_not_start, with the smallest size that would). With backups on, it gets its own pgBackRest stanza, a first full backup and its backup timers, staggered from the other databases' (warning first_backup_failed when that fails; eru apply retries). The app and ro URLs use the transaction pooler on 6432; the migrate URL uses the session pooler on 5432, because the transaction pooler refuses the migrate role. Both route by the database name."},
					Examples:       []string{"eru db create app --size small", "eru db create app --memory 768MiB --profile oltp", "eru db create app --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
			{
				use:  "drop <name>",
				args: cobra.ExactArgs(1),
				run:  runDBDrop,
				flags: []fl{
					flagBool("no-final-dump", "Skip the final long-retention dump taken when backups are configured"),
				},
				meta: CommandMeta{
					Summary:        "Drop a database and its roles after a final dump to the bucket",
					WhenToUse:      []string{"Remove an application database the human has approved dropping. With backups on, a final age-encrypted dump goes to the bucket first. Its cluster is stopped and deleted with its data, roles, pooler entries and timers; its pgBackRest stanza is kept (data.stanza_kept) and recorded as dropped, so its backups stay restorable until they expire. Dropping the last database stops both poolers."},
					Examples:       []string{"eru db drop app --confirm app", "eru db drop app --dry-run"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}",
				},
			},
			{
				use:  "resize <db>",
				args: cobra.ExactArgs(1),
				run:  runDBResize,
				flags: []fl{
					flagString("size", "", "New size: nano, small, medium, large, xlarge or custom; drops a memory override unless --memory is given"),
					flagString("memory", "", "New memory for the cluster, such as 768MiB or 2GiB"),
					flagBool("defer-restart", "Apply what changes live now and leave the restart pending"),
					flagStringArray("confirm", "Confirm the restart: <db>/restart. --yes never satisfies this check"),
				},
				meta: CommandMeta{
					Summary:        "Change a database's size or memory",
					WhenToUse:      []string{"Give a database more or less memory, CPU weight and connections. The cgroup limits and reload-only settings change live; shared_buffers, max_connections and huge_pages need a restart of that database only, confirmed with --confirm <db>/restart, or left pending with --defer-restart (finish later with eru apply --db <db> --confirm <db>/restart). A size that couldn't start is refused (would_not_start); shrinking below current use warns (shrink_below_use); an overcommitted server warns (memory_overcommitted). eru config set clusters.<db>.size does the same change without applying it."},
					Examples:       []string{"eru db resize app --size large --confirm app/restart", "eru db resize app --memory 768MiB --defer-restart", "eru db resize app --size small --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}/restart",
					NeedsRestart:   "maybe",
				},
			},
			{
				use:  "fork <db>",
				args: cobra.ExactArgs(1),
				run:  runDBFork,
				flags: []fl{
					flagString("name", "", "New database name (default <db>_fork_<yyyymmddhhmm>)"),
					flagString("at", "", "Point in time to fork from: RFC 3339 or relative like -1h (default: the live database now)"),
				},
				meta: CommandMeta{
					Summary:        "Copy one database under a new name, now or at a point in time",
					WhenToUse:      []string{"Rehearse a migration on yesterday's data, or investigate a past state. The fork gets its own roles and passwords, printed once."},
					Examples:       []string{"eru db fork app", "eru db fork app --at 2026-10-09T12:00:00Z --name app_before"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     backupErrorCodes,
				},
			},
			{
				use:  "credentials <name>",
				args: cobra.ExactArgs(1),
				run:  runDBCredentials,
				flags: []fl{
					flagBool("rotate", "Generate a new password for --role (required: eru keeps no database passwords)"),
					flagString("role", "app", "Role to rotate: migrate, app, or ro"),
				},
				meta: CommandMeta{
					Summary:          "Rotate one role password and print the new credentials",
					WhenToUse:        []string{"A database password was lost or leaked: rotate it with --rotate --confirm <db>. eru does not keep database passwords."},
					Examples:         []string{"eru db credentials app --rotate --role app --confirm app"},
					MutatingFlags:    []string{"--rotate"},
					DestructiveFlags: []string{"--rotate"},
					SupportsDryRun:   true,
					Remote:           true,
					ConfirmToken:     "{db}",
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
					ConfirmToken:   "{server}/restart",
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
				flags: []fl{
					flagString("db", "", "The database whose cluster to show; needed when there are several"),
				},
				meta: CommandMeta{
					Summary:   "Print the server's budget sheet and the calculated settings",
					WhenToUse: []string{"See how the server's memory is split between databases (each one's size, limits, and what is allocated, free or overcommitted), whether IOWeight is enforced, and the values eru would write for a database's cluster. Oversubscription comes back as warnings."},
					Examples:  []string{"eru tune show", "eru tune show --db app"},
					Remote:    true,
				},
			},
			{
				use: "explain",
				run: runTuneExplain,
				flags: []fl{
					flagString("db", "", "The database whose cluster to explain; needed when there are several"),
				},
				meta: CommandMeta{
					Summary:   "Print tuning inputs and the reason for each setting",
					WhenToUse: []string{"Understand why a memory or WAL setting has its value."},
					Examples:  []string{"eru tune explain", "eru tune explain --db app"},
					Remote:    true,
				},
			},
			{
				use: "apply",
				run: runTuneApply,
				flags: []fl{
					flagStringArray("confirm", "Confirm restarts: <db>/restart for one database, {server}/restart for every one; repeatable. --yes never satisfies this check"),
				},
				meta: CommandMeta{
					Summary:        "Write calculated settings and restart when required",
					WhenToUse:      []string{"Install tuning after a resize. A restart needs --confirm <db>/restart, or {server}/restart for every database."},
					Examples:       []string{"eru tune apply --dry-run", "eru tune apply --confirm app/restart"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}/restart",
					NeedsRestart:   "maybe",
				},
			},
		},
	}
}

// backupErrorCodes are the codes the backup commands can return.
var backupErrorCodes = []string{
	"context_not_found", "remote_unreachable", "transport_failed", "node_version_mismatch",
	"precondition_failed", "external_command_failed", "input_required",
}

func backupCommands() cmdSpec {
	return cmdSpec{
		use: "backup",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Set up, take, list, and verify database backups",
			WhenToUse: []string{"Operate the pgBackRest repository. Backups cover every database; restores name one."},
			Examples:  []string{"eru backup setup", "eru backup list --db app", "eru backup now --type full"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "setup",
				run: runBackupSetup,
				flags: []fl{
					flagString("provider", "", "Provider preset: r2, s3, b2, hetzner, minio, or custom"),
					flagString("endpoint", "", "S3 endpoint URL (https://...)"),
					flagString("region", "", "Bucket region (R2: auto)"),
					flagString("bucket", "", "Bucket name"),
					flagString("prefix", "", "Key prefix inside the bucket (default: the cluster name)"),
					flagString("uri-style", "", "path or host (default from the provider)"),
					flagString("key-id", "", "Access key ID"),
					flagBool("secret-stdin", "Read the secret access key from stdin"),
					flagString("secret-file", "", "Read the secret access key from a file"),
					flagString("full-schedule", "", "systemd OnCalendar for full backups (default Sun *-*-* 01:00:00)"),
					flagString("diff-schedule", "", "systemd OnCalendar for differential backups"),
					flagString("incr-schedule", "", "systemd OnCalendar for incremental backups (empty: none)"),
					flagInt("retention-full", 0, "Restore window in weeks (default 4); extra full backups never shorten it"),
				},
				meta: CommandMeta{
					Summary: "Turn on backups to an S3-compatible bucket, or rotate its keys",
					WhenToUse: []string{
						"Add backups to a cluster: probes the bucket, generates the repository cipher and the dump key, writes [backup], switches every database's WAL archiving to the bucket with a reload (no restart) and takes a first full backup of each.",
						"Re-run with the same bucket to rotate the access keys.",
					},
					WhenNotToUse:     []string{"Do not use it to move backups to another bucket or prefix: that is refused, because the restore window would be lost."},
					Examples:         []string{"eru backup setup", "eru backup setup --provider r2 --endpoint https://<account>.r2.cloudflarestorage.com --bucket eru-backups --key-id KEY --secret-file key.txt"},
					Mutating:         true,
					SupportsDryRun:   true,
					Prompts:          true,
					InteractiveFlags: []string{"--provider", "--endpoint", "--region", "--bucket", "--prefix", "--key-id", "--secret-file"},
					Remote:           true,
					ErrorCodes:       append([]string{"backup_auth_failed", "bucket_not_found", "endpoint_unreachable", "step_failed"}, backupErrorCodes...),
					Next:             []string{"eru secrets export", "eru backup list"},
				},
			},
			{
				use: "now",
				run: runBackupNow,
				flags: []fl{
					flagString("type", "", "full, diff, or incr (default: incr, or full when the stanza has none)"),
					flagString("db", "", "Back up only this database (default: every database, one at a time)"),
				},
				meta: CommandMeta{
					Summary:        "Take a backup now, one detachable job per database",
					WhenToUse:      []string{"Take a backup outside the timers. Each database is its own cluster and stanza; without --db every database is backed up, one after another, because each job takes the server's heavy-I/O lock. A database whose job is skipped or fails doesn't stop the others; the command fails at the end. Ctrl-C detaches from the running job and starts no more; eru job follow re-attaches."},
					Examples:       []string{"eru backup now --db app --type full", "eru backup now", "eru backup now --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     backupErrorCodes,
					Next:           []string{"eru backup list", "eru job list"},
				},
			},
			{
				use: "list",
				run: runBackupList,
				flags: []fl{
					flagString("db", "", "Show this database's sets, window, verify, drill and dumps"),
				},
				meta: CommandMeta{
					Summary:   "Show the restore window, backup sets, verify, drill, and dumps per database",
					WhenToUse: []string{"Pick a restore target, or check that backups, verify and drills are current. Without --db: one row per database (window, sets, latest set, last verify, drill and dump), then the dropped databases whose stanzas are kept until they expire. --db: that database's sets, archive status, window and dumps; a dropped database still in its window is listed from its kept stanza."},
					Examples:  []string{"eru backup list", "eru backup list --db app --output json"},
					Remote:    true,
					Next:      []string{"eru restore <db> --time <ts>", "eru backup verify"},
				},
			},
			{
				use: "verify",
				run: runBackupVerify,
				flags: []fl{
					flagString("db", "", "Verify only this database's stanza (default: every database, one at a time)"),
				},
				meta: CommandMeta{
					Summary:          "Check every file of each database's stanza against its checksum, one detachable job per database",
					WhenToUse:        []string{"Prove the backups are readable. pgbackrest verify runs per stanza and downloads its backup files and WAL segments, so it can take a while; without --db every database is verified, one after another. Ctrl-C detaches from the running job and starts no more; eru job follow re-attaches. The result goes to records.json and eru.prom per database."},
					Examples:         []string{"eru backup verify --db app", "eru backup verify"},
					Mutating:         true,
					Remote:           true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "verify only records a check and does not change cluster data",
					ErrorCodes:       backupErrorCodes,
					Next:             []string{"eru job list"},
				},
			},
		},
	}
}

func jobCommands() cmdSpec {
	return cmdSpec{
		use: "job",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "List and follow detachable jobs",
			WhenToUse: []string{"Backups, restores, drills, forks and dumps run as jobs on the node that survive a closed laptop."},
			Examples:  []string{"eru job list", "eru job follow 01J..."},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "list",
				run: runJobList,
				meta: CommandMeta{
					Summary:   "Show running and recent jobs",
					WhenToUse: []string{"Find a job ID after detaching, or see what the timers ran."},
					Examples:  []string{"eru job list", "eru job list --output json"},
					Remote:    true,
					Next:      []string{"eru job follow <id>"},
				},
			},
			{
				use:  "follow <id>",
				args: cobra.ExactArgs(1),
				run:  runJobFollow,
				meta: CommandMeta{
					Summary:   "Re-attach to a job and wait for its result",
					WhenToUse: []string{"Watch a job started earlier; a finished job prints its log and result."},
					Examples:  []string{"eru job follow 01J9ZK3Q6V3W8Y2XH5N7T1C4BD"},
					Remote:    true,
				},
			},
		},
	}
}

func restoreCommands() cmdSpec {
	return cmdSpec{
		use:  "restore <db>",
		args: cobra.MaximumNArgs(1),
		run:  runRestore,
		flags: []fl{
			flagString("time", "", "Point in time: RFC 3339, or relative like -15m"),
			flagBool("latest", "Restore to the newest archived WAL"),
			flagString("set", "", "pgBackRest backup label"),
			flagString("dump", "", "Long-retention dump id, for points older than the restore window"),
			flagBool("promote", "Swap the restored copy in as soon as it is loaded"),
		},
		meta: CommandMeta{
			Summary: "Rewind one database while the others keep serving",
			WhenToUse: []string{
				"Restore one database to a time, the latest WAL, a backup set or a dump. It loads into <db>__restoring beside the live database, which keeps serving; inspect it, then promote (a few-second swap) or abort.",
			},
			WhenNotToUse: []string{"Do not use it to copy a database under a new name: use eru db fork. Rebuilding a whole server is server init --restore."},
			Examples: []string{
				"eru restore app --time 2026-10-09T12:00:00Z --confirm app/restore",
				"eru restore app --time -15m --promote --confirm app/restore",
				"eru restore app --dump 20260701T030000Z --confirm app/restore",
			},
			Mutating:         true,
			Destructive:      true,
			SupportsDryRun:   true,
			Prompts:          true,
			InteractiveFlags: []string{"--confirm"},
			Remote:           true,
			ConfirmToken:     "{db}/restore",
			ErrorCodes:       append([]string{"unsafe_action_requires_confirmation"}, backupErrorCodes...),
			Next:             []string{"eru restore status <db>", "eru restore promote <db>", "eru restore abort <db>"},
		},
		children: []cmdSpec{
			{
				use:  "status [db]",
				args: cobra.MaximumNArgs(1),
				run:  runRestoreStatus,
				meta: CommandMeta{
					Summary:   "Show open restores, row estimates against live, and leftovers",
					WhenToUse: []string{"Check a restore is ready and compare the restored copy with the live database before promoting."},
					Examples:  []string{"eru restore status", "eru restore status app"},
					Remote:    true,
				},
			},
			{
				use:  "promote <db>",
				args: cobra.ExactArgs(1),
				run:  runRestorePromote,
				meta: CommandMeta{
					Summary:        "Swap the restored copy in for the live database",
					WhenToUse:      []string{"After inspecting <db>__restoring: PgBouncer pauses, the databases swap names, PgBouncer resumes. Credentials don't change; the old copy is kept for undo."},
					Examples:       []string{"eru restore promote app --confirm app/restore"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}/restore",
					Next:           []string{"eru restore undo <db>", "eru restore cleanup <db>"},
				},
			},
			{
				use:  "undo <db>",
				args: cobra.ExactArgs(1),
				run:  runRestoreUndo,
				meta: CommandMeta{
					Summary:        "Swap the copy from before the last promote back in",
					WhenToUse:      []string{"A promoted restore was the wrong point in time and the leftover still exists."},
					Examples:       []string{"eru restore undo app --confirm app/restore"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}/restore",
				},
			},
			{
				use:  "abort <db>",
				args: cobra.ExactArgs(1),
				run:  runRestoreAbort,
				meta: CommandMeta{
					Summary:        "Drop <db>__restoring; the live database was never touched",
					WhenToUse:      []string{"Throw away a restored copy you don't want, or clean up after a failed restore."},
					Examples:       []string{"eru restore abort app"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
			{
				use:  "cleanup [db]",
				args: cobra.MaximumNArgs(1),
				run:  runRestoreCleanup,
				meta: CommandMeta{
					Summary:          "List restore leftovers, or drop those of one database",
					WhenToUse:        []string{"Free the space of <db>__pre_restore_* copies once the restore is good. Without a database it only lists."},
					Examples:         []string{"eru restore cleanup", "eru restore cleanup app --confirm app/cleanup"},
					Mutating:         true,
					Destructive:      true,
					SupportsDryRun:   true,
					Prompts:          true,
					InteractiveFlags: []string{"--confirm"},
					Remote:           true,
					ConfirmToken:     "{db}/cleanup",
				},
			},
			{
				use: "drill",
				run: runRestoreDrill,
				flags: []fl{
					flagString("time", "", "Optional point in time for the drill"),
				},
				meta: CommandMeta{
					Summary:        "Restore the whole cluster into scratch, check every database, tear it down",
					WhenToUse:      []string{"Prove the backups restore. The monthly timer runs it too; the result shows in eru backup list."},
					Examples:       []string{"eru restore drill", "eru restore drill --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     backupErrorCodes,
				},
			},
		},
	}
}

func dumpCommands() cmdSpec {
	return cmdSpec{
		use:  "dump <db>",
		args: cobra.MaximumNArgs(1),
		run:  runDump,
		flags: []fl{
			flagString("to", "local", "local (stream to this machine) or bucket (age-encrypted long-retention dump)"),
			flagString("out", "", "Local output path (default ./<db>-<time>.dump)"),
		},
		meta: CommandMeta{
			Summary:          "Write a pg_dump custom-format archive of one database",
			WhenToUse:        []string{"Take a logical copy now: to this machine, or into the bucket next to the scheduled long-retention dumps."},
			Examples:         []string{"eru dump app --out app.dump", "eru dump app --to bucket"},
			Mutating:         true,
			Remote:           true,
			SkipDryRunCheck:  true,
			SkipDryRunReason: "dump writes an archive and does not change the cluster",
			ErrorCodes:       backupErrorCodes,
			Next:             []string{"eru dump list <db>"},
		},
		children: []cmdSpec{
			{
				use:  "list [db]",
				args: cobra.MaximumNArgs(1),
				run:  runDumpList,
				meta: CommandMeta{
					Summary:   "List the long-retention dumps in the bucket",
					WhenToUse: []string{"Find a dump id for dump get or restore --dump. Reads the bucket with the recovery kit; the node is not involved."},
					Examples:  []string{"eru dump list", "eru dump list app"},
				},
			},
			{
				use:  "get <db> <id>",
				args: cobra.ExactArgs(2),
				run:  runDumpGet,
				flags: []fl{
					flagString("out", "", "Where to write the decrypted archive"),
				},
				meta: CommandMeta{
					Summary:          "Download and decrypt one dump on this machine",
					WhenToUse:        []string{"Get a dump older than the restore window as a plain pg_dump archive. The age identity never leaves this machine."},
					Examples:         []string{"eru dump get app 20260701T030000Z --out app-july.dump"},
					Mutating:         true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "get writes a local file and changes nothing on the cluster",
					Prompts:          true,
					InteractiveFlags: []string{"--out"},
					ErrorCodes:       []string{"input_required", "secrets_error", "backup_auth_failed", "bucket_not_found", "endpoint_unreachable", "precondition_failed"},
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
							WhenToUse:      []string{"Capture /etc, the ACME account, and operator homes before a risky change."},
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
					ConfirmToken:   "{server}/vps-restore",
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
					ConfirmToken:   "{server}/kill",
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
					ConfirmToken:     "{server}/rollback",
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
				flags: []fl{
					flagString("job-id", "", "Run the detachable job an RPC prepared under this id"),
					flagString("db", "", "Database of a backup, verify, drill or dump job (the timer's instance)"),
				},
				meta: CommandMeta{
					Summary:        "Run one timer job or detachable job",
					WhenToUse:      []string{"systemd timers and transient eru-job-<id> units call this: certs-renew, stanza-expiry (daily: deletes the stanzas of dropped databases once their window has passed), and per database (--db, from eru-<job>@<db>.timer) backup-full|diff|incr, backup-verify, restore-drill and db-dump; restore and db-fork run only with --job-id. Full, diff, verify, drill and dump take the server's heavy-I/O lock, so one runs at a time; a busy lock records the job as skipped."},
					Examples:       []string{"sudo eru node run certs-renew", "sudo eru node run backup-incr --db app", "sudo eru node run db-dump --db app", "sudo eru node run stanza-expiry"},
					Mutating:       true,
					SupportsDryRun: true,
					LinuxOnly:      true,
				},
			},
			{
				use:  "dump <db>",
				args: cobra.ExactArgs(1),
				run:  runNodeDump,
				meta: CommandMeta{
					Summary:        "Write pg_dump -Fc of one database to stdout",
					WhenToUse:      []string{"eru dump --to local runs this over SSH and streams the archive to the operator."},
					Examples:       []string{"sudo eru node dump app > app.dump"},
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
