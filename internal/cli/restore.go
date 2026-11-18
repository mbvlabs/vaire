package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

// recoveryKit returns the workspace's recovery kit.
func recoveryKit(name string) (*workspaces.BackupCreds, error) {
	creds, err := workspaces.LoadCredentials(name)
	if err != nil {
		return nil, err
	}

	if creds.Backup == nil || creds.Backup.AccessKeyID == "" {
		return nil, output.NewError(output.CodeInputRequired, "this workspace has no recovery kit", output.ExitInput,
			"Run vaire backup setup, or log in from a transfer bundle that carries the recovery kit (vaire login).").
			WithDetails(map[string]any{"reason": "recovery_kit_missing"})
	}

	return creds.Backup, nil
}

func kitTarget(k *workspaces.BackupCreds) backup.Target {
	return backup.Target{Endpoint: k.Endpoint, Region: k.Region, Bucket: k.Bucket, Prefix: k.Prefix, URIStyle: k.URIStyle, KeyID: k.AccessKeyID, Secret: k.SecretAccessKey}
}

// restoreJobResult is the restore.start job's result, as the restore command
// reads it back.
type restoreJobResult struct {
	Phase     string `json:"phase"`
	Restoring string `json:"restoring"`
	// FollowerWarnings are a combined --promote's structured warnings, such
	// as followers_diverged (D12): they keep their code.
	FollowerWarnings []output.Warning `json:"follower_warnings"`
	PoolerWarning    string           `json:"pooler_warning"`
	Promoted         struct {
		Warning *output.Warning `json:"warning"`
	} `json:"promoted"`
}

// restoreWarnings is the restore command's warning list: the run's own and
// the reply's, and the job's structured warnings, which keep the code the
// node gave them.
func restoreWarnings(own, reply []output.Warning, res restoreJobResult) []output.Warning {
	warnings := append(own, reply...)

	warnings = append(warnings, res.FollowerWarnings...)
	if res.PoolerWarning != "" {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed", Message: res.PoolerWarning})
	}

	if res.Promoted.Warning != nil {
		warnings = append(warnings, *res.Promoted.Warning)
	}

	return warnings
}

func runRestore(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return parentRun(cmd, args)
	}

	db := args[0]

	params, target, err := restoreParams(cmd, db)
	if err != nil {
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

	if flagTrue(cmd, "promote") {
		// The node refuses it too; this saves asking for the token or
		// reporting a dry run first.
		if err := combinedRestorePreflight(cfg, db); err != nil {
			return err
		}
	}

	if !target.IsZero() {
		p.note("restore target: " + echoTime(target, cfg.Timezone))
	}

	token := db + "/restore"
	if !dryRun(cmd) {
		if err := Confirm(cmd, token); err != nil {
			return err
		}
	}

	reply, jobID, detached, err := callJob(commandContext(cmd), s, p, "restore.start", params, remote.CallOptions{Confirm: token, DryRun: dryRun(cmd)})
	if err != nil {
		return err
	}

	if detached {
		return detachedResult(cmd, s, jobID, "the restore")
	}

	if dryRun(cmd) {
		return restoreDryRunResult(cmd, db, s, reply)
	}

	res := restoreJobResult{}

	jr, err := decodeJob(reply, &res)
	if err != nil {
		return err
	}

	warnings := restoreWarnings(s.warnings, reply.Warnings, res)
	if res.Phase == "promoted" {
		return restorePromotedResult(cmd, db, jr, res, warnings)
	}

	return restoreReadyResult(cmd, db, params, jr, res, warnings, token)
}

// restoreParams are the restore.start parameters for the target the flags
// name, and that target for the note.
func restoreParams(cmd *cobra.Command, db string) (map[string]any, time.Time, error) {
	kinds := 0
	params := map[string]any{"database": db, "promote": flagTrue(cmd, "promote")}

	var target time.Time

	if flagTrue(cmd, "latest") {
		kinds++
		params["kind"] = backup.TargetLatest
	}

	if v := stringFlag(cmd, "set"); v != "" {
		kinds++
		params["kind"], params["set"] = backup.TargetSet, v
	}

	t, wire, err := targetFlag(cmd, "time")
	if err != nil {
		return nil, time.Time{}, err
	}

	if wire != "" {
		kinds++
		target = t
		params["kind"], params["time"] = backup.TargetTime, wire
	}

	if kinds != 1 {
		return nil, time.Time{}, output.NewError(output.CodeUsage, "pass exactly one of --time, --latest or --set", output.ExitUsage,
			"For example vaire restore "+db+" --time -15m.")
	}

	return params, target, nil
}

// restoreDryRunResult reports what a dry run would do.
func restoreDryRunResult(cmd *cobra.Command, db string, s *session, reply remote.Reply) error {
	var data map[string]any

	_ = reply.Decode(&data)

	msg := fmt.Sprintf("dry run: would restore %s to %v beside the live database", db, data["target"])
	if tl, _ := data["timeline"].(string); tl != "" {
		msg += ", with --target-timeline=" + tl
	}

	return output.Success(cmd, output.Result{Data: data, Message: msg, Warnings: append(s.warnings, reply.Warnings...)})
}

// restorePromotedResult reports a restore that cut over to the new copy.
func restorePromotedResult(cmd *cobra.Command, db string, jr jobReply, res restoreJobResult, warnings []output.Warning) error {
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"job_id": jr.JobID, "result": res},
		Message:  db + " is restored and promoted, on a new timeline; a full backup is running. The replaced data is kept as a stopped cluster until vaire restore cleanup " + db + "; vaire restore undo " + db + " swaps it back.",
		Warnings: warnings,
		Next:     []string{"vaire restore undo " + db, "vaire restore cleanup " + db},
	})
}

// restoreReadyResult reports the temporary cluster waiting beside the live
// database.
func restoreReadyResult(cmd *cobra.Command, db string, params map[string]any, jr jobReply, res restoreJobResult, warnings []output.Warning, token string) error {
	ready := "the temporary cluster " + res.Restoring + " is ready, paused at the target, beside the live " + db + ", which kept serving."
	if params["kind"] == backup.TargetLatest {
		// --latest has no target to pause at: the copy keeps replaying the
		// archived WAL until it is promoted or aborted.
		ready = "the temporary cluster " + res.Restoring + " is ready, still replaying the latest archived WAL, beside the live " + db + ", which kept serving."
	}

	return output.Success(cmd, output.Result{
		Data: map[string]any{"job_id": jr.JobID, "result": res},
		Message: strings.Join([]string{
			ready,
			"Inspect it with vaire sql --db " + res.Restoring + " \"select ...\", then promote or abort.",
		}, "\n"),
		Warnings: warnings,
		Next:     []string{"vaire restore status " + db, "vaire restore promote " + db + " --confirm " + token, "vaire restore abort " + db},
	})
}

func runRestoreStatus(cmd *cobra.Command, args []string) error {
	params := map[string]string{}
	if len(args) > 0 {
		params["database"] = args[0]
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	reply, err := p.call(commandContext(cmd), s.node, "restore.status", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var data struct {
		Restores []struct {
			backup.Restore
			Restoring        string `json:"restoring_database"`
			OnPromote        string `json:"on_promote"`
			ApplicationReady *bool  `json:"application_ready,omitempty"`
			AccessCoverage   string `json:"access_coverage,omitempty"`
			AccessNotice     string `json:"access_notice,omitempty"`
			Tables           []struct {
				Table     string `json:"table"`
				Live      *int64 `json:"live_rows"`
				Restoring *int64 `json:"restoring_rows"`
			} `json:"tables"`
		} `json:"restores"`
		Leftovers []backup.Leftover `json:"leftovers"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}

	var lines []string
	for _, r := range data.Restores {
		lines = append(lines, fmt.Sprintf("%s: %s, target %s, started %s", r.Database, r.Phase, r.Target, r.StartedAt))
		if r.Error != "" {
			lines = append(lines, "  error: "+r.Error)
		}

		if r.AccessNotice != "" {
			lines = append(lines, "  "+r.AccessNotice)
		}

		if len(r.Tables) > 0 {
			lines = append(lines, fmt.Sprintf("  %-40s %14s %14s", "TABLE (estimated rows)", "LIVE", "RESTORED"))
			for _, t := range r.Tables {
				lines = append(lines, fmt.Sprintf("  %-40s %14s %14s", t.Table, rowCount(t.Live), rowCount(t.Restoring)))
			}
		}

		if r.Phase == backup.PhaseReady {
			lines = append(lines, "  promote: "+r.OnPromote)
		}
	}

	for _, l := range data.Leftovers {
		lines = append(lines, "leftover "+l.Name+" (from "+l.CreatedAt+"); drop it with vaire restore cleanup "+l.Database)
	}

	if len(lines) == 0 {
		lines = []string{"no restore is open"}
	}

	return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: append(s.warnings, reply.Warnings...)})
}

func rowCount(n *int64) string {
	if n == nil {
		return "-"
	}

	if *n < 0 {
		return "unknown"
	}

	return fmt.Sprint(*n)
}

// restoreAction runs promote, undo or abort for one database. done gives
// the message from the node's reply. A protected database refuses action
// (when set) before the token is asked for; the node refuses it too.
func restoreAction(cmd *cobra.Command, db, method, token, action string, done func(data map[string]any) string) error {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	if action != "" {
		cfg, err := remoteConfig(cmd, s, p)
		if err != nil {
			return err
		}

		if err := restorePreflight(cfg, db, method, action); err != nil {
			return err
		}
	}

	if token != "" && !dryRun(cmd) {
		if err := Confirm(cmd, token); err != nil {
			return err
		}
	}

	reply, err := p.call(commandContext(cmd), s.node, method, map[string]string{"database": db},
		remote.CallOptions{Confirm: token, DryRun: dryRun(cmd), OnEvent: p.event})
	if err != nil {
		return err
	}

	var data map[string]any
	if err := reply.Decode(&data); err != nil {
		return err
	}

	msg := done(data)
	if dryRun(cmd) {
		msg = "dry run: " + method + " " + db + " would run"
	}

	return output.Success(cmd, output.Result{Data: data, Message: msg, Warnings: append(s.warnings, reply.Warnings...)})
}

// restorePreflight is what restoreAction refuses before it asks for the
// token: a follower first, with the node's is_follower error, then a
// protected database.
func restorePreflight(cfg config.Cluster, db, method, action string) error {
	if err := node.RefuseFollowerMethod(cfg, method, map[string]string{"database": db}); err != nil {
		return err
	}

	return node.CheckNotProtected(cfg, db, action)
}

// combinedRestorePreflight is what restore --promote refuses, in the same
// order as the node: a follower first, then a protected database.
func combinedRestorePreflight(cfg config.Cluster, db string) error {
	return restorePreflight(cfg, db, "restore.start", "restore promote")
}

func runRestorePromote(cmd *cobra.Command, args []string) error {
	db := args[0]

	return restoreAction(cmd, db, "restore.promote", db+"/restore", "restore promote", func(_ map[string]any) string {
		return db + " now holds the restored data, on a new timeline; a full backup is running. The previous data is kept as a stopped cluster: vaire restore undo " + db + " swaps it back, vaire restore cleanup " + db + " drops it."
	})
}

func runRestoreUndo(cmd *cobra.Command, args []string) error {
	db := args[0]

	return restoreAction(cmd, db, "restore.undo", db+"/restore", "restore undo", func(data map[string]any) string {
		msg := db + " is back to its data from before the restore, on its old timeline; a full backup is running."
		if aside, _ := data["pre_undo"].(string); aside != "" {
			msg += " The restored data is kept as the stopped cluster " + aside + " until vaire restore cleanup " + db + "; undo can't swap it back in."
		}

		return msg
	})
}

func runRestoreAbort(cmd *cobra.Command, args []string) error {
	db := args[0]

	return restoreAction(cmd, db, "restore.abort", "", "", func(map[string]any) string {
		return "dropped the temporary cluster " + db + "__restoring; the live " + db + " was never touched"
	})
}

func runRestoreCleanup(cmd *cobra.Command, args []string) error {
	db := ""
	if len(args) > 0 {
		db = args[0]
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	ctx := commandContext(cmd)

	reply, err := p.call(ctx, s.node, "restore.cleanup", map[string]any{"database": db}, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var data struct {
		Leftovers []struct {
			backup.Leftover
			SizeBytes int64 `json:"size_bytes"`
		} `json:"leftovers"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}

	rows := make([][]string, 0, len(data.Leftovers))
	for _, l := range data.Leftovers {
		rows = append(rows, []string{l.Name, humanBytes(l.SizeBytes), l.CreatedAt})
	}

	lines := alignColumns(rows)
	if len(lines) == 0 {
		return output.Success(cmd, output.Result{Data: data, Message: "no restore leftovers", Warnings: append(s.warnings, reply.Warnings...)})
	}

	if db == "" || dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: data, Message: "restore leftovers:\n" + strings.Join(lines, "\n"), Warnings: append(s.warnings, reply.Warnings...),
			Next: []string{"vaire restore cleanup <db> --confirm <db>/cleanup"}})
	}

	p.panel("Leftovers of "+db, lines)

	token := db + "/cleanup"
	if err := Confirm(cmd, token); err != nil {
		return err
	}

	reply, err = p.call(ctx, s.node, "restore.cleanup", map[string]any{"database": db, "drop": true}, remote.CallOptions{Confirm: token, OnEvent: p.event})
	if err != nil {
		return err
	}

	var res map[string]any

	_ = reply.Decode(&res)

	return output.Success(cmd, output.Result{Data: res, Message: fmt.Sprintf("dropped %d leftover cluster(s) of %s", len(lines), db), Warnings: append(s.warnings, reply.Warnings...)})
}

func runRestoreDrill(cmd *cobra.Command, _ []string) error {
	_, at, err := targetFlag(cmd, "time")
	if err != nil {
		return err
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	params := func(db string) any { return map[string]string{"time": at, "database": db} }
	if dryRun(cmd) {
		reply, err := p.call(commandContext(cmd), s.node, "restore.drill", params(stringFlag(cmd, "db")), remote.CallOptions{DryRun: true, OnEvent: p.event})
		if err != nil {
			return err
		}

		var data struct {
			Drills []struct {
				Database string `json:"database"`
				Target   string `json:"target"`
				Timeline string `json:"timeline,omitempty"`
			} `json:"drills"`
		}

		_ = reply.Decode(&data)

		lines := make([]string, 0, len(data.Drills))
		for _, d := range data.Drills {
			line := fmt.Sprintf("dry run: would restore %s at %s into %s, check it and drop it", d.Database, d.Target, backup.DrillName(d.Database))
			if d.Timeline != "" {
				line += " (timeline " + d.Timeline + ")"
			}

			lines = append(lines, line)
		}

		return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: append(s.warnings, reply.Warnings...)})
	}

	dbs, err := backupDatabases(cmd, s, p)
	if err != nil {
		return err
	}

	outcomes, lines, warnings, detached := eachDatabase(cmd, s, p, dbs, "restore.drill", params,
		func(raw []byte) (any, string, error) {
			var drill backup.Drill
			if err := json.Unmarshal(raw, &drill); err != nil {
				return nil, "", err
			}

			return drill, drillLines(drill), nil
		})
	if detached != nil {
		return detachedEach(cmd, s, *detached, "the drill", notStarted(dbs, outcomes), func(db string) string {
			if at != "" {
				return "vaire restore drill --db " + db + " --time " + at
			}

			return "vaire restore drill --db " + db
		})
	}

	if err := failedOutcomes("the restore drill", "drills", outcomes); err != nil {
		return err
	}

	return output.Success(cmd, output.Result{
		Data:     map[string]any{"drills": outcomes},
		Message:  strings.Join(lines, "\n"),
		Warnings: append(s.warnings, warnings...),
		Next:     []string{"vaire backup list"},
	})
}

func runDBFork(cmd *cobra.Command, args []string) error {
	src := args[0]

	name, err := forkName(cmd, src)
	if err != nil {
		return err
	}

	params, at, err := forkParams(cmd, src, name)
	if err != nil {
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

	if err := checkForkSize(cfg, src, stringFlag(cmd, "size")); err != nil {
		return err
	}

	if !at.IsZero() {
		p.note("fork point: " + echoTime(at, cfg.Timezone))
	}

	rpc, secret, err := forkSecrets(cfg, s, name)
	if err != nil {
		return err
	}

	reply, jobID, detached, err := callJob(commandContext(cmd), s, p, "db.fork", params, remote.CallOptions{Secrets: rpc, DryRun: dryRun(cmd)})
	if err != nil {
		return err
	}

	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: params, Message: "dry run: would fork " + src + " into " + name, Warnings: s.warnings})
	}

	if detached {
		return forkDetachedResult(cmd, p, name, jobID, secret, s.warnings)
	}

	return forkResult(cmd, reply, src, name, at, secret, s.warnings)
}

// forkName is the new database's name: --name or one derived from src.
func forkName(cmd *cobra.Command, src string) (string, error) {
	name := stringFlag(cmd, "name")
	if name == "" {
		name = src + "_fork_" + time.Now().UTC().Format("200601021504")
	}

	if err := config.CheckDatabaseName(name); err != nil {
		return "", output.NewError(output.CodeValidation, "database name "+name+" "+err.Error(), output.ExitConfig, "Pass a shorter --name.")
	}

	return name, nil
}

// forkParams are db.fork's parameters, and the fork point for the note.
func forkParams(cmd *cobra.Command, src, name string) (map[string]any, time.Time, error) {
	params := map[string]any{"source": src, "name": name}

	var at time.Time

	t, wire, err := targetFlag(cmd, "at")
	if err != nil {
		return nil, time.Time{}, err
	}

	if wire != "" {
		at = t
		params["at"] = wire
	}
	// Sent only when set, so an older node still takes the request.
	if size := stringFlag(cmd, "size"); size != "" {
		params["size"] = size
	}

	if flagTrue(cmd, "allow-overcommit") {
		params["allow_overcommit"] = true
	}

	return params, at, nil
}

// checkForkSize refuses a --size the node would refuse anyway.
func checkForkSize(cfg config.Cluster, src, size string) error {
	if size == "" {
		return nil
	}

	spec, ok := cfg.ClusterByName(src)
	if !ok {
		return nil
	}

	spec.Size, spec.Memory = size, ""

	return checkSizeFlags(cfg, spec)
}

// forkSecrets are the one-time credentials of the copy and the secrets the
// job needs.
func forkSecrets(cfg config.Cluster, s *session, name string) (map[string]string, databaseSecret, error) {
	secret, rpc, err := newDatabaseSecret(cfg, name, steps.DatabaseRoles)
	if err != nil {
		return nil, databaseSecret{}, err
	}

	service, _, generated, err := serviceSecrets(s.ctx.Name)
	if err != nil {
		return nil, databaseSecret{}, err
	}

	maps.Copy(rpc, service)

	s.warnings = append(s.warnings, staleWarning(generated)...)

	return rpc, secret, nil
}

// forkDetachedResult hands over the credentials of a fork that keeps
// running.
func forkDetachedResult(cmd *cobra.Command, p *progress, name, jobID string, secret databaseSecret, warnings []output.Warning) error {
	// The passwords are set in the copy before the job ends; they are
	// only shown here.
	p.warn("detached: the fork keeps running. These credentials work once it is done and are shown only now:")

	message, human := credentialsMessage(cmd, "fork detached; credentials shown only now", "fork detached; credentials shown only now", name, secret)

	return output.Success(cmd, output.Result{Data: map[string]any{"job_id": jobID, "detached": true, "database": secret},
		Message: message, Human: human, Warnings: warnings, Next: []string{"vaire job follow " + jobID}})
}

// forkResult reports a finished fork.
func forkResult(cmd *cobra.Command, reply remote.Reply, src, name string, at time.Time, secret databaseSecret, warnings []output.Warning) error {
	var res struct {
		Warnings []output.Warning `json:"warnings"`
	}

	jr, err := decodeJob(reply, &res)
	if err != nil {
		return err
	}

	all := append(append(warnings, reply.Warnings...), res.Warnings...)

	from := "the live " + src
	if !at.IsZero() {
		from = src + " as of " + at.Format(time.RFC3339Nano)
	}

	message, human := credentialsMessage(cmd, fmt.Sprintf("forked %s into %s with its own roles", from, name), "forked "+from+" into "+name+" with its own roles", name, secret)

	return output.Success(cmd, output.Result{
		Data:     map[string]any{"job_id": jr.JobID, "database": secret},
		Message:  message,
		Human:    human,
		Warnings: all,
	})
}

// drillLines renders a drill result: a header, then one line per check.
// The restore check says where the copy recovered to, so the header names
// it only when no check does.
func drillLines(drill backup.Drill) string {
	header := fmt.Sprintf("%s: restore drill %s: backup %s", drill.Database, drill.Result, drill.BackupLabel)
	restoreChecked := false

	for _, c := range drill.Checks {
		if c.Name == "restore" && c.Detail != "" {
			restoreChecked = true
		}
	}

	if !restoreChecked && drill.RecoveredTo != "" {
		header += " recovered to " + drill.DescribeRecoveredTo()
	}

	out := make([]string, 1, 1+len(drill.Checks))
	out[0] = header

	for _, c := range drill.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAILED"
		}

		line := "  " + mark + "  " + c.Name
		if c.Detail != "" {
			line += ": " + c.Detail
		}

		out = append(out, line)
	}

	return strings.Join(out, "\n")
}
