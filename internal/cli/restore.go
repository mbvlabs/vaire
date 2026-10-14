package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/spf13/cobra"
)

// recoveryKit returns the context's recovery kit.
func recoveryKit(name string) (*contexts.BackupCreds, error) {
	creds, err := contexts.LoadCredentials(name)
	if err != nil {
		return nil, err
	}
	if creds.Backup == nil || creds.Backup.AccessKeyID == "" {
		return nil, output.NewError(output.CodeInputRequired, "this context has no recovery kit", output.ExitInput,
			"Run eru backup setup, or log in from a transfer bundle that carries the recovery kit (eru login).").
			WithDetails(map[string]any{"reason": "recovery_kit_missing"})
	}
	return creds.Backup, nil
}

// dumpIdentity returns the kit's age identity for opening dumps, or an
// operator-side error when this machine's kit doesn't carry one.
func dumpIdentity(k *contexts.BackupCreds) (string, error) {
	if k.Dumps.AgeIdentity == "" {
		return "", output.NewError(output.CodeInputRequired, "this context's recovery kit has no dump age identity", output.ExitInput,
			"Dumps open with the age identity from the recovery kit. Run eru secrets export on the machine that ran eru backup setup, then eru login with that bundle here.").
			WithDetails(map[string]any{"reason": "recovery_kit_missing", "secret": steps.SecretBackupAgeIdentity})
	}
	return k.Dumps.AgeIdentity, nil
}

func kitTarget(k *contexts.BackupCreds) backup.Target {
	return backup.Target{Endpoint: k.Endpoint, Region: k.Region, Bucket: k.Bucket, Prefix: k.Prefix, URIStyle: k.URIStyle, KeyID: k.AccessKeyID, Secret: k.SecretAccessKey}
}

func runRestore(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return parentRun(cmd, args)
	}
	db := args[0]
	kinds := 0
	params := map[string]any{"database": db, "promote": flagTrue(cmd, "promote")}
	if flagTrue(cmd, "latest") {
		kinds++
		params["kind"] = backup.TargetLatest
	}
	if v := stringFlag(cmd, "set"); v != "" {
		kinds++
		params["kind"], params["set"] = backup.TargetSet, v
	}
	if v := stringFlag(cmd, "dump"); v != "" {
		kinds++
		params["kind"], params["dump"] = "dump", v
	}
	var target time.Time
	if v := stringFlag(cmd, "time"); v != "" {
		kinds++
		t, err := parseTarget(v, time.Now())
		if err != nil {
			return err
		}
		target = t
		params["kind"], params["time"] = backup.TargetTime, t.Format(time.RFC3339)
	}
	if kinds != 1 {
		return output.NewError(output.CodeUsage, "pass exactly one of --time, --latest, --set or --dump", output.ExitUsage,
			"For example eru restore "+db+" --time -15m.")
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
	if !target.IsZero() {
		p.note("restore target: " + echoTime(target, cfg.Timezone))
	}
	sec := map[string]string{}
	if params["kind"] == "dump" {
		kit, err := recoveryKit(s.ctx.Name)
		if err != nil {
			return err
		}
		ident, err := dumpIdentity(kit)
		if err != nil {
			return err
		}
		sec[steps.SecretBackupAgeIdentity] = ident
	}
	token := db + "/restore"
	if !dryRun(cmd) {
		if err := Confirm(cmd, token); err != nil {
			return err
		}
	}
	reply, jobID, detached, err := callJob(cmd, s, p, "restore.start", params, remote.CallOptions{Secrets: sec, Confirm: token, DryRun: dryRun(cmd)})
	if err != nil {
		return err
	}
	if detached {
		return detachedResult(cmd, s, jobID, "the restore")
	}
	if dryRun(cmd) {
		var data map[string]any
		_ = reply.Decode(&data)
		msg := fmt.Sprintf("dry run: would restore %s to %v beside the live database", db, data["target"])
		if tl, _ := data["timeline"].(string); tl != "" {
			msg += ", with --target-timeline=" + tl
		}
		return output.Success(cmd, output.Result{Data: data, Message: msg, Warnings: s.warnings})
	}
	var res struct {
		Phase         string   `json:"phase"`
		Restoring     string   `json:"restoring"`
		Warnings      []string `json:"warnings"`
		PoolerWarning string   `json:"pooler_warning"`
		Promoted      struct {
			Warning *output.Warning `json:"warning"`
		} `json:"promoted"`
	}
	jr, err := decodeJob(reply, &res)
	if err != nil {
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	for _, w := range res.Warnings {
		warnings = append(warnings, output.Warning{Code: "restore_dump_errors", Message: w})
	}
	if res.PoolerWarning != "" {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed", Message: res.PoolerWarning})
	}
	if res.Promoted.Warning != nil {
		warnings = append(warnings, *res.Promoted.Warning)
	}
	if res.Phase == "promoted" {
		return output.Success(cmd, output.Result{
			Data:     map[string]any{"job_id": jr.JobID, "result": res},
			Message:  db + " is restored and promoted, on a new timeline; a full backup is running. The replaced data is kept as a stopped cluster until eru restore cleanup " + db + "; eru restore undo " + db + " swaps it back.",
			Warnings: warnings,
			Next:     []string{"eru restore undo " + db, "eru restore cleanup " + db},
		})
	}
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
			"Inspect it with eru sql --db " + res.Restoring + " \"select ...\", then promote or abort.",
		}, "\n"),
		Warnings: warnings,
		Next:     []string{"eru restore status " + db, "eru restore promote " + db + " --confirm " + token, "eru restore abort " + db},
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
	reply, err := s.node.Call(commandContext(cmd), "restore.status", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Restores []struct {
			backup.Restore
			Restoring string `json:"restoring_database"`
			OnPromote string `json:"on_promote"`
			Tables    []struct {
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
		lines = append(lines, "leftover "+l.Name+" (from "+l.CreatedAt+"); drop it with eru restore cleanup "+l.Database)
	}
	if len(lines) == 0 {
		lines = []string{"no restore is open"}
	}
	return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: s.warnings})
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

// restoreAction runs promote, undo or abort for one database.
func restoreAction(cmd *cobra.Command, db, method, token, done string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if token != "" && !dryRun(cmd) {
		if err := Confirm(cmd, token); err != nil {
			return err
		}
	}
	reply, err := s.node.Call(commandContext(cmd), method, map[string]string{"database": db},
		remote.CallOptions{Confirm: token, DryRun: dryRun(cmd), OnEvent: p.event})
	if err != nil {
		return err
	}
	var data map[string]any
	if err := reply.Decode(&data); err != nil {
		return err
	}
	if dryRun(cmd) {
		done = "dry run: " + method + " " + db + " would run"
	}
	return output.Success(cmd, output.Result{Data: data, Message: done, Warnings: append(s.warnings, reply.Warnings...)})
}

func runRestorePromote(cmd *cobra.Command, args []string) error {
	db := args[0]
	return restoreAction(cmd, db, "restore.promote", db+"/restore",
		db+" now holds the restored data, on a new timeline; a full backup is running. The previous data is kept as a stopped cluster: eru restore undo "+db+" swaps it back, eru restore cleanup "+db+" drops it.")
}

func runRestoreUndo(cmd *cobra.Command, args []string) error {
	db := args[0]
	return restoreAction(cmd, db, "restore.undo", db+"/restore", db+" is back to its data from before the restore, on its old timeline; a full backup is running.")
}

func runRestoreAbort(cmd *cobra.Command, args []string) error {
	db := args[0]
	return restoreAction(cmd, db, "restore.abort", "", "dropped the temporary cluster "+db+"__restoring; the live "+db+" was never touched")
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
	reply, err := s.node.Call(ctx, "restore.cleanup", map[string]any{"database": db}, remote.CallOptions{OnEvent: p.event})
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
	lines := make([]string, 0, len(data.Leftovers))
	for _, l := range data.Leftovers {
		lines = append(lines, fmt.Sprintf("  %-50s %10s  %s", l.Name, humanBytes(l.SizeBytes), l.CreatedAt))
	}
	if len(lines) == 0 {
		return output.Success(cmd, output.Result{Data: data, Message: "no restore leftovers", Warnings: s.warnings})
	}
	if db == "" || dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: data, Message: "restore leftovers:\n" + strings.Join(lines, "\n"), Warnings: s.warnings,
			Next: []string{"eru restore cleanup <db> --confirm <db>/cleanup"}})
	}
	p.panel("Leftovers of "+db, lines)
	token := db + "/cleanup"
	if err := Confirm(cmd, token); err != nil {
		return err
	}
	reply, err = s.node.Call(ctx, "restore.cleanup", map[string]any{"database": db, "drop": true}, remote.CallOptions{Confirm: token, OnEvent: p.event})
	if err != nil {
		return err
	}
	var res map[string]any
	_ = reply.Decode(&res)
	return output.Success(cmd, output.Result{Data: res, Message: fmt.Sprintf("dropped %d leftover cluster(s) of %s", len(lines), db), Warnings: s.warnings})
}

func runRestoreDrill(cmd *cobra.Command, _ []string) error {
	at := ""
	if v := stringFlag(cmd, "time"); v != "" {
		t, err := parseTarget(v, time.Now())
		if err != nil {
			return err
		}
		at = t.Format(time.RFC3339)
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	params := func(db string) any { return map[string]string{"time": at, "database": db} }
	if dryRun(cmd) {
		reply, err := s.node.Call(commandContext(cmd), "restore.drill", params(stringFlag(cmd, "db")), remote.CallOptions{DryRun: true, OnEvent: p.event})
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
			out := []string{fmt.Sprintf("%s: restore drill %s: backup %s recovered to %s", drill.Database, drill.Result, drill.BackupLabel, drill.RecoveredTo)}
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
			return drill, strings.Join(out, "\n"), nil
		})
	if detached != nil {
		return detachedResult(cmd, s, *detached, "the drill"+notStarted(dbs, outcomes))
	}
	if err := failedOutcomes("the restore drill", "drills", outcomes); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"drills": outcomes},
		Message:  strings.Join(lines, "\n"),
		Warnings: append(s.warnings, warnings...),
		Next:     []string{"eru backup list"},
	})
}

func runDBFork(cmd *cobra.Command, args []string) error {
	src := args[0]
	name := stringFlag(cmd, "name")
	if name == "" {
		name = src + "_fork_" + time.Now().UTC().Format("200601021504")
	}
	if err := config.CheckDatabaseName(name); err != nil {
		return output.NewError(output.CodeValidation, "database name "+name+" "+err.Error(), output.ExitConfig, "Pass a shorter --name.")
	}
	params := map[string]string{"source": src, "name": name}
	var at time.Time
	if v := stringFlag(cmd, "at"); v != "" {
		t, err := parseTarget(v, time.Now())
		if err != nil {
			return err
		}
		at = t
		params["at"] = t.Format(time.RFC3339)
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
	if !at.IsZero() {
		p.note("fork point: " + echoTime(at, cfg.Timezone))
	}
	secret, rpc, err := newDatabaseSecret(cfg, name, steps.DatabaseRoles)
	if err != nil {
		return err
	}
	service, _, generated, err := serviceSecrets(s.ctx.Name)
	if err != nil {
		return err
	}
	for k, v := range service {
		rpc[k] = v
	}
	s.warnings = append(s.warnings, staleWarning(generated)...)
	reply, jobID, detached, err := callJob(cmd, s, p, "db.fork", params, remote.CallOptions{Secrets: rpc, DryRun: dryRun(cmd)})
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: params, Message: "dry run: would fork " + src + " into " + name, Warnings: s.warnings})
	}
	if detached {
		// The passwords are set in the copy before the job ends; they are
		// only shown here.
		p.warn("detached: the fork keeps running. These credentials work once it is done and are shown only now:")
		return output.Success(cmd, output.Result{Data: map[string]any{"job_id": jobID, "detached": true, "database": secret},
			Message: secret.String() + "\n" + shownOnceNote, Next: []string{"eru job follow " + jobID}})
	}
	var res struct {
		Warnings []output.Warning `json:"warnings"`
	}
	jr, err := decodeJob(reply, &res)
	if err != nil {
		return err
	}
	warnings := append(append(s.warnings, reply.Warnings...), res.Warnings...)
	from := "the live " + src
	if !at.IsZero() {
		from = src + " as of " + at.Format(time.RFC3339)
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"job_id": jr.JobID, "database": secret},
		Message:  fmt.Sprintf("forked %s into %s with its own roles\n%s\n%s", from, name, secret, shownOnceNote),
		Warnings: warnings,
	})
}

func runDump(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return parentRun(cmd, args)
	}
	db := args[0]
	to := stringFlag(cmd, "to")
	if to == "" {
		to = "local"
	}
	if to != "local" && to != "bucket" {
		return output.NewError(output.CodeUsage, "--to must be local or bucket", output.ExitUsage, "")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if to == "bucket" {
		reply, jobID, detached, err := callJob(cmd, s, p, "dump.create", map[string]string{"database": db}, remote.CallOptions{})
		if err != nil {
			return err
		}
		if detached {
			return detachedResult(cmd, s, jobID, "the dump")
		}
		var res struct {
			Dump backup.Dump `json:"dump"`
		}
		if _, err := decodeJob(reply, &res); err != nil {
			return err
		}
		return output.Success(cmd, output.Result{Data: res, Message: fmt.Sprintf("dump %s of %s is in the bucket (%s, age-encrypted)", res.Dump.ID, db, humanBytes(res.Dump.Size)), Warnings: s.warnings})
	}
	out := stringFlag(cmd, "out")
	if out == "" {
		out = backup.DumpFileName(db, time.Now().UTC().Format("20060102T150405Z"))
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator chose the output path.
	if err != nil {
		return output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Pick a path that does not exist yet with --out.")
	}
	p.note("streaming pg_dump -Fc of " + db + " to " + out)
	streamErr := s.node.Stream(commandContext(cmd), "dump "+db, f)
	if cerr := f.Close(); streamErr == nil {
		streamErr = cerr
	}
	if streamErr != nil {
		_ = os.Remove(out)
		return streamErr
	}
	info, _ := os.Stat(out)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}
	return output.Success(cmd, output.Result{Data: map[string]any{"database": db, "out": out, "size": size},
		Message: fmt.Sprintf("wrote %s (%s); restore it with pg_restore", out, humanBytes(size)), Warnings: s.warnings})
}

func runDumpList(cmd *cobra.Command, args []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	kit, err := recoveryKit(c.Name)
	if err != nil {
		return err
	}
	db := ""
	if len(args) > 0 {
		db = args[0]
	}
	dumps, err := backup.ListDumps(commandContext(cmd), kitTarget(kit), db)
	if err != nil {
		return err
	}
	lines := make([]string, 0, len(dumps))
	for _, d := range dumps {
		lines = append(lines, fmt.Sprintf("  %-20s %-24s %10s  %s", d.Database, d.ID, humanBytes(d.Size), d.Created))
	}
	if len(lines) == 0 {
		lines = []string{"no dumps in the bucket"}
	}
	return output.Success(cmd, output.Result{Data: map[string]any{"dumps": dumps}, Message: strings.Join(lines, "\n")})
}

func runDumpGet(cmd *cobra.Command, args []string) error {
	db, id := args[0], args[1]
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	kit, err := recoveryKit(c.Name)
	if err != nil {
		return err
	}
	out := stringFlag(cmd, "out")
	if err := RequireFlag(cmd, out != "", "--out", "dump get"); err != nil {
		return err
	}
	if out == "" {
		out = backup.DumpFileName(db, id)
	}
	ident, err := dumpIdentity(kit)
	if err != nil {
		return err
	}
	n, err := backup.DownloadDump(commandContext(cmd), kitTarget(kit), db, id, ident, out)
	if err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: map[string]any{"database": db, "id": id, "out": out, "size": n},
		Message: fmt.Sprintf("decrypted dump %s of %s to %s (%s); read it with pg_restore --list", id, db, out, humanBytes(n))})
}
