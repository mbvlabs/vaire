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
			flags: []fl{
				flagHiddenBool("licenses", "Print the third-party notices vaire embeds"),
			},
			meta: CommandMeta{
				Summary:   "Print the vaire version",
				WhenToUse: []string{"Check which vaire binary is running before an upgrade or a bug report."},
				Examples:  []string{"vaire version", "vaire version --output json"},
				Next:      []string{"vaire commands --json"},
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
				Examples:  []string{"vaire commands --json", "vaire commands --path \"backup list\"", "vaire commands --check-meta"},
			},
		},
		{
			use: "guide",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Print operator and agent guides",
				WhenToUse: []string{"Read how to operate vaire before running mutating commands."},
				Examples:  []string{"vaire guide agents"},
				Next:      []string{"vaire skill install"},
			},
			children: []cmdSpec{{
				use: "agents",
				run: runGuideAgents,
				meta: CommandMeta{
					Summary:   "Print the agent operating guide",
					WhenToUse: []string{"Load the rules for structured output, confirm tokens, and exit codes."},
					Examples:  []string{"vaire guide agents", "vaire guide agents --output json"},
					Next:      []string{"vaire skill install", "vaire commands --json"},
				},
			}},
		},
		{
			use: "skill",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Install the vaire agent skill",
				WhenToUse: []string{"Copy the agent guide into a coding-agent harness."},
				Examples:  []string{"vaire skill install --harness pi"},
			},
			children: []cmdSpec{{
				use: "install",
				run: runSkillInstall,
				flags: []fl{
					flagStrings("harness", "Harnesses to install: codex, claude, pi, opencode, crush"),
					flagString("dir", "", "Directory to install into (default: the current directory)"),
				},
				meta: CommandMeta{
					Summary:          "Write the vaire skill into harness directories",
					WhenToUse:        []string{"Install the agent guide into Codex, Claude, Pi, OpenCode, or Crush."},
					Examples:         []string{"vaire skill install --harness pi", "vaire skill install --harness claude,pi --dir /srv/app --dry-run"},
					Mutating:         true,
					SupportsDryRun:   true,
					Prompts:          true,
					InteractiveFlags: []string{"--harness"},
					Next:             []string{"vaire guide agents"},
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
				WhenToUse:        []string{"Open a server on a machine that has the transfer bundle and its passphrase from the secrets handoff.", "The file or stdin may hold the bundle alone or the whole copied handoff; vaire uses the armored block inside it. The passphrase is never read from the file."},
				Examples:         []string{"vaire login", "vaire login bundle.txt --passphrase-file pass.txt", "vaire login handoff.md --passphrase-file pass.txt", "vaire login - --passphrase-file pass.txt < bundle.txt"},
				Mutating:         true,
				SkipDryRunCheck:  true,
				SkipDryRunReason: "login writes a local context from a secrets document and has no host change to preview",
				Prompts:          true,
				InteractiveFlags: []string{"--passphrase-stdin", "--passphrase-file"},
				ErrorCodes:       []string{"secrets_error", "precondition_failed", "input_required", "validation_failed"},
				Next:             []string{"vaire plan", "vaire context show"},
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
				WhenToUse: []string{"Review pending step changes before vaire apply. Per-database steps show as <step>@<db>."},
				Examples:  []string{"vaire plan", "vaire plan --db app", "vaire plan --output json"},
				Remote:    true,
				ErrorCodes: []string{
					"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
					"node_version_mismatch", "precondition_failed",
				},
				Next: []string{"vaire apply"},
			},
		},
		{
			use: "apply",
			run: runApply,
			flags: []fl{
				flagStrings("step", "Run only these step ids: <step> for every database, <step>@<db> for one"),
				flagString("db", "", "Only this database's cluster steps"),
				flagBool("skip-doctor", "Skip the doctor checks after a full apply"),
				flagStringArray("confirm", "Confirm restarts: <db>/restart for one database, {server}/restart for every one; repeatable. --yes never satisfies this check"),
			},
			meta: CommandMeta{
				Summary: "Converge the node to cluster.toml",
				WhenToUse: []string{
					"Apply pending steps after plan, a config change, or an upgrade.",
					"A restart a step needs waits for --confirm <db>/restart, or --confirm {server}/restart, which restarts every database one at a time.",
				},
				Examples: []string{
					"vaire apply", "vaire apply --dry-run", "vaire apply --db app",
					"vaire apply --step postgres-config-v1@app", "vaire apply --confirm app/restart --confirm web/restart",
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
				Next: []string{"vaire plan"},
			},
		},
		configCommands(),
		{
			use: "status",
			run: runStatus,
			flags: []fl{
				flagBool("watch", "Refresh every 5 seconds until interrupted (terminal only)"),
				flagString("db", "", "Show one database"),
			},
			meta: CommandMeta{
				Summary: "Show the server summary and each database's health, backups and capacity",
				WhenToUse: []string{"Check whether the databases are serving, how much memory and how many connections each uses, " +
					"and when each was last backed up. It answers while an apply runs."},
				Examples:   []string{"vaire status", "vaire status --db app --output agent", "vaire status --watch"},
				Remote:     true,
				ErrorCodes: []string{"precondition_failed"},
				Next:       []string{"vaire doctor", "vaire backup list"},
			},
		},
		{
			use: "doctor",
			run: runDoctor,
			flags: []fl{
				flagString("only", "", "Limit checks to security, backups, host, postgres, tls or metrics"),
				flagString("db", "", "Limit the database checks to one database"),
			},
			meta: CommandMeta{
				Summary: "Run health checks and print a fix command when one exists",
				WhenToUse: []string{"Explain a failing or warning condition from status, or audit the server " +
					"(--only security is the CIS-style subset). Any failing check exits 5 with doctor_failed."},
				Examples:   []string{"vaire doctor", "vaire doctor --only backups", "vaire doctor --only security --json"},
				Remote:     true,
				ErrorCodes: []string{"doctor_failed", "precondition_failed"},
				Next:       []string{"vaire status"},
			},
		},
		{
			use: "top",
			run: runTop,
			flags: []fl{
				flagString("db", "", "Start filtered to one database"),
				flagBool("once", "Print one snapshot and exit"),
				flagBool("all-sessions", "Include internal/administrative sessions and statements, including postgres and maintenance roles"),
			},
			meta: CommandMeta{
				Summary: "Show live PostgreSQL and PgBouncer activity",
				WhenToUse: []string{"Watch sessions, waits, blocking chains and the busiest statements. It reads each database over the tunnel " +
					"through the session pooler as vaire_admin, read-only; --json or no terminal prints one snapshot. Internal sessions and statements are hidden by default, including postgres and maintenance roles (even manually issued work), and sessions whose application_name begins with vaire; --all-sessions shows them."},
				Examples: []string{"vaire top", "vaire top --db app", "vaire top --all-sessions", "vaire top --json"},
				Next:     []string{"vaire queries", "vaire maint report long-queries"},
			},
		},
		{
			use: "queries",
			run: runQueries,
			flags: []fl{
				flagString("db", "", "Database; every database when omitted"),
				flagInt("limit", 20, "How many statements to return"),
				flagString("order", "total", "Sort by total, mean, or calls"),
				flagBool("reset", "Reset pg_stat_statements. Requires --confirm"),
				flagBool("all-sessions", "Include statements run by internal/administrative roles, including postgres and maintenance roles"),
			},
			meta: CommandMeta{
				Summary:          "Show statement statistics, or reset them",
				WhenToUse:        []string{"Find expensive statements. Pass --reset only to clear pg_stat_statements: --confirm <db>/stats with --db, <server>/stats for every database. Statements run by internal/administrative roles (vaire sql, node setup, maintenance, the exporters) are hidden by default, including postgres and maintenance roles even for manually issued work; --all-sessions shows them. Application owner, migrate, app and read-only roles remain visible."},
				Examples:         []string{"vaire queries --db app --limit 20", "vaire queries --order mean", "vaire queries --all-sessions", "vaire queries --db app --reset --confirm app/stats"},
				Remote:           true,
				MutatingFlags:    []string{"--reset"},
				DestructiveFlags: []string{"--reset"},
				SkipDryRunCheck:  true,
				SkipDryRunReason: "--reset is the only mutation and requires a confirm token",
				ConfirmToken:     "{db}/stats",
				ErrorCodes:       []string{"precondition_failed", "unsafe_action_requires_confirmation"},
				Next:             []string{"vaire maint report missing-indexes"},
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
				Summary:   "Read systemd journal lines for an vaire-managed unit",
				WhenToUse: []string{"Inspect Postgres (one database's cluster, postgresql@18-<db>), the transaction pooler (pgbouncer), the session pooler on 5432 (pgbouncer-session), nginx, certificate renewal, or vaire run logs."},
				Examples:  []string{"vaire logs postgres --db app", "vaire logs pgbouncer --since 1h"},
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
				Summary: "Run SQL as vaire_admin. Read-only unless --write is set",
				WhenToUse: []string{"Inspect data or, with --write, change it. vaire keeps no application database passwords, so it connects as vaire_admin through the session pooler on 5432 of the node the database runs on, which routes by --db.",
					"Several statements separated by semicolons run in order in one transaction; human output prints each statement's rows or command tag, and --json keeps the last statement's columns, rows and command_tag at the top with every statement's result under results.",
					"On a protected database a read needs nothing; --write asks once per command, without echo, for the current 6-digit code from the authenticator app set up with vaire protect init, before it connects, and the node checks it and hands out a short-lived write login (protect_code_rejected when wrong, reused or locked out). With --yes, without a terminal or with structured output it is refused (database_protected, reason write_requires_human): agents get read-only access to a protected database."},
				WhenNotToUse:     []string{"Interactive exploration; use vaire psql.", "Agents: --write on a protected database. Ask the human to run it at their terminal."},
				Examples:         []string{"vaire sql --db app \"select 1\"", "vaire sql --db app \"select count(*) from users; select count(*) from orders\"", "vaire sql --db app --write \"alter role app_app set statement_timeout = '30s'\""},
				Mutating:         true,
				SkipDryRunCheck:  true,
				SkipDryRunReason: "--write runs SQL immediately and is not a host step",
				ErrorCodes:       []string{"database_protected", "protect_code_rejected"},
			},
		},
		{
			use:  "psql [-- psql-args...]",
			args: psqlArgs,
			run:  runPsql,
			flags: []fl{
				flagString("db", "", "Database name"),
				flagBool("write", "Allow statements that modify data"),
			},
			meta: CommandMeta{
				Summary: "Open psql as vaire_admin through the WireGuard tunnel, read-only unless --write",
				WhenToUse: []string{"Use an interactive SQL session with verify-full TLS, through the session pooler on 5432, which routes by --db.", "Arguments after -- go to psql as they are; -c before -- is vaire's --context.",
					"On a protected database --write asks once, when it starts and before psql connects, for the current 6-digit code from the authenticator app; the session stays writable for its whole length. Without a terminal, with --yes or structured output it is refused (database_protected, reason write_requires_human). Reads need no code."},
				Prerequisites: []string{"psql 16 or newer on PATH"},
				Examples:      []string{"vaire psql --db app", "vaire psql --db app --write", `vaire psql --db app -- -c "select 1"`},
				ErrorCodes:    []string{"database_protected", "protect_code_rejected"},
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
				Summary: "Open a usql console on a database as vaire_admin through the WireGuard tunnel, read-only unless --write",
				WhenToUse: []string{"Explore a database interactively without installing psql. vaire verifies the server certificate itself.",
					"On a protected database --write asks once, when it starts and before the console connects, for the current 6-digit code from the authenticator app; the session stays writable for its whole length. Without a terminal, with --yes or structured output it is refused (database_protected, reason write_requires_human). Reads need no code."},
				WhenNotToUse:  []string{"Agents and scripts; use vaire sql."},
				Prerequisites: []string{"usql on PATH, or let vaire download its pinned copy"},
				Examples:      []string{"vaire console app", "vaire console app --write"},
				ErrorCodes:    []string{"database_protected", "protect_code_rejected"},
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
				Examples: []string{"vaire tunnel", "vaire tunnel --db app --target postgres --local-port 6544"},
			},
			children: []cmdSpec{
				{
					use: "status",
					run: runTunnelStatus,
					meta: CommandMeta{
						Summary:    "Show the context's shared tunnel daemon",
						WhenToUse:  []string{"See whether the tunnel daemon runs, which vaire commands use it, its last WireGuard handshake, and when it exits idle."},
						Examples:   []string{"vaire tunnel status", "vaire tunnel status --output json"},
						ErrorCodes: []string{"transport_failed", "context_not_found"},
					},
				},
				{
					use: "stop",
					run: runTunnelStop,
					flags: []fl{
						flagBool("force", "Close the tunnel now, dropping the streams of running commands (they re-attach)"),
					},
					meta: CommandMeta{
						Summary: "Stop the context's shared tunnel daemon",
						WhenToUse: []string{
							"Restart the tunnel: without --force the daemon takes no new commands and exits once the running ones finish; the next command starts a new one.",
							"--force closes it now. Node work keeps running either way; job commands re-attach and other commands report stream_lost.",
						},
						Examples:         []string{"vaire tunnel stop", "vaire tunnel stop --force"},
						Next:             []string{"vaire tunnel status"},
						Mutating:         true,
						SkipDryRunCheck:  true,
						SkipDryRunReason: "stopping the local tunnel daemon changes nothing on the node",
						ErrorCodes:       []string{"transport_failed", "context_not_found"},
					},
				},
				{
					use:    "serve",
					hidden: true,
					run:    runTunnelServe,
					meta: CommandMeta{
						Summary:          "Run the context's shared tunnel daemon",
						WhenToUse:        []string{"vaire commands spawn this themselves; it owns the context's only WireGuard device and serves tunnel.sock."},
						WhenNotToUse:     []string{"Do not run it by hand; any remote command starts it."},
						Examples:         []string{"vaire tunnel serve --context prod"},
						Mutating:         true,
						SkipDryRunCheck:  true,
						SkipDryRunReason: "the daemon only proxies connections into the tunnel",
						ErrorCodes:       []string{"transport_failed", "context_not_found"},
					},
				},
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
				WhenToUse: []string{"Reach the admin user when a task has no vaire command yet.", "With stdout piped or redirected, --command runs without a PTY so the output stays clean; sudo then cannot ask for a password."},
				Examples:  []string{"vaire ssh", "vaire ssh --command uptime"},
			},
		},
		dbCommands(),
		protectCommands(),
		extCommands(),
		tuneCommands(),
		backupCommands(),
		restoreCommands(),
		dumpCommands(),
		jobCommands(),
		activityCommands(),
		diagnosticsCommands(),

		maintCommands(),
		metricsCommands(),
		{
			use: "secrets",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Show a replacement secrets handoff",
				WhenToUse: []string{"Rebuild the handoff and transfer bundle after credentials change."},
				Examples:  []string{"vaire secrets export"},
			},
			children: []cmdSpec{{
				use: "export",
				run: runSecretsExport,
				flags: []fl{
					flagString("out", "", "Also write the armored transfer bundle here (required without a terminal)"),
					flagBool("reveal-passphrase", "Put the bundle passphrase in structured output (required without a terminal)"),
				},
				meta: CommandMeta{
					Summary: "Rebuild the secrets handoff from the local context",
					WhenToUse: []string{"Replace the stored secrets after a password rotation.",
						"The whole document is shown, so a human factor is required: the master password set with vaire secrets password set, or a current code from the authenticator app that the node verifies. There is no flag or environment variable for either, so with --json, --no-input or no terminal it is refused (secret_gate_required) and an agent cannot export."},
					Examples:         []string{"vaire secrets export", "vaire secrets export --out bundle.txt --reveal-passphrase --output json"},
					Mutating:         true,
					Prompts:          true,
					InteractiveFlags: []string{"--out", "--reveal-passphrase"},
					SkipDryRunCheck:  true,
					SkipDryRunReason: "export builds a new transfer bundle and has no host step to preview",
					ErrorCodes:       []string{"secret_gate_required", "secrets_error"},
				},
			}, {
				use: "password",
				run: parentRun,
				meta: CommandMeta{
					Summary:   "Set the master password that gates secrets export",
					WhenToUse: []string{"Guard vaire secrets export and a new handoff with a password that lives only in the context as an argon2id hash."},
					Examples:  []string{"vaire secrets password set"},
				},
				children: []cmdSpec{{
					use: "set",
					run: runSecretsPasswordSet,
					meta: CommandMeta{
						Summary:          "Set or change the master password",
						WhenToUse:        []string{"Set a master password, or change it (the current password, or a code from the authenticator app, is required to change it). It is never taken from a flag and only its argon2id hash is stored (0600); vaire login carries the hash from the document to another machine. It prompts for the password at a terminal and is refused without one."},
						Examples:         []string{"vaire secrets password set"},
						Mutating:         true,
						SkipDryRunCheck:  true,
						SkipDryRunReason: "the password is never previewed or passed by flag",
						ErrorCodes:       []string{"secret_gate_required", "input_required"},
					},
				}},
			}},
		},
		{
			use: "wg",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Export the operator WireGuard configuration",
				WhenToUse: []string{"Install a system WireGuard interface instead of the userspace tunnel."},
				Examples:  []string{"vaire wg export"},
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
					Examples:         []string{"vaire wg export --out prod.conf", "vaire wg export --install --dry-run"},
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
				flagString("channel", "", "Release channel: stable, stable-<major>.x (for example stable-1.x) or edge"),
				flagBool("skip-nodes", "Update only the local binary"),
			},
			meta: CommandMeta{
				Summary:          "Install a signed vaire release and upgrade nodes",
				WhenToUse:        []string{"Move the operator CLI and its nodes to a newer signed release."},
				Examples:         []string{"vaire upgrade", "vaire upgrade --version 1.2.0 --dry-run"},
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
				Next: []string{"vaire version"},
			},
		},
		{
			use: "pg",
			run: parentRun,
			meta: CommandMeta{
				Summary:   "Update PostgreSQL packages",
				WhenToUse: []string{"Install held PostgreSQL, PgBouncer, and pgBackRest package updates."},
				Examples:  []string{"vaire pg update"},
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
					Examples:       []string{"vaire pg update --dry-run", "vaire pg update --now --confirm prod/restart"},
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
			Examples:  []string{"vaire context list", "vaire context use prod"},
		},
		children: []cmdSpec{
			{
				use: "list",
				run: runContextList,
				meta: CommandMeta{
					Summary:   "List local contexts",
					WhenToUse: []string{"See which servers this machine can operate."},
					Examples:  []string{"vaire context list"},
				},
			},
			{
				use:  "show [name]",
				args: cobra.MaximumNArgs(1),
				run:  runContextShow,
				meta: CommandMeta{
					Summary:   "Show one context, or the current context",
					WhenToUse: []string{"Read addresses, keys' public parts, and the operator name for a context."},
					Examples:  []string{"vaire context show", "vaire context show prod"},
				},
			},
			{
				use:  "use <name>",
				args: cobra.ExactArgs(1),
				run:  runContextUse,
				meta: CommandMeta{
					Summary:          "Select the current context",
					WhenToUse:        []string{"Point later commands at a different server."},
					Examples:         []string{"vaire context use prod"},
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
					Examples:         []string{"vaire context remove prod --confirm prod"},
					Mutating:         true,
					Destructive:      true,
					ConfirmToken:     "{name}",
					SkipDryRunCheck:  true,
					SkipDryRunReason: "remove deletes a local context and is guarded by the confirm token",
				},
			},
			{
				use:  "set-address <ip[:port]>",
				args: cobra.ExactArgs(1),
				run:  runContextSetAddress,
				flags: []fl{
					flagString("name", "", "The context (default: the current one)"),
				},
				meta: CommandMeta{
					Summary:        "Point a context at the server's new public address",
					WhenToUse:      []string{"After the server was rebuilt on a new IP (server init --restore on another machine). In acme mode vaire finds the new address in DNS by itself."},
					Examples:       []string{"vaire context set-address 198.51.100.7", "vaire context set-address '[2001:db8::7]:51820' --name prod"},
					Mutating:       true,
					SupportsDryRun: true,
					Next:           []string{"vaire status"},
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
			Examples:  []string{"vaire server init --host root@203.0.113.10"},
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
				flagString("admin-user", "", "Admin login user created on the server (default vaire)"),
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
				flagString("db-full-schedule", "", "systemd OnCalendar for full database backups"),
				flagString("db-diff-schedule", "", "systemd OnCalendar for differential database backups"),
				flagString("tls-mode", "", "acme or self-signed"),
				flagBool("local", "Initialise this machine as the server"),
				flagBool("restore", "Rebuild this context's server on a new VPS from its master bundle (Phase 5)"),
				flagString("bundle", "", "With --restore: a bundle name or a time (default: the newest in the bucket)"),
				flagString("bundle-file", "", "With --restore: rebuild from an exported bundle file (- for stdin) instead of the bucket"),
				flagBool("old-master-unreachable", "With --restore: skip the check that the old master no longer answers"),
				flagString("secrets-out", "", "Also write the armored transfer bundle here (required without a terminal)"),
				flagBool("reveal-passphrase", "Put the bundle passphrase in structured output (required without a terminal)"),
				flagBool("confirm-secrets-stored", "Confirm the secrets handoff was stored"),
				flagBool("dns-ready", "Skip the pause for creating the DNS records"),
				flagBool("skip-dns-check", "Deprecated: a DNS mismatch is only a warning now"),
				flagBool("skip-doctor", "Skip the doctor checks that gate init after the database phase"),
				flagBool("resume", "Continue an unfinished init"),
				flagBool("discard", "Delete an unfinished init's local state (with --name)"),
			},
			meta: CommandMeta{
				Summary:   "Bootstrap a new server, this machine, or a rebuild from a backup",
				WhenToUse: []string{"Turn a fresh Debian or Ubuntu server into the central command, or rebuild one from a transfer bundle."},
				Examples: []string{
					"vaire server init --host root@203.0.113.10 --answers answers.toml --no-input --confirm prod",
					"vaire server init --local",
					"vaire server init --discard --name prod",
					"vaire server init --restore --host root@198.51.100.7 --confirm prod/replace",
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
					"lockdown_failed", "release_verification_failed", "bundle_corrupt", "bundle_not_found",
				},
				Next: []string{"vaire plan", "vaire db recover --all"},
			},
		}, serverBackupCommands()},
	}
}

func configCommands() cmdSpec {
	return cmdSpec{
		use: "config",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Show and edit cluster.toml",
			WhenToUse: []string{"Read or change desired state, then apply it."},
			Examples:  []string{"vaire config show", "vaire config set postgres.settings.work_mem 64MB"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "show",
				run: runConfigShow,
				meta: CommandMeta{
					Summary:   "Print cluster.toml",
					WhenToUse: []string{"Read the desired state stored on the node."},
					Examples:  []string{"vaire config show"},
					Remote:    true,
				},
			},
			{
				use:  "get <key>",
				args: cobra.ExactArgs(1),
				run:  runConfigGet,
				meta: CommandMeta{
					Summary:   "Print one cluster.toml value",
					WhenToUse: []string{"Read a dotted key such as clusters.app.memory."},
					Examples:  []string{"vaire config get clusters.app.memory"},
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
					WhenToUse: []string{"Change a single setting without opening an editor.", "clusters.<db>.protected true protects a database; it is refused until vaire protect init has set up two-factor protection (precondition_failed, reason protect_not_initialized). Setting it false, or removing a protected entry, asks for the current 6-digit code from the authenticator app, typed without echo at an interactive terminal; the node checks it (protect_code_rejected when wrong, reused or locked out after 5 wrong codes). With --yes, without a terminal or with structured output it is refused (database_protected), so an agent cannot lift it. vaire db protect and vaire db unprotect make the same change for one or more databases."},
					Examples: []string{
						"vaire config set postgres.settings.work_mem 64MB",
						"vaire config set clusters.app.size large",
						"vaire config set clusters.app.settings.work_mem 32MB --apply --dry-run",
						"vaire config set clusters.app.backup.retention_full 1",
						"vaire config set clusters.app.protected true",
					},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					NeedsRestart:   "maybe",
					Next:           []string{"vaire apply"},
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
					WhenToUse:        []string{"Change several settings together. Non-interactive edits use vaire config set."},
					Examples:         []string{"vaire config edit", "vaire config edit --apply --dry-run"},
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
					Examples:  []string{"vaire config validate"},
					Remote:    true,
				},
			},
		},
	}
}

func protectCommands() cmdSpec {
	return cmdSpec{
		use: "protect",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Set up the two-factor code that lifts a database's protection, and protect databases",
			WhenToUse: []string{"Protected databases (clusters.<db>.protected) refuse db drop, a shrinking resize, credential rotation and restore promote/undo. Lifting the protection asks for a TOTP code from an authenticator app; vaire protect init sets that up and protects the databases in the same run. vaire db protect and vaire db unprotect change it later."},
			Examples:  []string{"vaire protect init", "vaire protect init --all", "vaire protect status"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "init",
				run: runProtectInit,
				flags: []fl{
					flagBool("rotate", "Replace the existing secret; asks for a current code from it first"),
					flagStringArray("db", "Protect this database once the code is confirmed, without asking; repeatable"),
					flagBool("all", "Protect every database once the code is confirmed, without asking"),
				},
				meta: CommandMeta{
					Summary: "Set up the TOTP secret that lifting a database's protection asks a code from, and protect databases",
					WhenToUse: []string{
						"Once per cluster, by a human, before the first database is protected: the node makes a 160-bit secret and keeps it root-only, this shows it once as a QR code (and as base32), and a code from the authenticator app confirms it before the node uses it. Interactive only: without a terminal, with --yes or with structured output it is refused (input_required).",
						"Once the code is confirmed it protects databases in the same run: the ones --db names (repeatable) or every one with --all, or else it asks which of the unprotected databases to protect, all of them picked to start with. An unknown --db name is refused before the QR code is shown. The result names the protected databases (data.protected_databases, data.newly_protected) and gives vaire db protect with the names of any left unprotected. With no databases yet, create them protected with vaire db create --protected.",
						"With a secret already set up it is refused (precondition_failed, reason protect_already_initialized) unless --rotate, which asks for a current code from the old secret first; the old one keeps working until the new one is confirmed.",
					},
					Examples:         []string{"vaire protect init", "vaire protect init --all", "vaire protect init --db app --db billing", "vaire protect init --rotate"},
					Mutating:         true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "init is interactive and only writes the node's TOTP secret, which has nothing to preview",
					Remote:           true,
					ErrorCodes:       []string{"input_required", "precondition_failed", "protect_code_rejected"},
				},
			},
			{
				use: "status",
				run: runProtectStatus,
				meta: CommandMeta{
					Summary:   "Show whether two-factor protection is set up, any lockout, and each database's protection",
					WhenToUse: []string{"Check before protecting a database or lifting a protection: it lists every database with protected yes or no (data.databases). Five wrong codes in a row lock codes out for 15 minutes; status shows until when."},
					Examples:  []string{"vaire protect status"},
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
			Summary:   "Create, list, resize, fork, drop, protect, and rotate application databases",
			WhenToUse: []string{"Manage databases declared for the cluster."},
			Examples:  []string{"vaire db list", "vaire db create app"},
			Remote:    true,
		},
		children: []cmdSpec{
			dbRecoverCommand(),
			{
				use: "list",
				run: runDBList,
				meta: CommandMeta{
					Summary:   "List application databases with their node, host, size, limits, stanza, protection and state",
					WhenToUse: []string{"See the databases vaire manages: where each runs, its URL host, its size and limits, its pgBackRest stanza, whether it is protected (PROTECTED, data.databases[].protected), and whether its cluster is running, stopped, pending or orphaned (a cluster on the server that cluster.toml doesn't list)."},
					Examples:  []string{"vaire db list"},
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
					flagString("size", "", "Size of the database's cluster: nano, small, medium, large, xlarge or custom (default [postgres] default_size)"),
					flagString("memory", "", "Memory for the cluster, such as 768MiB or 2GiB; overrides the size's (custom needs it)"),
					flagString("profile", "", "Workload profile for its tuning: oltp, mixed or analytics (default [postgres] profile)"),
					flagBool("protected", "Protect it (clusters.<db>.protected): db drop, a shrinking resize, credential rotation and restore promote/undo are refused even with --confirm. Needs vaire protect init first"),
					flagBool("allow-overcommit", "Create it even when its size pushes the databases' sizes past the server's memory budget (warning memory_overcommitted instead of the refusal)"),
				},
				meta: CommandMeta{
					Summary:        "Create a database in its own PostgreSQL cluster, with its migrate, app, and read-only roles",
					WhenToUse:      []string{"Add an application database and persist it in cluster.toml. Each database is its own cluster with a size (memory, CPU weight, connections). Without --size or --memory it gets [postgres] default_size. A size that pushes the databases' sizes past the server's memory budget (RAM less the reserve) is refused, with or without --size (precondition_failed, reason memory_overcommitted): the error gives the numbers and the largest size that still fits with the complete command (details.largest_size, details.suggested_command), or says no size fits and to resize another database down or use a bigger server. Nothing steps down by itself. --allow-overcommit creates it anyway with the warning memory_overcommitted. A cluster that couldn't start is refused (would_not_start, with the smallest size that would). Creating a database changes the other databases' share of the server, so their changed postgres-config is applied in the same run as a reload; a change that needs a restart is left pending (warning restart_pending with the confirm token). With backups on, it gets its own pgBackRest stanza, a first full backup and its backup timers, staggered from the other databases' (warning first_backup_failed when that fails; vaire apply retries). The app and ro URLs use the transaction pooler on 6432; the migrate URL uses the session pooler on 5432, because the transaction pooler refuses the migrate role. Both route by the database name. The credentials are printed once; if the connection is lost after the request was sent (transport_failed), they are still printed (and in error.details.database), since the node keeps creating the database and vaire apply finishes it. --protected marks it protected (see vaire db protect); it is refused until vaire protect init has set up two-factor protection (precondition_failed, reason protect_not_initialized)."},
					Examples:       []string{"vaire db create app --size small", "vaire db create app --protected", "vaire db create app --memory 768MiB --profile oltp", "vaire db create big --size xlarge --allow-overcommit", "vaire db create app --dry-run"},
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
					WhenToUse:      []string{"Remove an application database the human has approved dropping. With backups on, a final age-encrypted dump goes to the bucket first. Its cluster is stopped and deleted with its data, roles, pooler entries and timers. With backups on, its pgBackRest stanza is kept (data.stanza_kept) and recorded as dropped, so its backups stay restorable until they expire. The other databases' changed postgres-config (their larger share of the server) is applied in the same run as a reload; a change that needs a restart is left pending (warning restart_pending). Dropping the last database stops both poolers. A protected database (clusters.<db>.protected) is refused with database_protected, even with --confirm."},
					Examples:       []string{"vaire db drop app --confirm app", "vaire db drop app --dry-run"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}",
					ErrorCodes:     []string{"unsafe_action_requires_confirmation", "database_protected", "precondition_failed"},
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
					flagBool("allow-overcommit", "Grow it even when the new size pushes the databases' sizes past the server's memory budget (warning memory_overcommitted instead of the refusal)"),
				},
				meta: CommandMeta{
					Summary:        "Change a database's size or memory",
					WhenToUse:      []string{"Give a database more or less memory, CPU weight and connections. The cgroup limits and reload-only settings change live; shared_buffers, max_connections and huge_pages need a restart of that database only, confirmed with --confirm <db>/restart, or left pending with --defer-restart (finish later with vaire apply --db <db> --confirm <db>/restart). A size that couldn't start is refused (would_not_start). A larger size that pushes the databases' sizes past the server's memory budget is refused (precondition_failed, reason memory_overcommitted) with the numbers and the largest larger size that still fits, as a complete command, or says none does; --allow-overcommit grows it anyway with the warning memory_overcommitted. Shrinking is never refused for the budget; shrinking below current use warns (shrink_below_use). vaire config set clusters.<db>.size does the same change without applying it. Shrinking a protected database is refused (database_protected)."},
					Examples:       []string{"vaire db resize app --size large --confirm app/restart", "vaire db resize app --memory 768MiB --defer-restart", "vaire db resize app --size xlarge --confirm app/restart --allow-overcommit", "vaire db resize app --size small --dry-run"},
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
					flagString("size", "", "Size of the fork's cluster: nano, small, medium, large, xlarge or custom (default the source's size and memory)"),
					flagBool("allow-overcommit", "Fork even when the fork's size pushes the databases' sizes past the server's memory budget (warning memory_overcommitted instead of the refusal)"),
				},
				meta: CommandMeta{
					Summary:        "Copy one database under a new name, now or at a point in time",
					WhenToUse:      []string{"Rehearse a migration on yesterday's data, or investigate a past state. The fork is a cluster of its own: a physical copy of the source, from its stanza at --at or from the live source without it (which works without backups), with the database and roles renamed and new passwords, printed once. It takes the source's size, limits and settings, 90-overrides.conf included, or the size --size gives it; like db create, a size that pushes the databases' sizes past the server's memory budget is refused (precondition_failed, reason memory_overcommitted) with the largest size that fits as a complete command (vaire db fork <db> --name <new> --size <size>), and --allow-overcommit forks anyway with the warning; the other databases' changed postgres-config is applied in the same job as a reload; with backups on it gets its own stanza and first full backup (warning first_backup_failed when that fails). Needs free space for one copy of the source."},
					Examples:       []string{"vaire db fork app", "vaire db fork app --at 2026-10-09T12:00:00Z --name app_before", "vaire db fork app --name app_small --size small"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     backupErrorCodes,
				},
			},
			{
				use:  "protect [<db>...]",
				args: cobra.ArbitraryArgs,
				run:  runDBProtect,
				flags: []fl{
					flagBool("all", "Protect every database"),
				},
				meta: CommandMeta{
					Summary:        "Protect databases so drop, a shrinking resize, credential rotation and restore promote/undo are refused",
					WhenToUse:      []string{"Guard production databases against a drop or a destructive restore, by an agent or a slip. Takes one or more database names, or --all. It sets clusters.<db>.protected true in cluster.toml, as vaire config set clusters.<db>.protected true does; the node checks it live, so nothing needs applying. Databases already protected are reported and left alone. It is refused until vaire protect init has set up two-factor protection (precondition_failed, reason protect_not_initialized), since a protection must be liftable. An unknown name is refused before anything changes (precondition_failed, reason database_not_found). Lifting it is vaire db unprotect, for a human with the authenticator app."},
					Examples:       []string{"vaire db protect app", "vaire db protect app billing", "vaire db protect --all", "vaire db protect app --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     []string{"precondition_failed", "usage_error"},
				},
			},
			{
				use:  "unprotect <db>...",
				args: cobra.MinimumNArgs(1),
				run:  runDBUnprotect,
				meta: CommandMeta{
					Summary:        "Lift the protection of databases, with a code from the authenticator app",
					WhenToUse:      []string{"A human lifts a protection before dropping, shrinking, rotating or restoring a protected database. It asks once, without echo, for the current 6-digit code from the authenticator app set up with vaire protect init, for every database named, and the node checks it (protect_code_rejected when wrong, reused or locked out after 5 wrong codes). With --yes, without a terminal or with structured output it is refused (database_protected, reason unprotect_requires_human), so an agent cannot lift it. It sets clusters.<db>.protected false, as vaire config set clusters.<db>.protected false does. Databases that are not protected are reported and left alone."},
					WhenNotToUse:   []string{"Agents: never. Ask the human to run it at their terminal."},
					Examples:       []string{"vaire db unprotect app", "vaire db unprotect app billing"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     []string{"database_protected", "protect_code_rejected", "precondition_failed"},
				},
			},
			{
				use:  "credentials <name>",
				args: cobra.ExactArgs(1),
				run:  runDBCredentials,
				flags: []fl{
					flagBool("rotate", "Generate a new password for --role (required: vaire keeps no database passwords)"),
					flagString("role", "app", "Role to rotate: migrate, app, or ro"),
				},
				meta: CommandMeta{
					Summary:          "Rotate one role password and print the new credentials",
					WhenToUse:        []string{"A database password was lost or leaked: rotate it with --rotate --confirm <db>. vaire does not keep database passwords. Refused for a protected database (database_protected)."},
					Examples:         []string{"vaire db credentials app --rotate --role app --confirm app"},
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
			Examples:  []string{"vaire ext list", "vaire ext add vector --db app"},
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
					Examples:  []string{"vaire ext list", "vaire ext list --db app"},
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
					Examples:       []string{"vaire ext add vector --db app", "vaire ext add pg_cron --confirm prod/restart"},
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
					Examples:       []string{"vaire ext remove vector --db app --confirm app"},
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
			Examples:  []string{"vaire tune explain"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "show",
				run: runTuneShow,
				flags: []fl{
					flagString("db", "", "The database whose settings to add to the sheet; the only one is picked when there is one"),
				},
				meta: CommandMeta{
					Summary:   "Print the server's budget sheet and the calculated settings",
					WhenToUse: []string{"See how the server's memory is split between databases (each one's size, limits, and what is allocated, free or overcommitted), whether IOWeight is enforced, and the values vaire would write for a database's cluster. Oversubscription comes back as warnings."},
					Examples:  []string{"vaire tune show", "vaire tune show --db app"},
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
					Examples:  []string{"vaire tune explain", "vaire tune explain --db app"},
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
					Examples:       []string{"vaire tune apply --dry-run", "vaire tune apply --confirm app/restart"},
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
			Examples:  []string{"vaire backup setup", "vaire backup list --db app", "vaire backup now --type full"},
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
					Examples:         []string{"vaire backup setup", "vaire backup setup --provider r2 --endpoint https://<account>.r2.cloudflarestorage.com --bucket vaire-backups --key-id KEY --secret-file key.txt"},
					Mutating:         true,
					SupportsDryRun:   true,
					Prompts:          true,
					InteractiveFlags: []string{"--provider", "--endpoint", "--region", "--bucket", "--prefix", "--key-id", "--secret-file"},
					Remote:           true,
					ErrorCodes:       append([]string{"backup_auth_failed", "bucket_not_found", "endpoint_unreachable", "step_failed"}, backupErrorCodes...),
					Next:             []string{"vaire secrets export", "vaire backup list"},
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
					WhenToUse:      []string{"Take a backup outside the timers. Each database is its own cluster and stanza; without --db every database is backed up, one after another, because each job takes the server's heavy-I/O lock. A database whose job is skipped or fails doesn't stop the others; the command fails at the end. Ctrl-C detaches from the running job and starts no more; vaire job follow re-attaches."},
					Examples:       []string{"vaire backup now --db app --type full", "vaire backup now", "vaire backup now --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     backupErrorCodes,
					Next:           []string{"vaire backup list", "vaire job list"},
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
					Examples:  []string{"vaire backup list", "vaire backup list --db app --output json"},
					Remote:    true,
					Next:      []string{"vaire restore <db> --time <ts>", "vaire backup verify"},
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
					WhenToUse:        []string{"Prove the backups are readable. pgbackrest verify runs per stanza and downloads its backup files and WAL segments, so it can take a while; without --db every database is verified, one after another. Ctrl-C detaches from the running job and starts no more; vaire job follow re-attaches. The result goes to records.json and vaire.prom per database."},
					Examples:         []string{"vaire backup verify --db app", "vaire backup verify"},
					Mutating:         true,
					Remote:           true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "verify only records a check and does not change cluster data",
					ErrorCodes:       backupErrorCodes,
					Next:             []string{"vaire job list"},
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
			Examples:  []string{"vaire job list", "vaire job follow 01J..."},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "list",
				run: runJobList,
				meta: CommandMeta{
					Summary:   "Show running and recent jobs",
					WhenToUse: []string{"Find a job ID after detaching, or see what the timers ran."},
					Examples:  []string{"vaire job list", "vaire job list --output json"},
					Remote:    true,
					Next:      []string{"vaire job follow <id>"},
				},
			},
			{
				use:  "follow <id>",
				args: cobra.ExactArgs(1),
				run:  runJobFollow,
				meta: CommandMeta{
					Summary:   "Re-attach to a job and wait for its result",
					WhenToUse: []string{"Watch a job started earlier; a finished job prints its log and result."},
					Examples:  []string{"vaire job follow 01J9ZK3Q6V3W8Y2XH5N7T1C4BD"},
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
			flagBool("promote", "Cut over to the restored copy as soon as it is ready"),
		},
		meta: CommandMeta{
			Summary: "Rewind one database while the others keep serving",
			WhenToUse: []string{
				"Restore one database to a time, the latest WAL, a backup set or a dump. A physical restore of its stanza becomes the temporary cluster <db>__restoring, paused at the target, beside the live database, which keeps serving; inspect it with vaire sql --db <db>__restoring, then promote (a cut-over in place, a few seconds plus the replay to the target) or abort. It needs free space for one copy of the database. A dump (for points older than the window) loads into a new cluster instead, which gets a new stanza on promote.",
			},
			WhenNotToUse: []string{"Do not use it to copy a database under a new name: use vaire db fork. Rebuilding a whole server is server init --restore."},
			Examples: []string{
				"vaire restore app --time 2026-10-09T12:00:00Z --confirm app/restore",
				"vaire restore app --time -15m --promote --confirm app/restore",
				"vaire restore app --dump 20260701T030000Z --confirm app/restore",
			},
			Mutating:         true,
			Destructive:      true,
			SupportsDryRun:   true,
			Prompts:          true,
			InteractiveFlags: []string{"--confirm"},
			Remote:           true,
			ConfirmToken:     "{db}/restore",
			ErrorCodes:       append([]string{"unsafe_action_requires_confirmation"}, backupErrorCodes...),
			Next:             []string{"vaire restore status <db>", "vaire restore promote <db>", "vaire restore abort <db>"},
		},
		children: []cmdSpec{
			{
				use:  "status [db]",
				args: cobra.MaximumNArgs(1),
				run:  runRestoreStatus,
				meta: CommandMeta{
					Summary:   "Show open restores, row estimates against live, and leftovers",
					WhenToUse: []string{"Check a restore is ready and compare the restored copy with the live database before promoting."},
					Examples:  []string{"vaire restore status", "vaire restore status app"},
					Remote:    true,
				},
			},
			{
				use:  "promote <db>",
				args: cobra.ExactArgs(1),
				run:  runRestorePromote,
				meta: CommandMeta{
					Summary:        "Cut the live database over to its restored copy",
					WhenToUse:      []string{"After inspecting <db>__restoring: PgBouncer pauses the database, its cluster stops, the restored data directory takes the live one's place, and the cluster starts, reaches the target and opens on a new timeline, archiving to the same stanza; then a full backup starts. The URL and credentials don't change; the old data is kept as the stopped cluster <db>__pre_restore_<time> for undo."},
					Examples:       []string{"vaire restore promote app --confirm app/restore"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}/restore",
					Next:           []string{"vaire restore undo <db>", "vaire restore cleanup <db>"},
				},
			},
			{
				use:  "undo <db>",
				args: cobra.ExactArgs(1),
				run:  runRestoreUndo,
				meta: CommandMeta{
					Summary:        "Swap the data from before the last promote back in",
					WhenToUse:      []string{"A promoted restore was the wrong point in time and the leftover still exists. The old cluster resumes on its old timeline, vaire records it so later restores follow it, and a full backup starts. After a restore from a dump it also takes the old stanza back."},
					Examples:       []string{"vaire restore undo app --confirm app/restore"},
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
					Summary:        "Drop the temporary cluster <db>__restoring; the live database was never touched",
					WhenToUse:      []string{"Throw away a restored copy you don't want, or clean up after a failed restore."},
					Examples:       []string{"vaire restore abort app"},
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
					WhenToUse:        []string{"Free the space of the stopped <db>__pre_restore_* clusters once the restore is good. Without a database it only lists."},
					Examples:         []string{"vaire restore cleanup", "vaire restore cleanup app --confirm app/cleanup"},
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
					flagString("time", "", "Optional point in time for the drill (RFC 3339 or relative like -2h)"),
					flagString("db", "", "Drill only this database (default: every database, one at a time)"),
				},
				meta: CommandMeta{
					Summary:        "Restore a database into a temporary cluster, check it against live, drop it",
					WhenToUse:      []string{"Prove the backups restore. Each database's stanza is restored into the temporary cluster <db>__drill on the server's data disk (space for one copy), at the latest point or --time, and promoted there with archiving off; then pg_amcheck checks the database, its tables are compared with the live cluster's, and the copy is dropped. The live database keeps serving. Without --db every database is drilled, one after another, because each job takes the server's heavy-I/O lock; a database whose drill is skipped or fails doesn't stop the others. Each database's monthly timer runs it too, on a day of the month of its own; the result shows in vaire backup list."},
					Examples:       []string{"vaire restore drill --db app", "vaire restore drill", "vaire restore drill --db app --time -2h", "vaire restore drill --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     backupErrorCodes,
					Next:           []string{"vaire backup list"},
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
			Examples:         []string{"vaire dump app --out app.dump", "vaire dump app --to bucket"},
			Mutating:         true,
			Remote:           true,
			SkipDryRunCheck:  true,
			SkipDryRunReason: "dump writes an archive and does not change the cluster",
			ErrorCodes:       backupErrorCodes,
			Next:             []string{"vaire dump list <db>"},
		},
		children: []cmdSpec{
			{
				use:  "list [db]",
				args: cobra.MaximumNArgs(1),
				run:  runDumpList,
				meta: CommandMeta{
					Summary:   "List the long-retention dumps in the bucket",
					WhenToUse: []string{"Find a dump id for dump get or restore --dump. Reads the bucket with the recovery kit; the node is not involved."},
					Examples:  []string{"vaire dump list", "vaire dump list app"},
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
					Examples:         []string{"vaire dump get app 20260701T030000Z --out app-july.dump"},
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

func nodeCommands() cmdSpec {
	return cmdSpec{
		use:    "node",
		hidden: true,
		run:    parentRun,
		meta: CommandMeta{
			Summary:   "Node-side operations, hidden from top-level help",
			WhenToUse: []string{"Run the server half of vaire. Operator commands call these over SSH."},
			Examples:  []string{"vaire node version", "sudo vaire node rpc"},
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
					Examples:         []string{"vaire node upgrade", "vaire node upgrade --rollback --confirm prod/rollback"},
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
						"root, or sudo via /etc/sudoers.d/vaire",
						"the request's client_version, client_channel and, for dev builds, client_commit match vaire node version",
						"/etc/vaire/cluster.toml for plan and apply",
					},
					Examples:         []string{"sudo vaire node rpc < request.json"},
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
					Examples:  []string{"vaire node version --output json"},
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
					Examples:      []string{"sudo vaire node selfcheck", "sudo vaire node selfcheck --output json"},
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
					WhenToUse:      []string{"systemd timers and transient vaire-job-<id> units call this: certs-renew, stanza-expiry (daily: deletes the stanzas of dropped databases once their window has passed), and per database (--db, from vaire-<job>@<db>.timer) backup-full|diff|incr, backup-verify, restore-drill and db-dump; restore and db-fork run only with --job-id. Full, diff, verify, drill and dump take the server's heavy-I/O lock, so one runs at a time. A busy lock records the job as skipped: a timer run first waits up to 15 minutes for its locks, a run by hand or by an operator command skips at once."},
					Examples:       []string{"sudo vaire node run certs-renew", "sudo vaire node run backup-incr --db app", "sudo vaire node run db-dump --db app", "sudo vaire node run stanza-expiry"},
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
					WhenToUse:      []string{"vaire dump --to local runs this over SSH and streams the archive to the operator."},
					Examples:       []string{"sudo vaire node dump app > app.dump"},
					SupportsDryRun: true,
					LinuxOnly:      true,
				},
			},
			{
				use:    "archive-push <wal-path>",
				args:   cobra.ArbitraryArgs,
				run:    runNodeArchivePush,
				hidden: true,
				flags: []fl{
					flagString("stanza", "", "The database's pgBackRest stanza"),
					flagString("pgbackrest", "", "The pgBackRest binary to exec, /usr/bin/pgbackrest when empty (pgBackRest requires its name in archive_command)"),
				},
				meta: CommandMeta{
					Summary:          "Archive one WAL segment unless a newer master fenced this node",
					WhenToUse:        []string{"Every cluster's archive_command; PostgreSQL runs it as postgres. It refuses on a fenced node, then runs pgbackrest archive-push."},
					Examples:         []string{"/usr/local/bin/vaire node archive-push --pgbackrest=/usr/bin/pgbackrest --stanza=app-3f9a1c %p"},
					Mutating:         true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "archive-push is PostgreSQL's archive_command and must archive or fail",
					LinuxOnly:        true,
				},
			},
			{
				use: "unfence",
				run: runNodeUnfence,
				flags: []fl{
					flagBool("reclaim", "Also write a newer epoch naming this node, fencing the instance that took over (an abandoned rebuild only)"),
				},
				meta: CommandMeta{
					Summary:        "Lift the epoch fence on this node",
					WhenToUse:      []string{"Only when the server init --restore that fenced this server was a mistake or was abandoned. Run on the node over break-glass SSH, then vaire apply from your machine. A server that was really replaced is destroyed instead."},
					Examples:       []string{"sudo vaire node unfence --confirm prod/unfence --dry-run", "sudo vaire node unfence --confirm prod/unfence --reclaim"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					ConfirmToken:   "{server}/unfence",
					LinuxOnly:      true,
					ErrorCodes:     []string{"unsafe_action_requires_confirmation", "precondition_failed"},
					Next:           []string{"vaire apply"},
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
					Examples:         []string{"sudo vaire node ssh-revert"},
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
					WhenToUse:      []string{"Break glass when WireGuard is unreachable. This runs on the server, as root, for example from the provider's console; it is not an operator-machine command and there is nothing to run from your machine until the tunnel works."},
					Examples:       []string{"sudo vaire node unlock-ssh --minutes 30"},
					Mutating:       true,
					SupportsDryRun: true,
					LinuxOnly:      true,
				},
			},
		},
	}
}
