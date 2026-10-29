package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	"github.com/mbvlabs/vaire/internal/secrets"
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

// followerServer are the server effects of a follow and of an unfollow: the
// work that needs psql, pgBackRest and systemd. They are function fields so a
// test stops a job at one boundary and reads what it left durable; this
// variable holds the node's own. The steps themselves run through
// engine.Apply, which needs no lock of its own.
type followerServer struct {
	// build makes the follower's cluster: the slot on the source, the copy,
	// its recovery settings and its adoption as 18/<name>. recover is a db
	// recover of a follower: the build may continue an owned interrupted copy.
	build func(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec, from string, recover bool) error
	// start writes the follower's managed configuration, starts it and
	// finishes the follow (its own steps, the poolers, metrics, the other
	// databases' changed configuration, a short wait for streaming).
	// recovering is a database awaiting recovery the steps must see.
	start func(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec, recovering string) ([]output.Warning, followerReading, error)
	// promote ends recovery, rename renames the source's database and roles to
	// the follower's with the operator's new passwords, removeConf removes
	// 60-vaire-follower.conf, restart starts the cluster with its own
	// configuration, dropSlot drops the follower's slot on its source, and
	// finish makes the rest of it independent (its own steps, the poolers,
	// metrics, its stanza, first full and timers). resume lets the poolers'
	// clients in again after the renames.
	promote    func(ctx context.Context, local pg.Local, data string) ([]string, error)
	rename     func(ctx context.Context, local sqlRunner, source, name string, passwords map[string]string, report engine.Reporter, job string) error
	removeConf func(cluster string) error
	restart    func(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec) error
	dropSlot   func(ctx context.Context, cfg config.Cluster, source, name string) *output.Warning
	finish     func(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec, source string, resume func()) ([]output.Warning, error)
}

// followerCopy is the physical work of materialising one follower's copy.
// backup.Materialised implements it; a test replaces the constructors below to
// leave the exact filesystem state of an interrupted build while the production
// ownership, admission and resumption flow still runs.
type followerCopy interface {
	DataDir() string
	Restore(ctx context.Context, d backup.Driver, opts backup.RestoreOptions) error
	StandbyBaseBackup(ctx context.Context, slot string) error
	Adopt(ctx context.Context, port int) error
	Drop() error
}

// newFollowerCopy creates a fresh copy; resumeFollowerCopy addresses the
// deterministic paths an interrupted build already registered, without
// reserving a port or refusing their existence.
var (
	newFollowerCopy = func(dataRoot, name, source string, sourcePort int, sh engine.Shell, stepID string, report engine.Reporter) (followerCopy, error) {
		return backup.NewMaterialised(dataRoot, name, source, sourcePort, sh, stepID, report)
	}
	resumeFollowerCopy = func(dataRoot, name, source string, sourcePort int, sh engine.Shell, stepID string, report engine.Reporter) followerCopy {
		return backup.ResumeMaterialised(dataRoot, name, source, sourcePort, sh, stepID, report)
	}
)

// followerSlotOps are the source-side slot effects of a build: a variable so a
// lifecycle test can stop the build without a live source cluster. inspect
// leaves the slot untouched; create makes a missing one; reset drops an
// inactive leftover and creates it again; drop removes it.
var followerSlotOps = struct {
	inspect func(ctx context.Context, src config.ClusterSpec, slot string) (followerSlotState, error)
	create  func(ctx context.Context, src config.ClusterSpec, slot string) error
	reset   func(ctx context.Context, src config.ClusterSpec, slot string) error
	drop    func(ctx context.Context, src config.ClusterSpec, slot string) error
}{inspect: inspectFollowerSlot, create: createFollowerSlot, reset: resetFollowerSlot, drop: dropSlot}

// markFollowerAdopted is the durable marker boundary of a recovery; a variable
// so a test can stop after the adoption, before the marker, and retry.
var markFollowerAdopted = markAdopted

var followerServerOps = followerServer{
	build:      materialiseFollower,
	start:      startFollower,
	promote:    backup.Promote,
	rename:     renameFollower,
	removeConf: steps.RemoveFollowerConf,
	restart:    restartFollower,
	dropSlot:   dropUnfollowedSlot,
	finish:     finishUnfollow,
}

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
	"backup.now":      {action: "a backup", field: "database"},
	"backup.verify":   {action: "a verify", field: "database"},
	"backup.list":     {action: "listing backups", field: "database"},
	"restore.start":   {action: "a restore", field: "database"},
	"restore.promote": {action: "a restore promote", field: "database"},
	"restore.undo":    {action: "a restore undo", field: "database"},
	"restore.drill":   {action: "a restore drill", field: "database"},
	"dump.create":     {action: "a dump", field: "database"},
	"db.fork":         {action: "a fork", field: "source"},
	"maint.run":       {action: "maintenance", field: "db"},
	"db.credentials":  {action: "rotating its credentials", field: "name", applies: func(p map[string]any) bool { r, _ := p["rotate"].(bool); return r }},
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

// RefuseFollowerMethod is refuseFollower for the CLI, which refuses a method
// on a follower before it checks protection or asks for a confirmation,
// with the same is_follower error the node gives.
func RefuseFollowerMethod(cfg config.Cluster, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return refuseFollower(rpc.Request{Method: method, Params: raw}, cfg)
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

// refuseFollowerFloorDrop refuses a resize that would lower a follower's
// hot-standby floors (Phase 6, Q1a): until the follower restarts above the
// source's old values its replay can pause, and a retry cannot order that
// restart around the source.
func refuseFollowerFloorDrop(cfg config.Cluster, db string, was, now config.ClusterSpec, host hostinfo.Info) error {
	drops, err := steps.FollowerFloorDrops(cfg, db, was, now, host)
	if err != nil {
		return err
	}
	if len(drops) == 0 {
		return nil
	}
	followers := cfg.FollowerNames(db)
	falls := make([]string, 0, len(drops))
	for _, d := range drops {
		falls = append(falls, fmt.Sprintf("%s %s %d -> %d", d.Follower, d.Setting, d.From, d.To))
	}
	f := followers[0]
	return output.NewError(output.CodePrecondition,
		db+" has followers ("+strings.Join(followers, ", ")+") whose hot-standby floors would fall: "+strings.Join(falls, ", "), output.ExitPrecondition,
		"Unfollow or drop "+f+" first (vaire db unfollow "+f+" --confirm "+f+"/unfollow, or vaire db drop "+f+" --confirm "+f+"), resize "+db+", then run vaire db follow "+db+" again.").
		WithDetails(map[string]any{"reason": "has_followers", "database": db, "followers": followers, "settings": drops})
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

// followerSlotState is what a source slot offers a completed copy: whether it
// exists and whether it can still retain the WAL that copy needs.
type followerSlotState struct {
	Exists bool
	Usable bool
}

// inspectFollowerSlot reads slot on src without touching it. A missing slot, a
// lost or unreserved one, one without a restart position, or one whose status
// vaire does not know cannot retain the copy's WAL.
func inspectFollowerSlot(ctx context.Context, src config.ClusterSpec, slot string) (followerSlotState, error) {
	rows, err := steps.ClusterPG(src).Query(ctx, "postgres",
		"SELECT wal_status, (restart_lsn IS NOT NULL)::text AS has_restart FROM pg_replication_slots WHERE slot_name = "+quoteLiteral(slot), nil)
	if err != nil {
		return followerSlotState{}, fmt.Errorf("inspect the replication slot %s on %s: %w", slot, src.Name, err)
	}
	return followerSlotStateOf(rows), nil
}

// followerSlotStateOf is inspectFollowerSlot's reading of its row. Only a
// reserved or extended slot with a restart position still retains WAL; an
// unreserved one no longer keeps it, and another status fails closed.
func followerSlotStateOf(rows []map[string]string) followerSlotState {
	if len(rows) == 0 {
		return followerSlotState{}
	}
	status := rows[0]["wal_status"]
	return followerSlotState{
		Exists: true,
		Usable: (status == "reserved" || status == "extended") && rows[0]["has_restart"] == "true",
	}
}

// createFollowerSlot makes follower's physical slot on its source with its
// WAL reserved at once (D4) when it does not exist yet. An existing slot is
// left as it is: resetFollowerSlot is how a fresh copy deals with a leftover.
func createFollowerSlot(ctx context.Context, src config.ClusterSpec, slot string) error {
	s := quoteLiteral(slot)
	sql := "SELECT pg_create_physical_replication_slot(" + s + ", true) WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = " + s + ");"
	if err := steps.ClusterPG(src).Exec(ctx, "postgres", sql, nil); err != nil {
		return fmt.Errorf("create the replication slot %s on %s: %w", slot, src.Name, err)
	}
	return nil
}

// resetFollowerSlot drops an inactive slot of the same name, left by a failed
// attempt, and creates it again with its WAL reserved at the source's current
// position. A completed copy's slot is never reset: that copy needs the WAL its
// older slot position retains.
func resetFollowerSlot(ctx context.Context, src config.ClusterSpec, slot string) error {
	s := quoteLiteral(slot)
	sql := "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = " + s + " AND NOT active;\n"
	if err := steps.ClusterPG(src).Exec(ctx, "postgres", sql, nil); err != nil {
		return fmt.Errorf("reset the replication slot %s on %s: %w", slot, src.Name, err)
	}
	return createFollowerSlot(ctx, src, slot)
}

// prepareFollowerSlot makes the slot of a fresh build: a missing one is
// created, and an inactive leftover of the same name is reset.
func prepareFollowerSlot(ctx context.Context, src config.ClusterSpec, slot string) error {
	state, err := followerSlotOps.inspect(ctx, src, slot)
	if err != nil {
		return err
	}
	if state.Exists {
		return followerSlotOps.reset(ctx, src, slot)
	}
	return followerSlotOps.create(ctx, src, slot)
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
// db recover, leaves it awaiting recovery).
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
	// db recover keeps the database's durable awaiting entry until the
	// follower runs: the steps see it through Options.Recovering instead
	// (Phase 5's engine view hides a database awaiting recovery), so a job
	// that dies before the follower is up leaves recovery retryable (D14).
	recovering := ""
	if p.Recover {
		recovering = p.Name
	}
	build := true
	if p.Recover {
		// A copy an earlier attempt adopted is 18/<name> already: start it
		// with the configuration and steps it is missing instead of copying
		// over it (D14's resumption). An adopted record without the expected
		// cluster is only rebuilt when nothing is there; mismatched paths are
		// refused, never adopted or dropped.
		if build, err = followerRecoveryNeedsBuild(cfg, spec); err != nil {
			return nil, err
		}
	}
	if build {
		if err := followerServerOps.build(ctx, env, cfg, src, spec, p.From, p.Recover); err != nil {
			if p.Recover {
				return nil, err
			}
			if ferr := forgetDatabase(p.Name); ferr != nil {
				err = errors.Join(err, ferr)
			}
			return nil, err
		}
		if p.Recover {
			// The copy is 18/<name> on its own port: a recovery that stops
			// from here starts it instead of copying again. The ownership
			// record goes only after this durable marker.
			if err := markFollowerAdopted(p.Name); err != nil {
				return nil, err
			}
		}
	}
	if p.Recover {
		if err := clearFollowOwnership(p.Name); err != nil {
			logLine(env.Report, env.Job, "the interrupted follow record was left: "+err.Error())
		}
	}
	warnings, st, err := followerServerOps.start(ctx, env, cfg, src, spec, recovering)
	if err != nil {
		return nil, err
	}
	if p.Recover {
		// The follower runs with its own configuration: recovery is done and
		// it no longer awaits it.
		if _, err := clearAwaiting(p.Name); err != nil {
			return nil, err
		}
	}
	return map[string]any{"source": src.Name, "follows": src.Name, "name": p.Name, "port": spec.Port, "from": p.From,
		"slot": config.FollowerSlot(p.Name), "follower": st, "warnings": warnings}, nil
}

// followerRecoveryNeedsBuild reports whether a db recover of spec still has to
// build. An adopted copy whose deterministic registration and data directory
// match is started as it is. An adopted record with no local artefact is built
// again; adopted or foreign artefacts that do not match are refused.
func followerRecoveryNeedsBuild(cfg config.Cluster, spec config.ClusterSpec) (bool, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return false, err
	}
	art, err := followerArtefactsOf(cfg, spec.Name)
	if err != nil {
		return false, err
	}
	if !recs.AwaitingRecovery[spec.Name].Adopted {
		return true, nil
	}
	if art.Match && art.Data {
		return false, nil
	}
	if art.ConfDir || art.Conf || art.Data {
		return false, clusterExistsRefusal(spec.Name)
	}
	return true, nil
}

// followerResumePreview is what db recover's dry run can say about a retry:
// whether it continues an existing copy, and why it copies again when it does
// not. A completed copy is only continued on a source slot that still retains
// its WAL; when the preview cannot read the slot, it does not promise reuse and
// marks the answer conditional, and the job inspects the slot again either way.
type followerResumePreview struct {
	Resume       bool
	RecopyReason string
	Conditional  bool
}

// followerRecoveryResumes reports whether a retry continues an existing copy
// instead of copying again: either a durable adopted marker with both expected
// artefacts present and pointing at each other, or an owned copy that finished
// (phase copied) with the same proof and a source slot that still retains its
// WAL. The slot is read, never changed, as the job reads it before it resumes.
func followerRecoveryResumes(ctx context.Context, cfg config.Cluster, src, spec config.ClusterSpec) (followerResumePreview, error) {
	art, err := followerArtefactsOf(cfg, spec.Name)
	if err != nil {
		return followerResumePreview{}, err
	}
	if !art.Match || !art.Data {
		return followerResumePreview{}, nil
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return followerResumePreview{}, err
	}
	awaiting := recs.AwaitingRecovery[spec.Name]
	if awaiting.Adopted {
		// An adopted copy is started as it is: this retry copies and inspects
		// no slot.
		return followerResumePreview{Resume: true}, nil
	}
	own, owned, err := readFollowOwnership(spec.Name, spec.Follows, awaiting)
	if err != nil {
		return followerResumePreview{}, err
	}
	if !owned || own.Phase != followPhaseCopied {
		return followerResumePreview{}, nil
	}
	slot := config.FollowerSlot(spec.Name)
	state, err := followerSlotOps.inspect(ctx, src, slot)
	if err != nil {
		return followerResumePreview{RecopyReason: "the replication slot " + slot + " on " + src.Name + " could not be read", Conditional: true}, nil
	}
	if state.Usable {
		return followerResumePreview{Resume: true}, nil
	}
	return followerResumePreview{RecopyReason: "the replication slot " + slot + " on " + src.Name + " cannot retain the WAL the completed copy needs"}, nil
}

// markAdopted records that db's follower copy is its own cluster 18/<name>
// already, so a db recover that stops after the adoption starts it instead of
// copying it again. A plain db follow has no awaiting entry to mark.
func markAdopted(db string) error {
	recs, err := backup.LoadRecords()
	if err != nil || !recs.IsAwaiting(db) {
		return err
	}
	return backup.UpdateRecords(func(s *backup.Records) error {
		aw, ok := s.AwaitingRecovery[db]
		if ok {
			aw.Adopted = true
			s.AwaitingRecovery[db] = aw
		}
		return nil
	})
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

// dropOwnedFollowerCopy removes the operation-owned data and configuration of
// an interrupted copy; a variable so a lifecycle test can stop before a real
// pg_dropcluster.
var dropOwnedFollowerCopy = backup.DropTemporaryCluster

// clearFollowOwnershipWhenClean removes the ownership record only when no
// local artefact of the operation remains. A configuration directory or data
// directory a failed cleanup left keeps the record, so the next retry can
// finish removing it instead of stranding it as a foreign cluster.
func clearFollowOwnershipWhenClean(cfg config.Cluster, name string) error {
	art, err := followerArtefactsOf(cfg, name)
	if err != nil {
		return err
	}
	if art.ConfDir || art.Conf || art.Data {
		return nil
	}
	return clearFollowOwnership(name)
}

// dropFreshFollowerSlot removes the source slot a fresh build reserved when the
// build cannot go on, with its own timeout so a cancelled job still cleans it
// up. A completed copy's slot is never dropped this way.
func dropFreshFollowerSlot(ctx context.Context, src config.ClusterSpec, slot string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return followerSlotOps.drop(cctx, src, slot)
}

// materialiseFollower builds the follower's data in the temporary cluster
// <name> from the source's stanza or with pg_basebackup through its slot,
// drops pgBackRest's recovery settings and adopts it as 18/<name> on spec's
// port. A plain follow drops a failed build and its slot. A db recover records
// ownership before its first local mutation: a partial copy is removed and
// rebuilt, while a completed copy is adopted again instead of recopied. A
// completed copy keeps the source slot it left behind; only a missing or lost
// slot makes the operation drop its own copy and copy again. An adoption error
// keeps the owned copy for the retry. Foreign paths are refused.
func materialiseFollower(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec, from string, recover bool) error {
	slot := config.FollowerSlot(spec.Name)
	resume := false
	var awaiting backup.Awaiting
	if recover {
		recs, err := backup.LoadRecords()
		if err != nil {
			return err
		}
		awaiting = recs.AwaitingRecovery[spec.Name]
		art, err := followerArtefactsOf(cfg, spec.Name)
		if err != nil {
			return err
		}
		own, owned, err := readFollowOwnership(spec.Name, spec.Follows, awaiting)
		if err != nil {
			return err
		}
		if owned && own.Phase == followPhaseCopied && art.Match && art.Data {
			// The copy is complete. Its slot's older position protects the WAL
			// it still has to stream, so the slot is only inspected here.
			state, err := followerSlotOps.inspect(ctx, src, slot)
			if err != nil {
				return err
			}
			if state.Usable {
				resume = true
			} else {
				// Reset would lose WAL beneath data that needs it. The owned
				// copy is abandoned and copied again with a fresh slot instead.
				logLine(env.Report, env.Job, "the replication slot "+slot+" on "+src.Name+" is missing or lost, so the owned copy of "+spec.Name+" is made again")
				if err := dropOwnedFollowerCopy(env.shell(), env.Job, env.Report, spec.Name, cfg.Server.DataDirectory(spec.Name)); err != nil {
					return fmt.Errorf("remove the completed copy of %s whose replication slot %s is missing or lost: %w", spec.Name, slot, err)
				}
			}
		} else if art.ConfDir || art.Conf || art.Data {
			if !owned || art.Conf && !art.Match {
				return clusterExistsRefusal(spec.Name)
			}
			if err := dropOwnedFollowerCopy(env.shell(), env.Job, env.Report, spec.Name, cfg.Server.DataDirectory(spec.Name)); err != nil {
				return fmt.Errorf("remove the interrupted copy of %s: %w", spec.Name, err)
			}
		}
		if !resume {
			if err := writeFollowOwnership(spec.Name, src.Name, awaiting, followPhaseCopying); err != nil {
				return err
			}
		}
	}
	if resume {
		return resumeFollowerAdoption(ctx, env, cfg, src, spec)
	}
	logLine(env.Report, env.Job, "creating the replication slot "+slot+" on "+src.Name)
	if err := prepareFollowerSlot(ctx, src, slot); err != nil {
		return err
	}
	m, err := newFollowerCopy(cfg.Server.DataRoot, spec.Name, src.Name, src.Port, env.shell(), env.Job, env.Report)
	if err != nil {
		err = errors.Join(err, dropFreshFollowerSlot(ctx, src, slot))
		if recover {
			// The constructor made nothing; a record from this attempt owns
			// nothing either, unless a failed cleanup left an artefact.
			err = errors.Join(err, clearFollowOwnershipWhenClean(cfg, spec.Name))
		}
		return err
	}
	copied := false
	if from == followFromStanza {
		recs, _ := backup.LoadRecords()
		err = m.Restore(ctx, driver(src, env.Job, env.Report), backup.RestoreOptions{
			Kind: backup.TargetLatest, Pause: true, Timeline: recs.TargetTimeline(src.Name, src.Stanza),
		})
	} else {
		err = m.StandbyBaseBackup(ctx, slot)
	}
	if err == nil {
		if recover {
			// The copy is complete and registered: from here a retry adopts
			// it again instead of copying over it.
			err = writeFollowOwnership(spec.Name, src.Name, awaiting, followPhaseCopied)
		}
		copied = err == nil
	}
	if err == nil {
		err = backup.DropRecoverySettings(m.DataDir())
	}
	if err == nil {
		err = m.Adopt(ctx, spec.Port)
	}
	if err != nil {
		if recover && copied {
			// The owned copy is complete: keep it and its slot for the retry,
			// which resumes the adoption.
			return err
		}
		dropErr := m.Drop()
		err = errors.Join(err, dropErr)
		if derr := dropFreshFollowerSlot(ctx, src, slot); derr != nil {
			err = errors.Join(err, derr)
		}
		if recover {
			err = errors.Join(err, clearFollowOwnershipWhenClean(cfg, spec.Name))
		}
	}
	return err
}

// resumeFollowerAdoption finishes an owned copy whose registration a previous
// attempt completed. Adopt stops the temporary cluster and rewrites the same
// files, so it is safe on a partial registration or an already adopted one.
func resumeFollowerAdoption(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec) error {
	m := resumeFollowerCopy(cfg.Server.DataRoot, spec.Name, src.Name, src.Port, env.shell(), env.Job, env.Report)
	if err := backup.DropRecoverySettings(m.DataDir()); err != nil {
		return err
	}
	return m.Adopt(ctx, spec.Port)
}

// startFollower writes the follower's whole managed configuration, including
// 60-vaire-follower.conf, before its first start under systemd (D2): a standby
// that started with defaults below the source's floors would pause replay at
// once, and one without primary_conninfo would never stream. Then it starts the
// follower and finishes the follow. recovering is a database awaiting recovery
// the steps must see (db recover), empty for a plain follow.
func startFollower(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec, recovering string) ([]output.Warning, followerReading, error) {
	rt := jobRuntime(env)
	if err := steps.WriteFollowerConf(cfg, spec, rt.Host); err != nil {
		return nil, followerReading{}, err
	}
	others := otherConfigs(ctx, steps.Catalog(), cfg, rt, spec.Name, "")
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, followRuns(spec.Name, others)...))
	logLine(env.Report, env.Job, "starting "+spec.Name+" as a standby of "+src.Name)
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Recovering: recovering, Restarter: steps.Restarter(cfg), Report: env.Report})
	if err == nil {
		err = applyNewCluster(ctx, cfg, rt, spec.Name, recovering, env.Report)
	}
	if err != nil {
		return nil, followerReading{}, withHint(err, finishNewCluster(spec.Name))
	}
	var warnings []output.Warning
	if err := applyPoolers(ctx, cfg, rt, recovering, env.Report); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the poolers were not reloaded, so " + spec.Name + " is not reachable through them until vaire apply: " + err.Error()})
	}
	if w := applyMetrics(ctx, cfg, rt, env.Report, spec.Name, recovering); w != nil {
		warnings = append(warnings, *w)
	}
	warnings = append(warnings, applyOtherConfigs(ctx, steps.Catalog(), cfg, rt, others,
		engine.Options{Recovering: recovering, Restarter: steps.Restarter(cfg), Report: env.Report})...)
	logLine(env.Report, env.Job, "waiting for "+spec.Name+" to stream from "+src.Name)
	st := waitStreaming(ctx, spec, src, streamWait)
	if !st.Streaming {
		warnings = append(warnings, output.Warning{Code: "follower_not_streaming",
			Message: spec.Name + " is not streaming from " + src.Name + " yet: vaire doctor --db " + spec.Name + " and vaire logs postgres --db " + spec.Name + " show why"})
	}
	writeFollowerMetrics(ctx, cfg)
	return warnings, st, nil
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

// --- db unfollow ---

// handleDBUnfollow checks the follower (or the unfinished unfollow it
// resumes) and the new passwords, then runs the detachable job db-unfollow
// (D10).
func handleDBUnfollow(ctx context.Context, env *Env) (Result, error) {
	var p dbUnfollowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	recs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	spec, unfinished, resuming, err := unfollowState(cfg, recs, p.Name)
	if err != nil {
		return Result{}, err
	}
	src := spec.Follows
	if resuming {
		src = unfinished.Source
	}
	if env.Req.Confirm == "" && !env.Req.DryRun {
		return Result{}, confirmRequired("unfollowing "+p.Name+" promotes it for good: it stops following "+src+" and its credentials change", p.Name+"/unfollow")
	}
	// A resumed unfollow starts the cluster itself: its earlier attempt may
	// have stopped at the restart, so a cluster that is down is no reason to
	// refuse it (D10's follower_not_running refuses a follower that was never
	// promoted).
	if !resuming && !clusterRunning(spec) {
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
		data := map[string]any{"name": p.Name, "source": src, "slot": config.FollowerSlot(p.Name),
			"renames": unfollowRenames(src, p.Name), "backups": cfg.BackupConfigured(), "dry_run": true}
		if resuming {
			// A resumed unfollow keeps the stanza its record fixed and finishes
			// at whichever step the earlier attempt stopped.
			data["resume"], data["stanza"] = true, unfinished.Stanza
		}
		return Result{Data: data}, nil
	}
	return runJobRPC(ctx, env, "db-unfollow", p, jobSecrets)
}

// unfollowState resolves db unfollow's target: its entry in cluster.toml and,
// when an earlier attempt stopped part way, the record it left. A database
// whose follows cluster.toml no longer has is admitted by that record alone: it
// is the only place its source and stanza are left (D10's resumption).
func unfollowState(cfg config.Cluster, recs backup.Records, name string) (config.ClusterSpec, backup.Unfollow, bool, error) {
	spec, ok := cfg.ClusterByName(name)
	if !ok {
		return config.ClusterSpec{}, backup.Unfollow{}, false, databaseNotFound(name)
	}
	unfinished, resuming := recs.Unfollowing[name]
	if !resuming && !spec.IsFollower() {
		return spec, backup.Unfollow{}, false, output.NewError(output.CodePrecondition, name+" follows nothing, so there is nothing to unfollow", output.ExitPrecondition,
			"vaire db list shows the followers (state following).").
			WithDetails(map[string]any{"reason": "not_a_follower", "database": name})
	}
	return spec, unfinished, resuming, nil
}

// unfollowRenames are the renames unfollow makes, for the dry run.
func unfollowRenames(source, name string) map[string]string {
	out := map[string]string{source: name}
	for _, role := range append([]string{"owner"}, steps.DatabaseRoles...) {
		out[source+"_"+role] = name + "_" + role
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
//
// The job holds the node lock exclusive and the follower's cluster lock
// exclusive (RunJob), so it reloads cluster.toml first: a transition another
// command made after this job's process started is kept, and the unfollow's
// own transition can't lose one made beside it. Its own record in records.json
// is written before the first irreversible step and cleared only when the
// database is independent: a job that stops anywhere between resumes with the
// same source and stanza.
func jobUnfollow(ctx context.Context, env *jobEnv) (any, error) {
	var p dbUnfollowParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	cfg, _, err := followerState().Load()
	if err != nil {
		return nil, err
	}
	env.Cluster = cfg
	recs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	spec, unfinished, resuming, err := unfollowState(cfg, recs, p.Name)
	if err != nil {
		return nil, err
	}
	src := spec.Follows
	if resuming {
		src = unfinished.Source
	}
	local := steps.ClusterPG(spec)
	resume := sync.OnceFunc(steps.KillPoolerDatabase(ctx, p.Name))
	defer resume()

	unfinished, err = beginUnfollow(p.Name, src)
	if err != nil {
		return nil, err
	}
	if resuming && !clusterRunning(spec) {
		// The earlier attempt stopped at its restart, which leaves the
		// cluster down: it is started with its own configuration (that
		// attempt removed 60-vaire-follower.conf already) so the promotion
		// and the renames below have a cluster to run in.
		if err := followerServerOps.restart(ctx, env, cfg, spec); err != nil {
			return nil, mayHaveChangedPasswords(err)
		}
	}
	logLine(env.Report, env.Job, "promoting "+p.Name)
	if _, err := followerServerOps.promote(ctx, local, cfg.Server.DataDirectory(p.Name)); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	if err := followerServerOps.rename(ctx, local, src, p.Name, env.Secrets, env.Report, env.Job); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	cfg, err = independent(p.Name, unfinished.Stanza)
	if err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	env.Cluster = cfg
	spec, _ = cfg.ClusterByName(p.Name)
	// Without 60-vaire-follower.conf the floors and the replication settings
	// go (postmaster ones among them, as a promote's recovery targets): a
	// restart now, so its own steps find nothing waiting (Phase 5's
	// SettlePromoted does the same for a restore).
	if err := followerServerOps.removeConf(p.Name); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	if err := followerServerOps.restart(ctx, env, cfg, spec); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	var warnings []output.Warning
	if w := followerServerOps.dropSlot(ctx, cfg, src, p.Name); w != nil {
		warnings = append(warnings, *w)
	}
	more, err := followerServerOps.finish(ctx, env, cfg, spec, src, resume)
	warnings = append(warnings, more...)
	if err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	if err := endUnfollow(p.Name); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	return map[string]any{"name": p.Name, "source": src, "stanza": spec.Stanza, "port": spec.Port, "warnings": warnings}, nil
}

// finishUnfollow makes the promoted follower independent: its own steps (the
// floors and the replication settings are gone), the poolers with the alias as
// a plain entry and their clients let in again, metrics, and with backups on
// its stanza, first full and timers.
func finishUnfollow(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec, source string, resume func()) ([]output.Warning, error) {
	rt := jobRuntime(env)
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, unfollowRuns(cfg, spec.Name)...))
	logLine(env.Report, env.Job, "restarting "+spec.Name+" with its own configuration")
	if err := applyNewCluster(ctx, cfg, rt, spec.Name, "", env.Report); err != nil {
		return nil, withHint(err, finishNewCluster(spec.Name))
	}
	var warnings []output.Warning
	if err := applyPoolers(ctx, cfg, rt, "", env.Report); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the poolers were not reloaded, so " + spec.Name + " is still routed to " + source + "'s database name until vaire apply: " + err.Error()})
	}
	resume()
	if w := applyMetrics(ctx, cfg, rt, env.Report, spec.Name, ""); w != nil {
		warnings = append(warnings, *w)
	}
	if w := firstBackup(ctx, cfg, rt, env.Report, spec.Name); w != nil {
		warnings = append(warnings, *w)
	}
	steps.WriteMetrics(cfg)
	writeFollowerMetrics(ctx, cfg)
	return warnings, nil
}

// restartFollower starts the unfollowed cluster with its own configuration.
func restartFollower(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec) error {
	return backup.RestartCluster(ctx, env.shell(), env.Job, env.Report, spec.Name, cfg.Server.DataDirectory(spec.Name), spec.Port)
}

// dropUnfollowedSlot drops name's slot on its source, which the record kept
// after cluster.toml stopped saying name follows it.
func dropUnfollowedSlot(ctx context.Context, cfg config.Cluster, source, name string) *output.Warning {
	return dropFollowerSlot(ctx, cfg, config.ClusterSpec{Name: name, Follows: source})
}

// UnfollowPasswordsDetail marks a failed db unfollow whose promotion and
// renames may have run already: the operator's new passwords may be on the
// database, so the CLI hands them over (D10). It is a flag, never a value.
const UnfollowPasswordsDetail = "passwords_may_have_changed"

func mayHaveChangedPasswords(err error) error {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		// A plain error (pg_promote, the slot, a wrapped command) keeps its
		// text in the cause; the flag rides with it.
		cliErr = output.WrapError(output.CodeError, err, output.ExitGeneric, "")
	}
	copied := *cliErr
	details := map[string]any{}
	if m, ok := cliErr.Details.(map[string]any); ok {
		for k, v := range m {
			details[k] = v
		}
	}
	details[UnfollowPasswordsDetail] = true
	copied.Details = details
	return &copied
}

// beginUnfollow records name's unfollow before its first irreversible step,
// with the source it leaves and the stanza it keeps: a resumed unfollow uses
// those two and never generates another stanza. An existing record is returned
// unchanged, so a continuation always finishes the unfollow it started.
func beginUnfollow(name, source string) (backup.Unfollow, error) {
	var unfinished backup.Unfollow
	err := backup.UpdateRecords(func(s *backup.Records) error {
		if old, ok := s.Unfollowing[name]; ok {
			unfinished = old
			return nil
		}
		stanza, err := config.NewStanza(name)
		if err != nil {
			return err
		}
		unfinished = backup.Unfollow{Source: source, Stanza: stanza, StartedAt: stamp()}
		s.Unfollowing[name] = unfinished
		return nil
	})
	return unfinished, err
}

// endUnfollow clears name's unfollow record: the database is independent.
func endUnfollow(name string) error {
	return backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Unfollowing, name)
		return nil
	})
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

// sqlRunner is the SQL surface the renames read and write: the cluster's
// pg.Local on the node, and a fake in tests that models its databases and roles.
type sqlRunner interface {
	Exec(ctx context.Context, db, sql string, vars map[string]string) error
	Query(ctx context.Context, db, sql string, vars map[string]string) ([]map[string]string, error)
}

// renameState is what a promoted follower still has to rename: the source's
// database and the roles still under the source's name, and the ones of its
// own that exist already.
type renameState struct {
	database bool     // the source's database is still there
	taken    bool     // its own database exists already
	roles    []string // the role suffixes still under the source's name

	owned []string // the role suffixes already renamed to its own name
	both  []string // the role suffixes that exist under both names
}

// renameFollower renames the source's database and roles in the promoted
// follower to its own (the fork's renames) with the new passwords, finishing a
// resumed unfollow at whichever rename it stopped: the names the cluster still
// holds decide, so a database renamed with only some roles renamed completes
// instead of failing on the old ones.
func renameFollower(ctx context.Context, local sqlRunner, source, name string, passwords map[string]string, report engine.Reporter, job string) error {
	st, err := readRenames(ctx, local, source, name)
	if err != nil {
		return err
	}
	statements, err := renameStatements(st, source, name, passwords)
	if err != nil {
		return err
	}
	if st.database || len(st.roles) > 0 {
		logLine(report, job, "renaming "+source+" and its roles to "+name)
	} else {
		logLine(report, job, name+" is renamed already; setting its new passwords")
	}
	return local.Exec(ctx, "postgres", statements, nil)
}

// renameRoles are the roles a database owns: the owner and its login roles.
func renameRoles() []string { return append([]string{"owner"}, steps.DatabaseRoles...) }

// readRenames reads which of the source's and of the follower's names exist in
// the cluster at local.
func readRenames(ctx context.Context, local sqlRunner, source, name string) (renameState, error) {
	var st renameState
	rows, err := local.Query(ctx, "postgres",
		"SELECT datname FROM pg_database WHERE datname IN ("+quoteLiteral(source)+", "+quoteLiteral(name)+") ORDER BY datname", nil)
	if err != nil {
		return st, err
	}
	for _, row := range rows {
		switch row["datname"] {
		case source:
			st.database = true
		case name:
			st.taken = true
		}
	}
	roles := renameRoles()
	quoted := make([]string, 0, 2*len(roles))
	for _, role := range roles {
		quoted = append(quoted, quoteLiteral(source+"_"+role), quoteLiteral(name+"_"+role))
	}
	rows, err = local.Query(ctx, "postgres",
		"SELECT rolname FROM pg_roles WHERE rolname IN ("+strings.Join(quoted, ", ")+") ORDER BY rolname", nil)
	if err != nil {
		return st, err
	}
	existing := map[string]bool{}
	for _, row := range rows {
		existing[row["rolname"]] = true
	}
	for _, role := range roles {
		switch {
		case existing[source+"_"+role] && existing[name+"_"+role]:
			st.both = append(st.both, role)
		case existing[source+"_"+role]:
			st.roles = append(st.roles, role)
		case existing[name+"_"+role]:
			st.owned = append(st.owned, role)
		}
	}
	return st, nil
}

// renameStatements is the SQL that finishes the renames st describes: only the
// names still under the source's, then the settings and passwords every role of
// the follower needs, renamed now or by an earlier attempt. Ownership, grants,
// memberships, default privileges and per-role settings follow the role OIDs;
// only <name>_migrate's SET role names the owner, so it is rewritten.
func renameStatements(st renameState, source, name string, passwords map[string]string) (string, error) {
	if st.database && st.taken {
		return "", output.NewError(output.CodePrecondition, "both "+source+" and "+name+" exist in "+name+"'s cluster, so the unfollow can't tell which is its data", output.ExitPrecondition,
			"Rename the one that doesn't belong in "+name+"'s cluster (psql as postgres on the node), then retry vaire db unfollow "+name+".")
	}
	if len(st.both) > 0 {
		role := st.both[0]
		return "", output.NewError(output.CodePrecondition, "both "+source+"_"+role+" and "+name+"_"+role+" exist in "+name+"'s cluster, so the unfollow can't tell which is its role", output.ExitPrecondition,
			"Rename the one that doesn't belong in "+name+"'s cluster (psql as postgres on the node), then retry vaire db unfollow "+name+".")
	}
	for _, role := range renameRoles() {
		if !slices.Contains(st.roles, role) && !slices.Contains(st.owned, role) {
			return "", output.NewError(output.CodePrecondition, "role "+name+"_"+role+" is missing from "+name+"'s cluster", output.ExitPrecondition,
				"The unfollow can't give the database its own roles without it. Check the cluster (psql as postgres on the node), then retry vaire db unfollow "+name+".")
		}
	}
	var b strings.Builder
	if st.database {
		fmt.Fprintf(&b, "ALTER DATABASE %s RENAME TO %s;\n", pg.Quote(source), pg.Quote(name))
	}
	for _, role := range st.roles {
		fmt.Fprintf(&b, "ALTER ROLE %s RENAME TO %s;\n", pg.Quote(source+"_"+role), pg.Quote(name+"_"+role))
	}
	fmt.Fprintf(&b, "ALTER ROLE %s SET role = '%s';\n", pg.Quote(name+"_"+steps.RoleMigrate), name+"_owner")
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(name, role)
		password := passwords[key]
		if password == "" {
			return "", output.NewError(output.CodeInputRequired, "the password for "+name+"_"+role+" was not supplied", output.ExitInput,
				"Generate it on the operator and pass it as "+key+".").WithDetails(map[string]any{"secret": key})
		}
		secrets.Add(password)
		verifier, err := pg.SCRAMVerifier(password)
		if err != nil {
			return "", err
		}
		secrets.Add(verifier)
		fmt.Fprintf(&b, "ALTER ROLE %s PASSWORD '%s';\n", pg.Quote(name+"_"+role), verifier)
	}
	return b.String(), nil
}

// followerState is the node's desired state, cluster.toml. A variable so tests
// write their own file; the unfollow job holds the node lock exclusive, so no
// other writer of cluster.toml runs beside it.
var followerState = func() state.Store { return state.FileStore{} }

// independent clears name's follows in cluster.toml and gives it the stanza its
// unfollow fixed: the one change to follows vaire makes (D1).
func independent(name, stanza string) (config.Cluster, error) {
	store := followerState()
	cfg, _, err := store.Load()
	if err != nil {
		return cfg, err
	}
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name != name {
			continue
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

// followerConfigRun is one follower's configuration during db resize: that
// follower's own step, and only that follower's pending restarts (Phase 6,
// D6). The source's configuration and its pending restart stay out of it.
// base carries the request's confirmation, restarter, report and journal;
// Only and DB are the follower's.
func followerConfigRun(f string, base engine.Options) engine.Options {
	base.Only = []string{steps.ConfigStepID(f)}
	base.DB = f
	return base
}

// followerApplyResult is what db resize's follower runs did.
type followerApplyResult struct {
	Restarts []engine.ClusterRestart
	Settled  []string
	Warnings []output.Warning
	// Failed is the follower whose update stopped the run.
	Failed string
}

// applyFollowerStep is engine.Apply for a follower's run; a variable so tests
// record the runs and their failures.
var applyFollowerStep = engine.Apply

// applyFollowerConfigs applies the configuration of db's followers, one
// follower at a time, each run limited to that follower and its own pending
// restarts, so db itself never restarts here. A follower restarts only when
// the request's confirmation covers it; else the warning restart_pending
// names its token and says its replay may pause. The first failure stops, and
// is returned with the follower it was.
func applyFollowerConfigs(ctx context.Context, cfg config.Cluster, rt engine.Runtime, db string, base engine.Options) (followerApplyResult, error) {
	var out followerApplyResult
	for _, f := range cfg.FollowerNames(db) {
		data, warnings, err := applyFollowerStep(ctx, steps.Catalog(), cfg, rt, followerConfigRun(f, base))
		out.Warnings = append(out.Warnings, followerReplayWarnings(warnings, f)...)
		out.Restarts = append(out.Restarts, data.Restarts...)
		out.Settled = append(out.Settled, data.RestartsSettled...)
		if err != nil {
			out.Failed = f
			return out, err
		}
	}
	return out, nil
}

// followerReplayWarnings says what a deferred restart means for a follower:
// PostgreSQL 18 pauses recovery when replay meets a parameter change whose
// value is above the standby's own, and only a restart with the new floors
// lets replay continue.
func followerReplayWarnings(warnings []output.Warning, follower string) []output.Warning {
	out := make([]output.Warning, 0, len(warnings))
	for _, w := range warnings {
		if w.Code == "restart_pending" {
			w.Message += "; " + follower + " may pause replay until it restarts: PostgreSQL 18 pauses recovery when replay meets a parameter change above the standby's own value"
		}
		out = append(out, w)
	}
	return out
}

// followerPlan is one of db's followers in a resize dry run, with the plan
// of its own configuration and the restart it would take (Phase 6, D6).
type followerPlan struct {
	Follower string `json:"follower"`
	// Restart is "restart" when the new floors change a postmaster setting
	// the follower applies only at its start, or when its restart is already
	// pending; "none" otherwise.
	Restart engine.RestartNeed `json:"restart"`
	Plan    engine.PlanData    `json:"plan"`
}

// followerFloorsOnDiskDiffer reports whether plan would rewrite follower's
// 60-vaire-follower.conf with other floor values than the file on disk has:
// a retried resize whose earlier follower write failed has the same old and
// new entry, yet the follower still restarts.
func followerFloorsOnDiskDiffer(plan engine.PlanData, follower string) bool {
	for _, s := range plan.Steps {
		for _, d := range s.Diffs {
			if d.Path == steps.FollowerConfPath(follower) && steps.FloorsDiffer(d.Diff) {
				return true
			}
		}
	}
	return false
}

// followerPlans plans db's followers' configuration as db resize applies it:
// one follower at a time, each limited to that follower's own step and
// pending restarts. Only an affected follower is listed, one whose
// configuration step would run or that has a restart pending, with the
// rendered floor change and the restart it would take.
func followerPlans(ctx context.Context, cfg config.Cluster, rt engine.Runtime, db string, was, now config.ClusterSpec, base engine.Options) ([]followerPlan, error) {
	names := cfg.FollowerNames(db)
	out := make([]followerPlan, 0, len(names))
	for _, f := range names {
		data, err := engine.Plan(ctx, steps.Catalog(), cfg, rt, followerConfigRun(f, base))
		if err != nil {
			return nil, err
		}
		if data.WouldApply == 0 && len(data.RestartPending) == 0 {
			continue
		}
		spec, _ := cfg.ClusterByName(f)
		changed, err := steps.FollowerFloorsChange(cfg, spec, was, now, rt.Host)
		if err != nil {
			return nil, err
		}
		restart := engine.RestartNone
		if changed || len(data.RestartPending) > 0 || followerFloorsOnDiskDiffer(data, f) {
			restart = engine.RestartRestart
		}
		out = append(out, followerPlan{Follower: f, Restart: restart, Plan: data})
	}
	return out, nil
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
	if err := followerRecoverInTheWay(env.Cluster, spec, recs); err != nil {
		return Result{}, err
	}
	fp := dbFollowParams{Source: src.Name, Name: spec.Name, From: followFrom(ctx, env.Cluster, src), Recover: true}
	if env.Req.DryRun {
		preview, err := followerRecoveryResumes(ctx, env.Cluster, src, spec)
		if err != nil {
			return Result{}, err
		}
		data := map[string]any{"database": spec.Name, "follows": src.Name, "from": fp.From, "resume": preview.Resume, "dry_run": true}
		if preview.RecopyReason != "" {
			data["recopy_reason"] = preview.RecopyReason
		}
		if preview.Conditional {
			data["resume_conditional"] = true
		}
		return Result{Data: data}, nil
	}
	return runJobRPC(ctx, env, "db-follow", fp, stepSecrets(env.Req.Secrets))
}

// followerRecoverInTheWay admits db recover of a follower whose deterministic
// paths exist only when they are proven to be the operation's own: an adopted
// copy whose registration points at its data directory, or the matching
// node-local ownership record of an interrupted build. Foreign paths, and an
// adopted record whose paths do not match, are refused untouched.
func followerRecoverInTheWay(cfg config.Cluster, spec config.ClusterSpec, recs backup.Records) error {
	art, err := followerArtefactsOf(cfg, spec.Name)
	if err != nil {
		return err
	}
	_, owned, err := readFollowOwnership(spec.Name, spec.Follows, recs.AwaitingRecovery[spec.Name])
	if err != nil {
		return err
	}
	return followerRecoverRefusal(art, recs.AwaitingRecovery[spec.Name].Adopted, owned, spec.Name)
}

// followerRecoverRefusal is the decision followerRecoverInTheWay makes.
func followerRecoverRefusal(art followerArtefacts, adopted, owned bool, name string) error {
	if !art.ConfDir && !art.Conf && !art.Data {
		return nil
	}
	if adopted {
		if art.Match && art.Data {
			return nil
		}
		return clusterExistsRefusal(name)
	}
	if owned && (!art.Conf || art.Match) {
		return nil
	}
	return clusterExistsRefusal(name)
}

func clusterExistsRefusal(name string) error {
	return output.NewError(output.CodePrecondition, "cluster 18/"+name+" already exists", output.ExitPrecondition,
		"Drop what is left of it first (pg_dropcluster 18 "+name+" --stop on the node), then retry.").
		WithDetails(map[string]any{"reason": "cluster_exists", "cluster": name})
}
