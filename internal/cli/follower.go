package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/health"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/spf13/cobra"
)

// vaire db follow and vaire db unfollow (Phase 6): a read-only physical standby
// of one database, and its promotion into an independent database.

func dbFollowCommand() cmdSpec {
	return cmdSpec{
		use:  "follow <db>",
		args: cobra.ExactArgs(1),
		run:  runDBFollow,
		flags: []fl{
			flagString("name", "", "The follower's name (default <db>_follower)"),
			flagString("size", "", "Size of the follower's cluster: nano, small, medium, large, xlarge or custom (default the source's size and memory)"),
			flagString("cpu-mode", "", "CPU scheduling: normal or spare"),
			flagString("cpu-quota", "", "CPU ceiling as a percentage of one CPU, or none"),
			flagBool("allow-overcommit", "Follow even when the follower's size pushes the databases' sizes past the server's memory budget (warning memory_overcommitted instead of the refusal)"),
		},
		meta: CommandMeta{
			Summary:        "Make a read-only follower of a database: a physical standby that keeps streaming from it",
			WhenToUse:      []string{"Rehearse a risky migration on current production data, or read production data without load on it. The follower is a cluster of its own on the same server, with its own size and limits (the source's unless --size), copied from the source's stanza when it has a backup, else with pg_basebackup of the live source (works without backups), then streaming from the source through a replication slot (vaire_<name>) capped by max_slot_wal_keep_size. It is reachable at the same host as <name>, read-only, with the source's credentials (db1.<domain>:6432/<name> as <db>_ro or <db>_app); writes fail. It has no stanza and no backups of its own. Its connection limits are at least the source's (the hot-standby floors). Like db create, a size that overcommits the server is refused (memory_overcommitted) with the size that fits as a complete command; --allow-overcommit follows anyway. db list, status and doctor show its lag. It does not inherit the source's protection: a follower is unprotected unless you protect it (vaire db protect). When the rehearsal starts, vaire db unfollow makes it an independent database."},
			WhenNotToUse:   []string{"For a copy at a past time, or one to write to at once: vaire db fork."},
			Examples:       []string{"vaire db follow app", "vaire db follow app --name app_rehearsal --cpu-mode spare --cpu-quota 150%", "vaire db follow app --dry-run"},
			Mutating:       true,
			SupportsDryRun: true,
			Remote:         true,
			ErrorCodes:     []string{"precondition_failed", "validation_failed", "input_required"},
			Next:           []string{"vaire db list", "vaire status --db <name>", "vaire db unfollow <name> --confirm <name>/unfollow"},
		},
	}
}

func dbUnfollowCommand() cmdSpec {
	return cmdSpec{
		use:  "unfollow <follower>",
		args: cobra.ExactArgs(1),
		run:  runDBUnfollow,
		meta: CommandMeta{ //nolint:gosec // G101: help text, not a credential.
			Summary:        "Promote a follower into an independent database with its own roles and stanza",
			WhenToUse:      []string{"Start the migration rehearsal on a follower: it stops following for good, is promoted, its database and roles are renamed from the source's to its own with new passwords (printed once), and with backups on it gets its own stanza and first full backup (warning first_backup_failed when that fails). Its clients, logged in as the source's roles, are disconnected. The source is never touched. Like a follower, the unfollowed copy does not inherit the source's protection; it keeps only the protection the operator gave it (vaire db protect). Refused for a database that follows nothing (not_a_follower) or whose cluster is down (follower_not_running)."},
			Examples:       []string{"vaire db unfollow app_follower --confirm app_follower/unfollow", "vaire db unfollow app_follower --dry-run"},
			Mutating:       true,
			Destructive:    true,
			SupportsDryRun: true,
			Remote:         true,
			ConfirmToken:   "{db}/unfollow",
			ErrorCodes:     []string{"unsafe_action_requires_confirmation", "precondition_failed", "input_required"},
			Next:           []string{"vaire backup list --db <db>", "vaire db list"},
		},
	}
}

func runDBFollow(cmd *cobra.Command, args []string) error {
	src := args[0]
	name := stringFlag(cmd, "name")
	if name == "" {
		name = src + "_follower"
	}
	if err := config.CheckDatabaseName(name); err != nil {
		return output.NewError(output.CodeValidation, "database name "+name+" "+err.Error(), output.ExitConfig, "Pass a shorter --name.")
	}
	params := map[string]any{"source": src, "name": name}
	addCPUFlags(cmd, params)
	size := stringFlag(cmd, "size")
	if size != "" {
		params["size"] = size
	}
	if flagTrue(cmd, "allow-overcommit") {
		params["allow_overcommit"] = true
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if size != "" {
		cfg, _, err := remoteConfig(cmd, s, p)
		if err != nil {
			return err
		}
		if spec, ok := cfg.ClusterByName(src); ok {
			spec.Size, spec.Memory = size, ""
			if err := checkSizeFlags(cfg, spec); err != nil {
				return err
			}
		}
	}
	// The follower's steps run in the job: what an apply sends, so a step
	// that finds a service password missing on the node takes it from here.
	service, _, generated, err := serviceSecrets(s.ctx.Name)
	if err != nil {
		return err
	}
	s.warnings = append(s.warnings, staleWarning(generated)...)
	reply, jobID, detached, err := callJob(cmd, s, p, "db.follow", params, remote.CallOptions{DryRun: dryRun(cmd), Secrets: service})
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		var data map[string]any
		_ = reply.Decode(&data)
		return output.Success(cmd, output.Result{Data: data, Warnings: append(s.warnings, reply.Warnings...),
			Message: fmt.Sprintf("dry run: would follow %s as %s, copied from %v, streaming through the slot %v", src, name, data["from"], data["slot"])})
	}
	if detached {
		return detachedResult(cmd, s, jobID, "the follower "+name)
	}
	var res struct {
		Follower health.FollowerState `json:"follower"`
		From     string               `json:"from"`
		Warnings []output.Warning     `json:"warnings"`
	}
	jr, err := decodeJob(reply, &res)
	if err != nil {
		return err
	}
	warnings := append(append(s.warnings, reply.Warnings...), res.Warnings...)
	state := "not streaming yet"
	if res.Follower.Streaming {
		state = "streaming, lag " + followerLagText(&res.Follower)
	}
	return output.Success(cmd, output.Result{
		Data: map[string]any{"job_id": jr.JobID, "source": src, "name": name, "from": res.From, "follower": res.Follower},
		Message: fmt.Sprintf("%s follows %s (%s). Read it at the server's host as %s with %s's credentials (%s_ro or %s_app); it is read-only.",
			name, src, state, name, src, src, src),
		Warnings: warnings,
		Next:     []string{"vaire status --db " + name, "vaire db unfollow " + name + " --confirm " + name + "/unfollow"},
	})
}

func addCPUFlags(cmd *cobra.Command, params map[string]any) {
	if cmd.Flags().Changed("cpu-mode") {
		params["cpu_mode"] = stringFlag(cmd, "cpu-mode")
	}
	if cmd.Flags().Changed("cpu-quota") {
		params["cpu_quota"] = stringFlag(cmd, "cpu-quota")
	}
}

func runDBUnfollow(cmd *cobra.Command, args []string) error {
	name := args[0]
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	opts := remote.CallOptions{DryRun: dryRun(cmd)}
	if !dryRun(cmd) {
		token := name + "/unfollow"
		if err := Confirm(cmd, token); err != nil {
			return err
		}
		opts.Confirm = token
	}
	cfg, _, err := remoteConfig(cmd, s, p)
	if err != nil {
		return err
	}
	secret, rpcSecrets, err := newDatabaseSecret(cfg, name, steps.DatabaseRoles)
	if err != nil {
		return err
	}
	service, _, generated, err := serviceSecrets(s.ctx.Name)
	if err != nil {
		return err
	}
	for k, v := range service {
		rpcSecrets[k] = v
	}
	s.warnings = append(s.warnings, staleWarning(generated)...)
	opts.Secrets = rpcSecrets
	reply, jobID, detached, err := callJob(cmd, s, p, "db.unfollow", map[string]any{"name": name}, opts)
	if err != nil {
		// The node marks every failure of an unfollow that started, and a lost
		// connection leaves its outcome unknown: the passwords may be on the
		// database, so they are handed over here (D10).
		note, out := unfollowCredentials(err, name, secret, dryRun(cmd))
		if note != "" {
			printCredentialBlock(cmd, note, name, secret)
		}
		return out
	}
	if dryRun(cmd) {
		var data map[string]any
		_ = reply.Decode(&data)
		return output.Success(cmd, output.Result{Data: data, Warnings: s.warnings,
			Message: fmt.Sprintf("dry run: would promote %s, stop it following %v, rename %v's database and roles to its own with new passwords, and give it its own stanza; CPU priority and quota continue", name, data["source"], data["source"])})
	}
	if detached {
		p.warn("detached: the unfollow keeps running. These credentials work once it is done and are shown only now:")
		message, human := credentialsMessage(cmd, "unfollow detached; CPU priority and quota continue; credentials shown only now", "unfollow detached; CPU priority and quota continue; credentials shown only now", name, secret)
		data := map[string]any{"job_id": jobID, "detached": true, "database": secret, "cpu_policy_continues": true}
		if spec, ok := cfg.ClusterByName(name); ok {
			if res, err := cfg.Resources(spec); err == nil {
				data["cpu_policy_at_request"] = map[string]string{"mode": res.CPUMode, "quota": res.CPUQuota}
			}
		}
		return output.Success(cmd, output.Result{Data: data,
			Message: message, Human: human, Next: []string{"vaire job follow " + jobID}})
	}
	var res struct {
		Source    string            `json:"source"`
		Stanza    string            `json:"stanza"`
		Warnings  []output.Warning  `json:"warnings"`
		CPUPolicy map[string]string `json:"cpu_policy"`
	}
	jr, err := decodeJob(reply, &res)
	if err != nil {
		return err
	}
	warnings := append(append(s.warnings, reply.Warnings...), res.Warnings...)
	quota := res.CPUPolicy["quota"]
	if quota == "" {
		quota = "none"
	}
	headline := name + " no longer follows " + res.Source + ": it is an independent database with its own roles; CPU mode " + res.CPUPolicy["mode"] + " and quota " + quota + " continue"
	message, human := credentialsMessage(cmd, headline, headline, name, secret)
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"job_id": jr.JobID, "source": res.Source, "stanza": res.Stanza, "database": secret, "cpu_policy": res.CPUPolicy},
		Message:  message,
		Human:    human,
		Warnings: warnings,
		Next:     []string{"vaire backup list --db " + name},
	})
}

// unfollowCredentials hands a failed db unfollow's generated passwords to the
// operator when the node may have installed them already: it returns the note
// that goes above the credential block, and the failure to report with the
// credentials in details.database (as db create's interrupted output does).
// The node's flag says they may have changed, never that they did, a lost
// connection leaves the outcome unknown, and so does a job whose process died
// without a result. Ordinary errors, the job's record and the activity history
// never carry the passwords, so nothing is handed over for a refusal the node
// answered before it started (a preflight failure, a missing confirmation) or
// for a dry run, which starts no job at all.
func unfollowCredentials(err error, name string, secret databaseSecret, dryRun bool) (string, error) {
	var cliErr *output.CLIError
	if dryRun || !errors.As(err, &cliErr) || !unfollowPasswordsUnknown(cliErr) {
		return "", err
	}
	note := "The unfollow failed after it may have changed " + name + "'s roles and passwords, so these are the credentials to try first. " +
		"If " + name + " still follows its source, they are unused and the next vaire db unfollow makes new ones."
	out := *cliErr
	details := map[string]any{}
	if m, ok := cliErr.Details.(map[string]any); ok {
		for k, v := range m {
			details[k] = v
		}
	}
	details["database"] = secret
	details["credentials_note"] = note + " " + shownOnceNote
	out.Details = details
	out.Hint = strings.TrimSpace(out.Hint + " If they don't work, run vaire db unfollow " + name + " again; it finishes the unfollow and prints new ones.")
	return note, &out
}

// unfollowPasswordsUnknown reports whether err may have installed the
// unfollow's new passwords: the node marks every failure of a job it started,
// a lost connection leaves the outcome unknown, and a job whose process died
// without recording a result may have stopped anywhere.
func unfollowPasswordsUnknown(cliErr *output.CLIError) bool {
	if cliErr.Code == output.CodeTransportFailed {
		return true
	}
	details, _ := cliErr.Details.(map[string]any)
	if changed, _ := details[node.UnfollowPasswordsDetail].(bool); changed {
		return true
	}
	reason, _ := details["reason"].(string)
	return reason == node.JobOutcomeUnknown
}

// printCredentials writes the credential block of a failed unfollow to the
// human output: structured output carries it in details.database instead.
func printCredentialBlock(cmd *cobra.Command, note, title string, secret databaseSecret) {
	opts, _ := output.ParseOptions(cmd)
	if output.UsesStructuredOutput(opts) {
		return
	}
	text := note + "\n" + secret.String() + "\n" + shownOnceNote
	if width, ok := styledStdout(cmd); ok {
		text = ui.Warn(note) + "\n" + secret.styled("", title, shownOnceNote, width)
	}
	resume := ui.Suspend()
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), text)
	resume()
}
