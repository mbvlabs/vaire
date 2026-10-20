package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/maint"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

func maintCommands() cmdSpec {
	return cmdSpec{
		use: "maint",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Run maintenance and capacity reports",
			WhenToUse: []string{"Vacuum, analyze, reindex, or report on bloat, wraparound, indexes, long queries and locks. Everything runs on the node as vaire_admin."},
			Examples:  []string{"vaire maint report bloat --db app"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "vacuum",
				run: runMaintVacuum,
				flags: []fl{
					flagString("db", "", "Database, or every database one after another when omitted"),
					flagString("table", "", "Vacuum one table (name or schema.name)"),
					flagBool("analyze", "Run ANALYZE with the vacuum"),
					flagBool("freeze", "Freeze tuples"),
					flagInt("parallel", 0, "Parallel index workers for a table. 0 lets PostgreSQL choose"),
				},
				meta: CommandMeta{
					Summary: "Vacuum a table or a whole database, as a detachable job",
					WhenToUse: []string{"Reclaim space for reuse or freeze tuples outside the daily maintenance job. " +
						"A whole database takes the heavy-I/O lock, so it never runs beside a full backup or a drill (lock_held)."},
					Examples:       []string{"vaire maint vacuum --db app --analyze", "vaire maint vacuum --db app --table events", "vaire maint vacuum --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     []string{"precondition_failed", "external_command_failed"},
					Next:           []string{"vaire job list"},
				},
			},
			{
				use: "analyze",
				run: runMaintAnalyze,
				flags: []fl{
					flagString("db", "", "Database, or every database one after another when omitted"),
					flagString("table", "", "Analyze one table"),
				},
				meta: CommandMeta{
					Summary:        "Refresh planner statistics, as a detachable job",
					WhenToUse:      []string{"Update statistics after a large load."},
					Examples:       []string{"vaire maint analyze --db app"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
				},
			},
			{
				use: "reindex",
				run: runMaintReindex,
				flags: []fl{
					flagString("db", "", "Database, or every database one after another when omitted"),
					flagString("table", "", "Reindex one table"),
					flagString("index", "", "Reindex one index"),
					flagBoolDefault("concurrently", true, "Rebuild indexes without blocking writes (vaire refuses false)"),
					flagBool("cleanup-invalid", "Drop leftover invalid _ccnew indexes of a failed concurrent rebuild"),
				},
				meta: CommandMeta{
					Summary: "Rebuild indexes concurrently, as a detachable job",
					WhenToUse: []string{"Repair or compact indexes without blocking writes. A failed rebuild leaves invalid _ccnew indexes " +
						"(warning reindex_left_invalid); --cleanup-invalid drops them."},
					Examples:       []string{"vaire maint reindex --db app --table events", "vaire maint reindex --db app --cleanup-invalid"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     []string{"precondition_failed"},
				},
			},
			{
				use: "repack",
				run: runMaintRepack,
				flags: []fl{
					flagString("db", "", "Database name"),
					flagString("table", "", "Table to repack"),
					flagBool("now", "Run outside the maintenance window"),
				},
				meta: CommandMeta{
					Summary: "Repack a bloated table with pg_repack (Phase 7)",
					WhenToUse: []string{"Rewrite a table online. pg_repack arrives with extensions in Phase 7; until then this fails with " +
						"precondition_failed (extension_missing)."},
					Examples:       []string{"vaire maint repack --db app --table events --confirm app"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}",
					ErrorCodes:     []string{"precondition_failed"},
				},
			},
			{
				use:  "report <kind>",
				args: cobra.ExactArgs(1),
				run:  runMaintReport,
				flags: []fl{
					flagString("db", "", "Database; every database when omitted"),
					flagInt("min-bytes", 0, "Minimum estimated bloat to include"),
					flagString("older-than", "5m", "Age threshold for long-query reports"),
				},
				meta: CommandMeta{
					Summary: "Report bloat, wraparound, indexes, long queries, or locks",
					WhenToUse: []string{"Read a maintenance report. kind is bloat, wraparound, unused-indexes, missing-indexes, long-queries, or locks. " +
						"Bloat is an estimate from statistics (the ioguix queries)."},
					Examples: []string{"vaire maint report bloat --db app", "vaire maint report wraparound", "vaire maint report long-queries --older-than 10m"},
					Remote:   true,
				},
			},
			{
				use: "kill",
				run: runMaintKill,
				flags: []fl{
					flagString("db", "", "Database whose backends to terminate"),
					flagBool("idle-in-tx", "Terminate idle-in-transaction sessions (never vaire's or the superuser's)"),
					flagInt("pid", 0, "Terminate this backend pid"),
					flagString("older-than", "", "Only sessions idle in transaction longer than this duration"),
				},
				meta: CommandMeta{
					Summary:        "Terminate backends of one database that match the filter",
					WhenToUse:      []string{"Stop a stuck session after the human approves the confirm token <db>/kill.", "On a protected database it also asks, after the token and without echo, for the current 6-digit code from the authenticator app, which the node checks. Without a terminal, with --yes or structured output it is refused (database_protected, reason write_requires_human). A dry run needs no code."},
					Examples:       []string{"vaire maint kill --db app --idle-in-tx --older-than 10m --confirm app/kill", "vaire maint kill --db app --pid 4242 --dry-run"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{db}/kill",
					ErrorCodes:     []string{"precondition_failed", "unsafe_action_requires_confirmation", "database_protected", "protect_code_rejected"},
				},
			},
		},
	}
}

// maintOptions reads a maintenance run's flags for kind.
func maintOptions(cmd *cobra.Command, kind string) maint.Options {
	o := maint.Options{Kind: kind, Table: stringFlag(cmd, "table"), Index: stringFlag(cmd, "index"),
		Analyze: flagTrue(cmd, "analyze"), Freeze: flagTrue(cmd, "freeze"), CleanupInvalid: flagTrue(cmd, "cleanup-invalid")}
	if cmd.Flags().Lookup("parallel") != nil {
		o.Parallel, _ = cmd.Flags().GetInt("parallel")
	}
	if f := cmd.Flags().Lookup("concurrently"); f != nil && f.Changed && f.Value.String() == "false" {
		o.NotConcurrently = true
	}
	return o
}

func runMaintVacuum(cmd *cobra.Command, _ []string) error  { return runMaint(cmd, maint.Vacuum) }
func runMaintAnalyze(cmd *cobra.Command, _ []string) error { return runMaint(cmd, maint.Analyze) }
func runMaintReindex(cmd *cobra.Command, _ []string) error { return runMaint(cmd, maint.Reindex) }

// runMaint runs a vacuum, analyze or reindex as a job per database, one
// after another (every database without --db).
func runMaint(cmd *cobra.Command, kind string) error {
	o := maintOptions(cmd, kind)
	probe := o
	probe.DB = "x"
	if err := probe.Validate(); err != nil {
		return output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dbs, err := backupDatabases(cmd, s, p)
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		var plans []any
		var lines []string
		for _, db := range dbs {
			o.DB = db
			reply, err := p.call(commandContext(cmd), s.node, "maint.run", o, remote.CallOptions{DryRun: true, OnEvent: p.event})
			if err != nil {
				return err
			}
			var plan struct {
				Statements []string `json:"statements"`
				Command    string   `json:"command"`
				HeavyIO    bool     `json:"heavy_io"`
			}
			_ = reply.Decode(&plan)
			plans = append(plans, plan)
			what := plan.Command
			if len(plan.Statements) > 0 {
				what = strings.Join(plan.Statements, "; ")
			}
			lines = append(lines, "dry run: "+db+": "+what)
		}
		return output.Success(cmd, output.Result{Data: map[string]any{"plans": plans}, Message: strings.Join(lines, "\n"), Warnings: s.warnings})
	}
	outcomes, lines, warnings, detached := eachDatabase(cmd, s, p, dbs, "maint.run",
		func(db string) any { o.DB = db; return o },
		func(raw []byte) (any, string, error) {
			var res struct {
				DB       string           `json:"db"`
				Kind     string           `json:"kind"`
				Invalid  []string         `json:"invalid_indexes"`
				Dropped  []string         `json:"dropped"`
				Warnings []output.Warning `json:"warnings"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return nil, "", err
			}
			line := res.DB + ": " + res.Kind + " finished"
			if len(res.Dropped) > 0 {
				line += "; dropped " + strings.Join(res.Dropped, ", ")
			}
			if len(res.Invalid) > 0 {
				line += "; invalid indexes left: " + strings.Join(res.Invalid, ", ") + " (--cleanup-invalid drops them)"
			}
			return res, line, nil
		})
	if detached != nil {
		return detachedEach(cmd, s, *detached, "the "+kind, notStarted(dbs, outcomes), func(db string) string {
			return "vaire maint " + kind + " --db " + db
		})
	}
	if err := failedOutcomes("the "+kind, "runs", outcomes); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: map[string]any{"runs": outcomes}, Message: strings.Join(lines, "\n"),
		Warnings: append(s.warnings, warnings...)})
}

// runMaintRepack refuses until pg_repack arrives with extensions (Phase 7).
// The node's precondition check runs before the confirm token is asked for.
func runMaintRepack(cmd *cobra.Command, _ []string) error {
	db := stringFlag(cmd, "db")
	if db == "" {
		return output.NewError(output.CodeInputRequired, "maint repack needs --db", output.ExitInput, "").WithDetails(map[string]any{"flag": "--db"})
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	o := maintOptions(cmd, maint.Repack)
	o.DB = db
	// The node checks pg_repack's preconditions on a dry run too: ask it
	// first, so a repack that cannot run fails before the confirm token is
	// asked for.
	plan, err := p.call(commandContext(cmd), s.node, "maint.run", o, remote.CallOptions{DryRun: true, OnEvent: p.event})
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		var data map[string]any
		_ = plan.Decode(&data)
		return output.Success(cmd, output.Result{Data: data, Message: "dry run: repack " + db, Warnings: append(s.warnings, plan.Warnings...)})
	}
	if err := Confirm(cmd, db); err != nil {
		return err
	}
	reply, err := p.call(commandContext(cmd), s.node, "maint.run", o, remote.CallOptions{Confirm: db, OnEvent: p.event})
	if err != nil {
		return err
	}
	var data map[string]any
	_ = reply.Decode(&data)
	return output.Success(cmd, output.Result{Data: data, Message: "repacked", Warnings: append(s.warnings, reply.Warnings...)})
}

// maintReportDB is one database's report from the node.
type maintReportDB struct {
	DB    string              `json:"db"`
	Rows  []map[string]string `json:"rows,omitempty"`
	Bloat *maint.BloatReport  `json:"bloat,omitempty"`
	Error string              `json:"error,omitempty"`
}

func runMaintReport(cmd *cobra.Command, args []string) error {
	kind := args[0]
	if !maint.ValidReport(kind) {
		return output.NewError(output.CodeUsage, "unknown report "+kind, output.ExitUsage, "Use one of "+strings.Join(maint.ReportKinds, ", ")+".")
	}
	minBytes, _ := cmd.Flags().GetInt("min-bytes")
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	params := map[string]any{"kind": kind, "older_than": stringFlag(cmd, "older-than"), "min_bytes": minBytes}
	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}
	reply, err := p.call(commandContext(cmd), s.node, "maint.report", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Kind      string          `json:"kind"`
		Databases []maintReportDB `json:"databases"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: data, Message: reportMessage(kind, data.Databases), Warnings: append(s.warnings, reply.Warnings...)})
}

// reportColumns are the columns each report prints, in order.
var reportColumns = map[string][]string{
	maint.ReportWraparound:     {"kind", "name", "xid_age", "mxid_age", "status"},
	maint.ReportUnusedIndexes:  {"schemaname", "table_name", "index_name", "bytes", "idx_scan"},
	maint.ReportMissingIndexes: {"schemaname", "table_name", "seq_scan", "seq_tup_read", "idx_scan", "n_live_tup"},
	maint.ReportLongQueries:    {"pid", "role", "state", "wait", "seconds", "query"},
	maint.ReportLocks:          {"pid", "role", "state", "wait", "seconds", "blocked_by", "query"},
}

func reportMessage(kind string, dbs []maintReportDB) string {
	var lines []string
	for _, db := range dbs {
		lines = append(lines, db.DB+":")
		switch {
		case db.Error != "":
			lines = append(lines, "  "+db.Error)
		case db.Bloat != nil:
			if len(db.Bloat.Rows) == 0 {
				lines = append(lines, "  no estimated bloat")
			}
			for _, r := range db.Bloat.Rows {
				name := r.Schema + "." + r.Table
				if r.Index != "" {
					name += " (index " + r.Index + ")"
				}
				lines = append(lines, fmt.Sprintf("  %-50s %10s of %10s  %5.1f%%", name, humanBytes(r.BloatBytes), humanBytes(r.RealSize), r.Ratio*100))
			}
			if len(db.Bloat.Unestimated) > 0 {
				lines = append(lines, "  not estimated (no statistics): "+strings.Join(db.Bloat.Unestimated, ", "))
			}
		case len(db.Rows) == 0:
			lines = append(lines, "  nothing to report")
		default:
			cols := reportColumns[kind]
			lines = append(lines, "  "+strings.Join(cols, "  "))
			for _, r := range db.Rows {
				vals := make([]string, 0, len(cols))
				for _, c := range cols {
					vals = append(vals, r[c])
				}
				lines = append(lines, "  "+strings.Join(vals, "  "))
			}
		}
	}
	if kind == maint.ReportBloat {
		lines = append(lines, "an estimate from statistics (ioguix/pgsql-bloat-estimation); a plain vacuum makes the space reusable, repack (Phase 7) shrinks it")
	}
	return strings.Join(lines, "\n")
}

func runMaintKill(cmd *cobra.Command, _ []string) error {
	db := stringFlag(cmd, "db")
	pid, _ := cmd.Flags().GetInt("pid")
	idle := flagTrue(cmd, "idle-in-tx")
	if db == "" {
		return output.NewError(output.CodeInputRequired, "maint kill needs --db", output.ExitInput, "").WithDetails(map[string]any{"flag": "--db"})
	}
	if (pid == 0) == !idle {
		return output.NewError(output.CodeUsage, "pass either --pid or --idle-in-tx", output.ExitUsage, "")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	params := map[string]any{"db": db, "pid": pid, "idle_in_tx": idle, "older_than": stringFlag(cmd, "older-than")}
	token := db + "/kill"
	var code map[string]string
	if !dryRun(cmd) {
		if err := Confirm(cmd, token); err != nil {
			return err
		}
		// On a protected database it also takes a code from the
		// authenticator app; the node checks it.
		code, err = protectedWriteCode(cmd, db, "terminating its sessions",
			func() (protectStatusData, error) { return fetchProtectStatus(cmd, s, p) }, p.note)
		if err != nil {
			return err
		}
	}
	reply, err := p.call(commandContext(cmd), s.node, "maint.kill", params, remote.CallOptions{DryRun: dryRun(cmd), Confirm: token, Secrets: code, OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Targets []maint.Backend `json:"targets"`
		DryRun  bool            `json:"dry_run"`
	}
	_ = reply.Decode(&data)
	verb := "terminated"
	if data.DryRun {
		verb = "dry run: would terminate"
	}
	lines := []string{fmt.Sprintf("%s %d backends in %s", verb, len(data.Targets), db)}
	for _, b := range data.Targets {
		lines = append(lines, fmt.Sprintf("  %d %s %s %ds %s", b.PID, b.Role, b.State, b.Seconds, b.Query))
	}
	var raw map[string]any
	_ = reply.Decode(&raw)
	return output.Success(cmd, output.Result{Data: raw, Message: strings.Join(lines, "\n"), Warnings: append(s.warnings, reply.Warnings...)})
}
