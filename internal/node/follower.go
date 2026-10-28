package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

// Followers (Phase 6): vaire db follow makes a read-only physical standby of
// one database's cluster, a cluster of its own on the same node that
// streams from its source over the source's socket through a physical
// slot. vaire db unfollow promotes it into an independent database with the
// fork's renames, its own stanza and first full.

// Where a follower's copy comes from (D2).
const (
	followFromStanza     = "stanza"
	followFromBaseBackup = "base_backup"
)

// streamWait is how long db follow waits for a new follower to stream
// before it warns follower_not_streaming.
const streamWait = time.Minute

// dbFollowParams are db.follow's params, and the db-follow job's.
type dbFollowParams struct {
	Source string `json:"source"`
	Name   string `json:"name,omitempty"`
	// Size replaces the source's size (and its memory override); empty
	// keeps the source's.
	Size string `json:"size,omitempty"`
	// AllowOvercommit follows even when the follower's size pushes the
	// databases' sizes past the budget, with the memory_overcommitted
	// warning.
	AllowOvercommit bool `json:"allow_overcommit,omitempty"`
	// From is where the job copies from (followFromStanza or
	// followFromBaseBackup); the RPC decides it.
	From string `json:"from,omitempty"`
	// Recover is db recover of a follower awaiting recovery (D14): its
	// entry is in cluster.toml already.
	Recover bool `json:"recover,omitempty"`
}

// dbUnfollowParams are db.unfollow's params, and the db-unfollow job's.
type dbUnfollowParams struct {
	Name string `json:"name"`
}

// followerName is the follower's name: --name, or <db>_follower.
func followerName(p dbFollowParams) string {
	if p.Name != "" {
		return p.Name
	}
	return p.Source + "_follower"
}

// --- refusals ---

// followerError refuses action on follower spec (D9).
func followerError(spec config.ClusterSpec, action string) error {
	return output.NewError(output.CodePrecondition,
		spec.Name+" is a follower of "+spec.Follows+", so "+action+" is refused", output.ExitPrecondition,
		"Run it on "+spec.Follows+", or make "+spec.Name+" independent first: vaire db unfollow "+spec.Name+" --confirm "+spec.Name+"/unfollow.").
		WithDetails(map[string]any{"reason": "is_follower", "database": spec.Name, "source": spec.Follows})
}

// followerRefusal is one method a follower refuses: the action named in
// the refusal, the params field naming the database, and whether the
// request is one that needs refusing (nil: always).
type followerRefusal struct {
	action  string
	field   string
	applies func(params map[string]any) bool
}

// followerRefused are the methods that need a stanza or a writable
// database (D9). db.follow of a follower is refused by its handler, with
// follows_follower.
var followerRefused = map[string]followerRefusal{
	"backup.now":     {action: "a backup", field: "database"},
	"backup.verify":  {action: "a verify", field: "database"},
	"backup.list":    {action: "listing backups", field: "database"},
	"restore.start":  {action: "a restore", field: "database"},
	"restore.drill":  {action: "a restore drill", field: "database"},
	"dump.create":    {action: "a dump", field: "database"},
	"db.fork":        {action: "a fork", field: "source"},
	"maint.run":      {action: "maintenance", field: "db"},
	"db.credentials": {action: "rotating its credentials", field: "name", applies: func(p map[string]any) bool { r, _ := p["rotate"].(bool); return r }},
}

// refuseFollower refuses req when its method needs a stanza or a writable
// database and the database it names is a follower (D9).
func refuseFollower(req rpc.Request, cfg config.Cluster) error {
	r, ok := followerRefused[req.Method]
	if !ok {
		return nil
	}
	var params map[string]any
	if json.Unmarshal(req.Params, &params) != nil {
		return nil
	}
	if r.applies != nil && !r.applies(params) {
		return nil
	}
	name, _ := params[r.field].(string)
	if spec, ok := cfg.ClusterByName(name); ok && spec.IsFollower() {
		return followerError(spec, r.action)
	}
	return nil
}

// refuseSourceDrop refuses dropping a database that has followers (D11).
func refuseSourceDrop(cfg config.Cluster, db string) error {
	followers := cfg.FollowerNames(db)
	if len(followers) == 0 {
		return nil
	}
	f := followers[0]
	return output.NewError(output.CodePrecondition,
		db+" has followers ("+strings.Join(followers, ", ")+"), so it can't be dropped", output.ExitPrecondition,
		"Drop each follower first (vaire db drop "+f+" --confirm "+f+"), or make it independent (vaire db unfollow "+f+" --confirm "+f+"/unfollow).").
		WithDetails(map[string]any{"reason": "has_followers", "database": db, "followers": followers})
}

// unfollowToken is db.unfollow's confirm token: <follower>/unfollow.
func unfollowToken(_ config.Cluster, params json.RawMessage) string {
	var p dbUnfollowParams
	if json.Unmarshal(params, &p) != nil || p.Name == "" {
		return ""
	}
	return p.Name + "/unfollow"
}

// --- the slot ---

// createFollowerSlot makes follower's physical slot on its source with its
// WAL reserved at once (D4). An inactive slot of the same name, left by a
// failed attempt, is dropped first.
func createFollowerSlot(ctx context.Context, src config.ClusterSpec, slot string) error {
	s := quoteLiteral(slot)
	sql := "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = " + s + " AND NOT active;\n" +
		"SELECT pg_create_physical_replication_slot(" + s + ", true) WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = " + s + ");\n"
	if err := steps.ClusterPG(src).Exec(ctx, "postgres", sql, nil); err != nil {
		return fmt.Errorf("create the replication slot %s on %s: %w", slot, src.Name, err)
	}
	return nil
}

// dropSlot drops slot on src, ending the walsender that still holds it
// (a follower that was just stopped or promoted lets go within seconds). A
// missing slot is fine.
func dropSlot(ctx context.Context, src config.ClusterSpec, slot string) error {
	s := quoteLiteral(slot)
	sql := "SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = " + s + " AND active;\n" +
		"SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = " + s + " AND NOT active;\n"
	local := steps.ClusterPG(src)
	var err error
	for i := 0; i < 10; i++ {
		if err = local.Exec(ctx, "postgres", sql, nil); err == nil {
			rows, qerr := local.Query(ctx, "postgres", "SELECT slot_name FROM pg_replication_slots WHERE slot_name = "+s, nil)
			if qerr == nil && len(rows) == 0 {
				return nil
			}
			err = errors.New("the slot " + slot + " is still active")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}

// dropFollowerSlot drops follower spec's slot on its source, as db drop
// and db unfollow do; a source that isn't running keeps it, with a warning
// naming the SQL.
func dropFollowerSlot(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) *output.Warning {
	src, ok := cfg.ClusterByName(spec.Follows)
	if !ok {
		return nil
	}
	slot := config.FollowerSlot(spec.Name)
	if clusterRunning(src) {
		if err := dropSlot(ctx, src, slot); err == nil {
			return nil
		}
	}
	return &output.Warning{Code: "slot_not_dropped", Message: fmt.Sprintf(
		"the replication slot %s on %s was not dropped and keeps WAL there until it is: run SELECT pg_drop_replication_slot('%s') in %s as postgres on the node",
		slot, src.Name, slot, src.Name)}
}

// --- db follow ---

// followFrom is where a follower of src is copied from (D2): the stanza
// when backups are on and it has a backup set, else pg_basebackup of the
// live source.
func followFrom(ctx context.Context, cfg config.Cluster, src config.ClusterSpec) string {
	if !cfg.BackupConfigured() || src.Stanza == "" {
		return followFromBaseBackup
	}
	info, err := driver(src, "db.follow", nil).Info(ctx, "")
	if err != nil {
		return followFromBaseBackup
	}
	if _, ok := info.Latest(); ok {
		return followFromStanza
	}
	return followFromBaseBackup
}

// followerSpec is a follower's cluster.toml entry before placeCluster gives
// it a node and port: the source's size, limits, pools, profile, statement
// timeout, extensions and settings (as a fork's), or size (D8).
func followerSpec(src config.ClusterSpec, name, size string) config.ClusterSpec {
	spec := forkSpec(src, name)
	spec.Follows = src.Name
	if size != "" {
		spec.Size, spec.Memory = size, ""
	}
	return spec
}

// followCommand is the db follow command at another size: what the
// memory_overcommitted refusal suggests.
func followCommand(p dbFollowParams) func(size string) string {
	return func(size string) string {
		return "vaire db follow " + p.Source + " --name " + followerName(p) + " --size " + size
	}
}

// checkFollowSource refuses a source that can't be followed: a follower
// (no cascading), one with an open restore, or one that isn't running.
func checkFollowSource(src config.ClusterSpec, action string) error {
	if src.IsFollower() {
		return output.NewError(output.CodePrecondition, src.Name+" is itself a follower of "+src.Follows+", and followers don't cascade", output.ExitPrecondition,
			"Follow its source instead: vaire db follow "+src.Follows+".").
			WithDetails(map[string]any{"reason": "follows_follower", "database": src.Name, "source": src.Follows})
	}
	if err := restoreInProgress(src.Name, action); err != nil {
		return err
	}
	if !clusterRunning(src) {
		return output.NewError(output.CodePrecondition, src.Name+"'s cluster is not running, so it can't be followed", output.ExitPrecondition,
			"Start it first (vaire apply --db "+src.Name+"), then retry.").
			WithDetails(map[string]any{"reason": "source_not_running", "database": src.Name})
	}
	return nil
}

// handleDBFollow checks the source and the name, then, under the exclusive
// lock, adds the follower to cluster.toml as db create with an explicit
// size does (a port, no stanza, the budget refusal) and starts the
// detachable job db-follow, which makes its cluster (D2).
func handleDBFollow(ctx context.Context, env *Env) (Result, error) {
	var p dbFollowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	p.Name, p.Recover = followerName(p), false
	cfg := env.Cluster
	src, ok := cfg.ClusterByName(p.Source)
	if !ok {
		return Result{}, databaseNotFound(p.Source)
	}
	if err := checkDatabaseName(p.Name); err != nil {
		return Result{}, err
	}
	if err := config.CheckDatabaseName(p.Name); err != nil {
		return Result{}, output.NewError(output.CodeValidation, "database name "+p.Name+" "+err.Error(), output.ExitConfig, "Pass a shorter --name.")
	}
	if err := checkFollowSource(src, "db follow"); err != nil {
		return Result{}, err
	}
	if _, ok := cfg.ClusterByName(p.Name); ok || steps.ClusterExists(ctx, p.Name) {
		return Result{}, existingDatabase(p.Name)
	}
	if _, err := restoreSpace(ctx, cfg, p.Source); err != nil {
		return Result{}, err
	}
	p.From = followFrom(ctx, cfg, src)
	if env.Req.DryRun {
		return Result{Data: map[string]any{"source": p.Source, "name": p.Name, "size": p.Size, "from": p.From,
			"slot": config.FollowerSlot(p.Name), "dry_run": true}}, nil
	}
	release, err := engine.Acquire(ctx, engine.LockExclusive, "db.follow", env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return Result{}, err
	}
	saved, _, created, err := addDatabase(ctx, env, followerSpec(src, p.Name, p.Size), p.AllowOvercommit, followCommand(p))
	release()
	if err != nil {
		return Result{}, err
	}
	env.Cluster = saved
	id, err := startJob(ctx, env, "db-follow", p, stepSecrets(env.Req.Secrets))
	if err != nil {
		// The job removes the entry when it fails; one that never started
		// can't.
		return Result{}, errors.Join(err, forgetDatabase(p.Name))
	}
	res, err := followJob(ctx, env, id)
	if err != nil {
		return res, err
	}
	if m, ok := res.Data.(map[string]any); ok {
		m["database"] = created
	}
	res.Warnings = append(res.Warnings, oversubscriptionWarnings(saved, runtimeFor(env, false).Host)...)
	return res, nil
}

// jobFollow makes a follower's cluster (D2): the slot on the source, the
// copy from the source's stanza or with pg_basebackup, adopted as
// 18/<name> with its whole configuration written before its first start,
// then its own steps (the cluster starts and streams), the poolers,
// metrics and the other databases' changed configuration. A failure before
// the follower is 18/<name> drops the copy, the slot and the entry (or, for
// db recover, leaves it awaiting recovery again).
func jobFollow(ctx context.Context, env *jobEnv) (any, error) {
	var p dbFollowParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	cfg := env.Cluster
	spec, ok := cfg.ClusterByName(p.Name)
	if !ok || !spec.IsFollower() {
		return nil, output.NewError(output.CodePrecondition, p.Name+" is not a follower in cluster.toml", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "not_a_follower", "database": p.Name})
	}
	src, err := clusterOf(cfg, spec.Follows)
	if err != nil {
		return nil, err
	}
	var awaiting backup.Awaiting
	if p.Recover {
		// The steps see it from here on (Phase 5's engine view hides a
		// database awaiting recovery).
		if awaiting, err = clearAwaiting(p.Name); err != nil {
			return nil, err
		}
	}
	rt := jobRuntime(env)
	if err := materialiseFollower(ctx, env, cfg, src, spec, p.From, rt.Host); err != nil {
		if p.Recover {
			err = errors.Join(err, markAwaiting(p.Name, awaiting))
		} else if ferr := forgetDatabase(p.Name); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return nil, err
	}
	others := otherConfigs(ctx, steps.Catalog(), cfg, rt, p.Name, "")
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, followRuns(p.Name, others)...))
	logLine(env.Report, env.Job, "starting "+p.Name+" as a standby of "+src.Name)
	_, _, err = engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Restarter: steps.Restarter(cfg), Report: env.Report})
	if err == nil {
		err = applyNewCluster(ctx, cfg, rt, p.Name, env.Report)
	}
	if err != nil {
		return nil, withHint(err, finishNewCluster(p.Name))
	}
	var warnings []output.Warning
	if err := applyPoolers(ctx, cfg, rt, env.Report); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the poolers were not reloaded, so " + p.Name + " is not reachable through them until vaire apply: " + err.Error()})
	}
	if w := applyMetrics(ctx, cfg, rt, env.Report, p.Name); w != nil {
		warnings = append(warnings, *w)
	}
	warnings = append(warnings, applyOtherConfigs(ctx, steps.Catalog(), cfg, rt, others,
		engine.Options{Restarter: steps.Restarter(cfg), Report: env.Report})...)
	logLine(env.Report, env.Job, "waiting for "+p.Name+" to stream from "+src.Name)
	st := waitStreaming(ctx, spec, src, streamWait)
	if !st.Streaming {
		warnings = append(warnings, output.Warning{Code: "follower_not_streaming",
			Message: p.Name + " is not streaming from " + src.Name + " yet: vaire doctor --db " + p.Name + " and vaire logs postgres --db " + p.Name + " show why"})
	}
	writeFollowerMetrics(ctx, cfg)
	return map[string]any{"source": src.Name, "follows": src.Name, "name": p.Name, "port": spec.Port, "from": p.From,
		"slot": config.FollowerSlot(p.Name), "follower": st, "warnings": warnings}, nil
}

// followRuns are the applies db follow runs, for the request's step
// numbering: the kernel's huge pages, the follower's own steps, the poolers,
// metrics and the other databases' changed configuration. A follower has no
// stanza and no timers.
func followRuns(name string, others []string) []engine.Options {
	runs := []engine.Options{{Only: steps.KernelStepIDs()}, {DB: name}, {Only: steps.PoolerStepIDs()}, {Only: steps.MetricsStepIDs()}}
	if len(others) > 0 {
		runs = append(runs, engine.Options{Only: others})
	}
	return runs
}

// materialiseFollower builds the follower's data in the temporary cluster
// <name> from the source's stanza or with pg_basebackup through its slot,
// drops pgBackRest's recovery settings, adopts it as 18/<name> on spec's
// port and writes its whole configuration. A failure drops what was built
// and the slot.
func materialiseFollower(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec, from string, host hostinfo.Info) error {
	slot := config.FollowerSlot(spec.Name)
	logLine(env.Report, env.Job, "creating the replication slot "+slot+" on "+src.Name)
	if err := createFollowerSlot(ctx, src, slot); err != nil {
		return err
	}
	m, err := backup.NewMaterialised(cfg.Server.DataRoot, spec.Name, src.Name, src.Port, env.shell(), env.Job, env.Report)
	if err == nil {
		if from == followFromStanza {
			recs, _ := backup.LoadRecords()
			err = m.Restore(ctx, driver(src, env.Job, env.Report), backup.RestoreOptions{
				Kind: backup.TargetLatest, Pause: true, Timeline: recs.TargetTimeline(src.Name, src.Stanza),
			})
		} else {
			err = m.StandbyBaseBackup(ctx, slot)
		}
		if err == nil {
			err = backup.DropRecoverySettings(m.Data)
		}
		if err == nil {
			err = m.Adopt(ctx, spec.Port)
		}
		if err == nil {
			err = steps.WriteFollowerConf(cfg, spec, host)
		}
		if err != nil {
			err = errors.Join(err, m.Drop())
		}
	}
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := dropSlot(cctx, src, slot); derr != nil {
			err = errors.Join(err, derr)
		}
	}
	return err
}

// waitStreaming reads spec's state until it streams from src, for at most
// wait.
func waitStreaming(ctx context.Context, spec, src config.ClusterSpec, wait time.Duration) followerReading {
	deadline := time.Now().Add(wait)
	for {
		st := readFollower(ctx, spec, src)
		if st.Streaming || time.Now().After(deadline) {
			return st
		}
		select {
		case <-ctx.Done():
			return st
		case <-time.After(2 * time.Second):
		}
	}
}

// markAwaiting records db as awaiting recovery again with its earlier
// record: a follower whose recovery failed before its cluster was made.
func markAwaiting(db string, prev backup.Awaiting) error {
	return backup.UpdateRecords(func(s *backup.Records) error {
		if s.AwaitingRecovery == nil {
			s.AwaitingRecovery = map[string]backup.Awaiting{}
		}
		s.AwaitingRecovery[db] = prev
		return nil
	})
}

// --- db unfollow ---

// handleDBUnfollow checks the follower and the new passwords, then runs the
// detachable job db-unfollow (D10).
func handleDBUnfollow(ctx context.Context, env *Env) (Result, error) {
	var p dbUnfollowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	spec, ok := cfg.ClusterByName(p.Name)
	if !ok {
		return Result{}, databaseNotFound(p.Name)
	}
	if !spec.IsFollower() {
		return Result{}, output.NewError(output.CodePrecondition, p.Name+" follows nothing, so there is nothing to unfollow", output.ExitPrecondition,
			"vaire db list shows the followers (state following).").
			WithDetails(map[string]any{"reason": "not_a_follower", "database": p.Name})
	}
	if env.Req.Confirm == "" && !env.Req.DryRun {
		return Result{}, confirmRequired("unfollowing "+p.Name+" promotes it for good: it stops following "+spec.Follows+" and its credentials change", p.Name+"/unfollow")
	}
	if !clusterRunning(spec) {
		return Result{}, output.NewError(output.CodePrecondition, p.Name+"'s cluster is not running, so it can't be promoted", output.ExitPrecondition,
			"Start it first (vaire apply --db "+p.Name+"), then retry.").
			WithDetails(map[string]any{"reason": "follower_not_running", "database": p.Name})
	}
	jobSecrets := stepSecrets(env.Req.Secrets)
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(p.Name, role)
		if env.Req.Secrets[key] == "" && !env.Req.DryRun {
			return Result{}, output.NewError(output.CodeInputRequired, "the password for "+p.Name+"_"+role+" was not supplied", output.ExitInput,
				"Generate it on the operator and pass it as "+key+".").WithDetails(map[string]any{"secret": key})
		}
		jobSecrets[key] = env.Req.Secrets[key]
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"name": p.Name, "source": spec.Follows, "slot": config.FollowerSlot(p.Name),
			"renames": unfollowRenames(spec), "backups": cfg.BackupConfigured(), "dry_run": true}}, nil
	}
	return runJobRPC(ctx, env, "db-unfollow", p, jobSecrets)
}

// unfollowRenames are the renames unfollow makes, for the dry run.
func unfollowRenames(spec config.ClusterSpec) map[string]string {
	out := map[string]string{spec.Follows: spec.Name}
	for _, role := range append([]string{"owner"}, steps.DatabaseRoles...) {
		out[spec.Follows+"_"+role] = spec.Name + "_" + role
	}
	return out
}

// jobUnfollow promotes a follower into an independent database (D10): its
// pooler clients are cut (they logged in as the source's roles), it is
// promoted, the source's database and roles are renamed to its own with
// the operator's new passwords, cluster.toml gives it a stanza instead of
// follows, the slot on the source goes, then its own steps restart it
// without the floors, the poolers route to it by its own name, and with
// backups on it gets its stanza, first full and timers.
func jobUnfollow(ctx context.Context, env *jobEnv) (any, error) {
	var p dbUnfollowParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	cfg := env.Cluster
	spec, ok := cfg.ClusterByName(p.Name)
	if !ok || !spec.IsFollower() {
		return nil, output.NewError(output.CodePrecondition, p.Name+" follows nothing", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "not_a_follower", "database": p.Name})
	}
	src := spec.Follows
	local := steps.ClusterPG(spec)
	resume := sync.OnceFunc(steps.KillPoolerDatabase(ctx, p.Name))
	defer resume()

	logLine(env.Report, env.Job, "promoting "+p.Name)
	if _, err := backup.Promote(ctx, local, cfg.Server.DataDirectory(p.Name)); err != nil {
		return nil, err
	}
	if err := renameFollower(ctx, local, src, p.Name, env.Secrets, env.Report, env.Job); err != nil {
		return nil, err
	}
	cfg, err := independent(p.Name)
	if err != nil {
		return nil, err
	}
	env.Cluster = cfg
	spec, _ = cfg.ClusterByName(p.Name)
	// Without 60-vaire-follower.conf the floors and the replication settings
	// go (postmaster ones among them, as a promote's recovery targets): a
	// restart now, so its own steps find nothing waiting (Phase 5's
	// SettlePromoted does the same for a restore).
	if err := steps.RemoveFollowerConf(p.Name); err != nil {
		return nil, err
	}
	if err := backup.RestartCluster(ctx, env.shell(), env.Job, env.Report, p.Name, cfg.Server.DataDirectory(p.Name), spec.Port); err != nil {
		return nil, err
	}
	var warnings []output.Warning
	if w := dropFollowerSlot(ctx, cfg, config.ClusterSpec{Name: p.Name, Follows: src}); w != nil {
		warnings = append(warnings, *w)
	}

	rt := jobRuntime(env)
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, unfollowRuns(cfg, p.Name)...))
	logLine(env.Report, env.Job, "restarting "+p.Name+" with its own configuration")
	if err := applyNewCluster(ctx, cfg, rt, p.Name, env.Report); err != nil {
		return nil, withHint(err, finishNewCluster(p.Name))
	}
	if err := applyPoolers(ctx, cfg, rt, env.Report); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the poolers were not reloaded, so " + p.Name + " is still routed to " + src + "'s database name until vaire apply: " + err.Error()})
	}
	resume()
	if w := applyMetrics(ctx, cfg, rt, env.Report, p.Name); w != nil {
		warnings = append(warnings, *w)
	}
	if w := firstBackup(ctx, cfg, rt, env.Report, p.Name); w != nil {
		warnings = append(warnings, *w)
	}
	steps.WriteMetrics(cfg)
	writeFollowerMetrics(ctx, cfg)
	return map[string]any{"name": p.Name, "source": src, "stanza": spec.Stanza, "port": spec.Port, "warnings": warnings}, nil
}

// unfollowRuns are the applies db unfollow runs, for the step numbering:
// the database's own steps, the poolers, metrics and, with backups on, its
// stanza and timers.
func unfollowRuns(cfg config.Cluster, name string) []engine.Options {
	runs := []engine.Options{{DB: name}, {Only: steps.PoolerStepIDs()}, {Only: steps.MetricsStepIDs()}}
	if cfg.BackupConfigured() {
		runs = append(runs, engine.Options{Only: append(steps.PgBackRestStepIDs(), steps.TimersStepIDs()...)})
	}
	return runs
}

// renameFollower renames the source's database and roles in the promoted
// follower to its own (the fork's renames) with the new passwords. A
// resumed unfollow whose renames are done sets the passwords only.
func renameFollower(ctx context.Context, local pg.Local, source, name string, passwords map[string]string, report engine.Reporter, job string) error {
	has, err := databaseIn(ctx, local, source)
	if err != nil {
		return err
	}
	if has {
		logLine(report, job, "renaming "+source+" and its roles to "+name)
		return renameFork(ctx, local, source, name, passwords)
	}
	logLine(report, job, name+" is renamed already; setting its new passwords")
	return setRolePasswords(ctx, local, name, passwords)
}

// setRolePasswords sets name's login roles' passwords as SCRAM verifiers.
func setRolePasswords(ctx context.Context, local pg.Local, name string, passwords map[string]string) error {
	var b strings.Builder
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(name, role)
		if passwords[key] == "" {
			return output.NewError(output.CodeInputRequired, "the password for "+name+"_"+role+" was not supplied", output.ExitInput, "").
				WithDetails(map[string]any{"secret": key})
		}
		verifier, err := pg.SCRAMVerifier(passwords[key])
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "ALTER ROLE %s PASSWORD '%s';\n", pg.Quote(name+"_"+role), verifier)
	}
	return local.Exec(ctx, "postgres", b.String(), nil)
}

// independent clears name's follows in cluster.toml and gives it a stanza
// of its own: the one change to follows vaire makes (D1).
func independent(name string) (config.Cluster, error) {
	store := state.FileStore{}
	cfg, _, err := store.Load()
	if err != nil {
		return cfg, err
	}
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name != name {
			continue
		}
		stanza, err := config.NewStanza(name)
		if err != nil {
			return cfg, err
		}
		cfg.Clusters[i].Follows, cfg.Clusters[i].Stanza = "", stanza
	}
	if err := cfg.Validate(); err != nil {
		return cfg, config.ValidationError(err)
	}
	raw, err := cfg.Encode()
	if err != nil {
		return cfg, err
	}
	return cfg, store.Save(raw)
}

// --- the source's followers ---

// applyFollowerConfigs applies the configuration of db's followers, whose
// hot-standby floors come from db's entry: before db's own steps when its
// size changes (D6). A follower restarts only when confirm allows it; else
// the warning restart_pending names its token.
func applyFollowerConfigs(ctx context.Context, cfg config.Cluster, rt engine.Runtime, db string, confirm engine.Confirmation, report engine.Reporter) []output.Warning {
	followers := cfg.FollowerNames(db)
	if len(followers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(followers))
	for _, f := range followers {
		ids = append(ids, steps.ConfigStepID(f))
	}
	_, warnings, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: ids, Confirm: confirm, Restarter: steps.Restarter(cfg), Report: report})
	if err != nil {
		return []output.Warning{{Code: "other_config_failed",
			Message: "the configuration of " + strings.Join(followers, ", ") + " was not updated for " + db + "'s new size: " + err.Error() + "; run vaire apply"}}
	}
	return warnings
}

// followersDiverged is the warning restore promote and undo give a source
// with followers (D12).
func followersDiverged(cfg config.Cluster, db string) []output.Warning {
	followers := cfg.FollowerNames(db)
	if len(followers) == 0 {
		return nil
	}
	sort.Strings(followers)
	f := followers[0]
	return []output.Warning{{Code: "followers_diverged", Message: db + " is on a new timeline, so its followers (" + strings.Join(followers, ", ") +
		") stopped following it and serve the old data read-only. Drop and follow again: vaire db drop " + f + " --confirm " + f + ", then vaire db follow " + db + " --name " + f + "."}}
}

// --- db recover of a follower ---

// recoverFollower is db recover of a follower awaiting recovery (D14): once
// its source is back, the db-follow job makes it again for its existing
// entry. It needs no bucket of its own; it always follows the source's
// present, so --time and --no-promote are refused.
func recoverFollower(ctx context.Context, env *Env, p recoverParams, spec config.ClusterSpec, recs backup.Records) (Result, error) {
	if p.Time != "" || p.NoPromote {
		return Result{}, output.NewError(output.CodeUsage, spec.Name+" is a follower of "+spec.Follows+": it comes back following "+spec.Follows+"'s present, so --time and --no-promote don't apply", output.ExitUsage,
			"Run vaire db recover "+spec.Name+" without them.")
	}
	src, err := clusterOf(env.Cluster, spec.Follows)
	if err != nil {
		return Result{}, err
	}
	if recs.IsAwaiting(src.Name) {
		return Result{}, output.NewError(output.CodePrecondition, spec.Name+" follows "+src.Name+", which awaits recovery too", output.ExitPrecondition,
			"Recover the source first: vaire db recover "+src.Name+" (vaire db recover --all does sources first).").
			WithDetails(map[string]any{"reason": "source_awaiting_recovery", "database": spec.Name, "source": src.Name})
	}
	if err := checkFollowSource(src, "db recover"); err != nil {
		return Result{}, err
	}
	if steps.ClusterExists(ctx, spec.Name) {
		return Result{}, output.NewError(output.CodePrecondition, "cluster 18/"+spec.Name+" already exists", output.ExitPrecondition,
			"Drop what is left of it first (pg_dropcluster 18 "+spec.Name+" --stop on the node), then retry.").
			WithDetails(map[string]any{"reason": "cluster_exists", "cluster": spec.Name})
	}
	fp := dbFollowParams{Source: src.Name, Name: spec.Name, From: followFrom(ctx, env.Cluster, src), Recover: true}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": spec.Name, "follows": src.Name, "from": fp.From, "dry_run": true}}, nil
	}
	return runJobRPC(ctx, env, "db-follow", fp, stepSecrets(env.Req.Secrets))
}
