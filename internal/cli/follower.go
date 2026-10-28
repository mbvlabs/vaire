package cli

import (
	"fmt"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/health"
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
			flagBool("allow-overcommit", "Follow even when the follower's size pushes the databases' sizes past the server's memory budget (warning memory_overcommitted instead of the refusal)"),
		},
		meta: CommandMeta{
			Summary:        "Make a read-only follower of a database: a physical standby that keeps streaming from it",
			WhenToUse:      []string{"Rehearse a risky migration on current production data, or read production data without load on it. The follower is a cluster of its own on the same server, with its own size and limits (the source's unless --size), copied from the source's stanza when it has a backup, else with pg_basebackup of the live source (works without backups), then streaming from the source through a replication slot (vaire_<name>) capped by max_slot_wal_keep_size. It is reachable at the same host as <name>, read-only, with the source's credentials (db1.<domain>:6432/<name> as <db>_ro or <db>_app); writes fail. It has no stanza and no backups of its own. Its connection limits are at least the source's (the hot-standby floors). Like db create, a size that overcommits the server is refused (memory_overcommitted) with the size that fits as a complete command; --allow-overcommit follows anyway. db list, status and doctor show its lag. When the rehearsal starts, vaire db unfollow makes it an independent database."},
			WhenNotToUse:   []string{"For a copy at a past time, or one to write to at once: vaire db fork."},
			Examples:       []string{"vaire db follow app", "vaire db follow app --name app_rehearsal --size small", "vaire db follow app --dry-run"},
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
			WhenToUse:      []string{"Start the migration rehearsal on a follower: it stops following for good, is promoted, its database and roles are renamed from the source's to its own with new passwords (printed once), and with backups on it gets its own stanza and first full backup (warning first_backup_failed when that fails). Its clients, logged in as the source's roles, are disconnected. The source is never touched. Refused for a database that follows nothing (not_a_follower) or whose cluster is down (follower_not_running)."},
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
		state = "streaming, lag " + humanBytes(res.Follower.LagBytes)
	}
	return output.Success(cmd, output.Result{
		Data: map[string]any{"job_id": jr.JobID, "source": src, "name": name, "from": res.From, "follower": res.Follower},
		Message: fmt.Sprintf("%s follows %s (%s). Read it at the server's host as %s with %s's credentials (%s_ro or %s_app); it is read-only.",
			name, src, state, name, src, src, src),
		Warnings: warnings,
		Next:     []string{"vaire status --db " + name, "vaire db unfollow " + name + " --confirm " + name + "/unfollow"},
	})
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
		return err
	}
	if dryRun(cmd) {
		var data map[string]any
		_ = reply.Decode(&data)
		return output.Success(cmd, output.Result{Data: data, Warnings: s.warnings,
			Message: fmt.Sprintf("dry run: would promote %s, stop it following %v, rename %v's database and roles to its own with new passwords, and give it its own stanza", name, data["source"], data["source"])})
	}
	if detached {
		p.warn("detached: the unfollow keeps running. These credentials work once it is done and are shown only now:")
		message, human := credentialsMessage(cmd, "unfollow detached; credentials shown only now", "unfollow detached; credentials shown only now", name, secret)
		return output.Success(cmd, output.Result{Data: map[string]any{"job_id": jobID, "detached": true, "database": secret},
			Message: message, Human: human, Next: []string{"vaire job follow " + jobID}})
	}
	var res struct {
		Source   string           `json:"source"`
		Stanza   string           `json:"stanza"`
		Warnings []output.Warning `json:"warnings"`
	}
	jr, err := decodeJob(reply, &res)
	if err != nil {
		return err
	}
	warnings := append(append(s.warnings, reply.Warnings...), res.Warnings...)
	headline := name + " no longer follows " + res.Source + ": it is an independent database with its own roles"
	message, human := credentialsMessage(cmd, headline, headline, name, secret)
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"job_id": jr.JobID, "source": res.Source, "stanza": res.Stanza, "database": secret},
		Message:  message,
		Human:    human,
		Warnings: warnings,
		Next:     []string{"vaire backup list --db " + name},
	})
}
