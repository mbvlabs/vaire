package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
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
	// metrics, its stanza, first full and timers). fence cuts the poolers'
	// clients and renders their alias out before the promotion, settle applies
	// the protection policy to the database and owner names the cluster holds,
	// and resume lets the poolers' clients in again after the renames and
	// clears a pause an earlier attempt left.
	promote    func(ctx context.Context, local pg.Local, data string) ([]string, error)
	rename     func(ctx context.Context, local sqlRunner, source, name string, passwords map[string]string, report engine.Reporter, job string) error
	removeConf func(cluster string) error
	restart    func(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec) error
	dropSlot   func(ctx context.Context, cfg config.Cluster, source, name string) *output.Warning
	finish     func(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec, source string, resume func()) ([]output.Warning, error)
	fence      func(ctx context.Context, env *jobEnv, cfg config.Cluster, name string) (func(), error)
	settle     func(ctx context.Context, local sqlRunner, spec config.ClusterSpec, source, name string) error
	resume     func(ctx context.Context, name string)
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
		return backup.NewForkMaterialised(dataRoot, name, source, sourcePort, sh, stepID, report)
	}
	resumeFollowerCopy = func(dataRoot, name, source string, sourcePort int, sh engine.Shell, stepID string, report engine.Reporter) followerCopy {
		return backup.ResumeMaterialised(dataRoot, name, source, sourcePort, sh, stepID, report)
	}
	followerRestoreCopy = (*backup.Materialised).Restore
	followerStandbyCopy = (*backup.Materialised).StandbyBaseBackup
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
	fence:      fenceUnfollowPoolers,
	settle:     settleUnfollowAdminAccess,
	resume:     steps.ResumePoolerDatabase,
}

// dbFollowParams are db.follow's params, and the db-follow job's.
type dbFollowParams struct {
	Source string `json:"source"`
	Name   string `json:"name,omitempty"`
	// Size replaces the source's size (and its memory override); empty
	// keeps the source's.
	Size     string  `json:"size,omitempty"`
	CPUMode  *string `json:"cpu_mode,omitempty"`
	CPUQuota *string `json:"cpu_quota,omitempty"`
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
	// Generation is an ordinary follow's opaque operation identity from its
	// prepared cluster entry. A job or rollback whose entry no longer carries
	// it refuses rather than acting on a replacement.
	Generation string `json:"generation,omitempty"`
	// RecoverSince is the awaiting record's nonempty generation for a
	// recovery. A job whose awaiting record changed refuses.
	RecoverSince string `json:"recover_since,omitempty"`
	// PreparedNew is true only for the request that added a fresh follower
	// entry. A public retry never is, so it never rolls the entry back.
	PreparedNew bool `json:"prepared_new,omitempty"`
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

type recoveryFloorDecrease struct {
	Setting string `json:"setting"`
	Before  int    `json:"before"`
	After   int    `json:"after"`
}

func recoveryFloorDecreases(before, after map[string]int) []recoveryFloorDecrease {
	var drops []recoveryFloorDecrease
	for _, name := range steps.FloorSettings {
		if after[name] < before[name] {
			drops = append(drops, recoveryFloorDecrease{Setting: name, Before: before[name], After: after[name]})
		}
	}
	return drops
}

// guardRecoveryFloorAddition compares an exact membership proposal without
// changing the caller's old cluster slice.
func guardRecoveryFloorAddition(old config.Cluster, added config.ClusterSpec, host hostinfo.Info) error {
	next := old
	next.Clusters = append(append([]config.ClusterSpec(nil), old.Clusters...), added)
	return guardRecoveryFloorChanges(old, next, host)
}

// guardRecoveryFloorChanges compares complete desired topology snapshots.
// Only surviving relationships are compared for follower floors. Sources
// remain protected when followers already exist in the old snapshot.
func guardRecoveryFloorChanges(old, next config.Cluster, host hostinfo.Info) error {
	for _, source := range old.Clusters {
		updated, ok := next.ClusterByName(source.Name)
		if !ok {
			continue
		}
		followers := old.FollowerNames(source.Name)
		if len(followers) == 0 {
			continue
		}
		before, err := steps.OwnFloors(old, source, host)
		if err != nil {
			return err
		}
		after, err := steps.OwnFloors(next, updated, host)
		if err != nil {
			return err
		}
		if drops := recoveryFloorDecreases(before, after); len(drops) > 0 {
			return output.NewError(output.CodePrecondition, source.Name+" has followers and its recovery floors would decrease", output.ExitPrecondition,
				"Unfollow or drop the followers before decreasing the source settings.").
				WithDetails(map[string]any{"reason": "has_followers", "database": source.Name, "followers": followers, "settings": drops})
		}
	}
	for _, follower := range old.Clusters {
		updated, ok := next.ClusterByName(follower.Name)
		if !ok || !follower.IsFollower() || updated.Follows != follower.Follows {
			continue
		}
		before, err := steps.EffectiveFollowerFloors(old, follower, host)
		if err != nil {
			return err
		}
		after, err := steps.EffectiveFollowerFloors(next, updated, host)
		if err != nil {
			return err
		}
		if drops := recoveryFloorDecreases(before, after); len(drops) > 0 {
			return output.NewError(output.CodePrecondition, follower.Name+" has recovery floors that would decrease", output.ExitPrecondition,
				"Keep the existing recovery floors, or unfollow or drop the follower before changing them.").
				WithDetails(map[string]any{"reason": "follower_recovery_floor_decrease", "database": follower.Name, "source": follower.Follows, "settings": drops})
		}
	}
	return nil
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
	spec.CPUMode = ""
	if size != "" {
		spec.Size, spec.Memory = size, ""
	}
	return spec
}

func followCPU(spec *config.ClusterSpec, p dbFollowParams) error {
	if p.CPUMode != nil {
		if *p.CPUMode != "normal" && *p.CPUMode != "spare" {
			return output.NewError(output.CodeValidation, "CPU mode must be normal or spare", output.ExitConfig, "")
		}
		spec.CPUMode = *p.CPUMode
	}
	if p.CPUQuota != nil {
		spec.CPUQuota = *p.CPUQuota
		switch spec.CPUQuota {
		case "none":
			spec.CPUQuota = ""
		case "":
			return output.NewError(output.CodeValidation, "CPU quota must be a percentage or none", output.ExitConfig, "")
		}
	}
	return nil
}

// followCommand is the db follow command at another size: what the
// memory_overcommitted refusal suggests.
func followCommand(p dbFollowParams) func(size string) string {
	return func(size string) string {
		cmd := "vaire db follow " + p.Source + " --name " + followerName(p) + " --size " + size
		if p.CPUMode != nil {
			cmd += " --cpu-mode " + *p.CPUMode
		}
		if p.CPUQuota != nil {
			cmd += " --cpu-quota " + *p.CPUQuota
		}
		return cmd
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

// handleDBFollow checks the source and the name, then either resumes an
// unfinished owned follow the same request left, or, under the exclusive lock,
// adds the follower to cluster.toml as db create with an explicit size does (a
// port, no stanza, the budget refusal), and starts the detachable job
// db-follow, which makes its cluster (D2). A new follow is refused when the
// running source still demands higher hot-standby floors than the follower is
// about to be configured with.
func handleDBFollow(ctx context.Context, env *Env) (Result, error) {
	var p dbFollowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := rejectNullCPUParams(env.Req.Params); err != nil {
		return Result{}, err
	}
	if err := validateCPUParams(p.CPUMode, p.CPUQuota); err != nil {
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
	recs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	if err := refuseFollowWithUnfollow(recs, p.Name); err != nil {
		return Result{}, err
	}
	existing, inConfig := cfg.ClusterByName(p.Name)
	clusterExists := followClusterExists(ctx, p.Name)
	resume := false
	if inConfig {
		if existing.IsFollower() && existing.Follows == p.Source {
			if resume, err = followRetryable(cfg, recs, existing); err != nil {
				return Result{}, err
			}
		}
		if !resume {
			return Result{}, existingDatabase(p.Name)
		}
	} else if clusterExists {
		return Result{}, existingDatabase(p.Name)
	}
	host := runtimeFor(env, false).Host
	cpuSpec := existing
	if resume {
		// The generation is the operation's identity; a later writer that
		// removes or replaces the entry makes the job refuse rather than build
		// under it. A retry never prepares or rolls back the entry.
		if existing.FollowGeneration == "" {
			return Result{}, existingDatabase(p.Name)
		}
		p.Generation = existing.FollowGeneration
		proposed := existing
		if err := followCPU(&proposed, p); err != nil {
			return Result{}, err
		}
		if proposed.CPUMode != existing.CPUMode || proposed.CPUQuota != existing.CPUQuota {
			return Result{}, output.NewError(output.CodePrecondition, "retry CPU policy differs from the prepared follower", output.ExitPrecondition, "Retry with the saved CPU policy.")
		}
		if err := checkSpareCPU(ctx, existing.Name, existing.CPUMode); err != nil {
			return Result{}, err
		}
	} else {
		// The preflight an operator sees before the lock is advisory; the
		// authoritative check runs with the exact prepared entry under the
		// preparation lock and again in the job after its locked reload.
		previewCfg, previewSpec, err := followerPreviewEntry(cfg, src, p)
		if err != nil {
			return Result{}, err
		}
		cpuSpec = previewSpec
		if _, err := previewCfg.Resources(previewSpec); err != nil {
			return Result{}, config.ValidationError(err)
		}
		if err := checkSpareCPU(ctx, previewSpec.Name, previewSpec.CPUMode); err != nil {
			return Result{}, err
		}
		if err := guardRecoveryFloorChanges(cfg, previewCfg, host); err != nil {
			return Result{}, err
		}
		if err := refuseUnsafeFollowFloors(ctx, previewCfg, src, previewSpec, host, false); err != nil {
			return Result{}, err
		}
	}
	if _, err := followSpace(ctx, cfg, p.Source); err != nil {
		return Result{}, err
	}
	p.From = followFrom(ctx, cfg, src)
	if env.Req.DryRun {
		res, err := cfg.Resources(cpuSpec)
		if err != nil {
			return Result{}, err
		}
		data := map[string]any{"source": p.Source, "name": p.Name, "size": p.Size, "from": p.From,
			"slot": config.FollowerSlot(p.Name), "dry_run": true,
			"cpu_policy": map[string]string{"mode": res.CPUMode, "quota": res.CPUQuota}}
		if resume {
			preview, err := followerFollowResume(ctx, cfg, src, existing)
			if err != nil {
				return Result{}, err
			}
			data["resume"] = preview.Resume
			if preview.RecopyReason != "" {
				data["recopy_reason"] = preview.RecopyReason
			}
			if preview.Conditional {
				data["resume_conditional"] = true
			}
		}
		return Result{Data: data}, nil
	}
	if resume {
		env.Cluster = cfg
		id, err := followStartJob(ctx, env, "db-follow", p, stepSecrets(env.Req.Secrets))
		if err != nil {
			return Result{}, err
		}
		res, err := followJob(ctx, env, id)
		if err != nil {
			return res, err
		}
		if m, ok := res.Data.(map[string]any); ok {
			m["database"] = existing
		}
		return res, nil
	}
	followBeforePrepareLock()
	release, err := followPrepareAcquire(ctx, env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return Result{}, err
	}
	generation, err := config.NewFollowGeneration()
	if err != nil {
		release()
		return Result{}, err
	}
	saved, _, created, err := addDatabaseFrom(ctx, env,
		func(cfg config.Cluster) (config.ClusterSpec, error) {
			src, ok := cfg.ClusterByName(p.Source)
			if !ok {
				return config.ClusterSpec{}, databaseNotFound(p.Source)
			}
			spec := followerSpec(src, p.Name, p.Size)
			if err := followCPU(&spec, p); err != nil {
				return config.ClusterSpec{}, err
			}
			spec.FollowGeneration = generation
			return spec, nil
		}, p.AllowOvercommit, followCommand(p),
		func(cfg config.Cluster, spec config.ClusterSpec) error {
			if err := checkSpareCPU(ctx, spec.Name, spec.CPUMode); err != nil {
				return err
			}
			src, ok := cfg.ClusterByName(spec.Follows)
			if !ok {
				return databaseNotFound(spec.Follows)
			}
			return refuseUnsafeFollowFloors(ctx, cfg, src, spec, runtimeFor(env, false).Host, false)
		})
	release()
	if err != nil {
		return Result{}, err
	}
	env.Cluster = saved
	p.Generation = created.FollowGeneration
	p.PreparedNew = true
	id, err := followStartJob(ctx, env, "db-follow", p, stepSecrets(env.Req.Secrets))
	if err != nil {
		// The job removes the entry when it fails; one that never started
		// can't. The cleanup serializes with every desired-state writer and
		// removes only the untouched entry this request prepared.
		return Result{}, errors.Join(err, rollbackPreparedFollow(ctx, p))
	}
	res, err := followJob(ctx, env, id)
	if err != nil {
		return res, err
	}
	if m, ok := res.Data.(map[string]any); ok {
		m["database"] = created
	}
	res.Warnings = append(res.Warnings, oversubscriptionWarnings(saved, host)...)
	return res, nil
}

// followPrepareAcquire takes the node lock that prepares a fresh follower
// entry. It is a variable so handler tests run the real preparation without
// the node's /run/lock files.
var followPrepareAcquire = func(ctx context.Context, requestID string, timeout time.Duration) (engine.Release, error) {
	return engine.Acquire(ctx, engine.LockExclusive, "db.follow", requestID, timeout)
}

// followStartJob starts the detachable follow job; a handler test fails it
// after the prepared entry exists to exercise the serialized cleanup.
var followStartJob = startJob

// followBeforePrepareLock runs after preflight and before the preparation lock;
// a test uses it to change desired state while the preflight facts are stale.
var followBeforePrepareLock = func() {}

// followerPreviewEntry is the exact proposed entry for a dry run or preflight:
// placed on a copy of cfg, so the target appears exactly once without writing.
func followerPreviewEntry(cfg config.Cluster, src config.ClusterSpec, p dbFollowParams) (config.Cluster, config.ClusterSpec, error) {
	preview := cfg
	spec := followerSpec(src, p.Name, p.Size)
	if err := followCPU(&spec, p); err != nil {
		return config.Cluster{}, config.ClusterSpec{}, err
	}
	if err := placeCluster(preview, &spec); err != nil {
		return config.Cluster{}, config.ClusterSpec{}, err
	}
	preview.Clusters = append(preview.Clusters, spec)
	return preview, spec, nil
}

// followSpace checks the copy's free space; a variable so admission tests can
// exercise the handler without a source cluster.
var followSpace = restoreSpace

var followClusterExists = steps.ClusterExists

// refuseFollowWithUnfollow makes the two operation kinds mutually exclusive.
// Once a durable unfollow record owns name, a follow (preview, admission or
// the locked job) must not adopt, recopy or start the target behind that newer
// irreversible transition. The unfollow's own public retry is unaffected.
func refuseFollowWithUnfollow(recs backup.Records, name string) error {
	if _, ok := recs.Unfollowing[name]; !ok {
		return nil
	}
	return output.NewError(output.CodePrecondition,
		name+" has an unfinished unfollow that owns it, so a follow cannot start or resume", output.ExitPrecondition,
		"Finish it with vaire db unfollow "+name+" --confirm "+name+"/unfollow, then retry.").
		WithDetails(map[string]any{"reason": "unfollow_in_progress", "database": name})
}

// refuseUnfinishedFollowApply keeps a general plan or apply away from an
// ordinary follow's entry until its own job has materialised and started it.
// The engine view already leaves the entry out; this makes --db name the real
// reason instead of a missing-database validation error.
func refuseUnfinishedFollowApply(cfg config.Cluster, db string) error {
	if db == "" {
		return nil
	}
	spec, ok := cfg.ClusterByName(db)
	if !ok || !spec.IsFollower() || spec.FollowGeneration == "" {
		return nil
	}
	return output.NewError(output.CodePrecondition,
		db+" is an unfinished db follow, so apply must not create or start it outside the follow job", output.ExitPrecondition,
		"Retry the follow: vaire db follow "+spec.Follows+" --name "+db+".").
		WithDetails(map[string]any{"reason": "follow_unfinished", "database": db, "source": spec.Follows})
}

// followRetryable reports whether spec is the unfinished ordinary follow the
// same public db follow retry may continue. The prepared entry's generation is
// the operation identity; matching ownership proves unfinished progress even
// while the target already runs. A completed entry (empty generation), a
// replaced entry, and local paths or a running cluster without matching
// ownership are refused rather than reused or removed. An unfinished unfollow
// that owns the target always wins: its promotion is irreversible and a
// follow must not resume across it.
func followRetryable(cfg config.Cluster, recs backup.Records, spec config.ClusterSpec) (bool, error) {
	if err := refuseFollowWithUnfollow(recs, spec.Name); err != nil {
		return false, err
	}
	if recs.IsAwaiting(spec.Name) || spec.FollowGeneration == "" {
		// Recovery has its own awaiting identity; a missing generation is
		// never evidence of an unfinished ordinary follow.
		return false, nil
	}
	own, owned, err := readFollowOwnership(followOwner{Name: spec.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec})
	if err != nil {
		return false, err
	}
	if err := refuseIncompleteFollowAttempt(own); err != nil {
		return false, err
	}
	art, err := followerArtefactsOf(cfg, spec.Name)
	if err != nil {
		return false, err
	}
	if owned {
		// A running target is not completed: the job resumes the unfinished
		// copy and only the completion write makes it complete.
		return own.Phase != followPhaseStarted, nil
	}
	// Anything materialised or running without matching ownership is foreign
	// or uncertain. An untouched prepared entry (no generation progress) is
	// retried from its own fresh preparation.
	return !art.any() && !clusterRunning(spec), nil
}

// followFloorMismatch is one hot-standby setting whose running source value is
// above the floor a proposed follower would start with.
type followFloorMismatch struct {
	Setting  string `json:"setting"`
	Source   int    `json:"source"`
	Follower int    `json:"follower"`
}

// followerEffectiveFloors reads the source's currently effective floor values
// from its running PostgreSQL. It is a variable so admission tests can present
// a deferred source restart without a server.
var followerEffectiveFloors = steps.RunningFloors

// followFloorMismatches compares a proposed follower's rendered floors with
// the source's running values. cfg must contain exactly the proposed target
// once: its presence changes CPU-derived tuning, so a pre-member config would
// not be the configuration the follower actually starts with. A missing
// running value is unknown, not safe.
func followFloorMismatches(cfg config.Cluster, src, spec config.ClusterSpec, host hostinfo.Info, running map[string]int) ([]followFloorMismatch, error) {
	seen := 0
	for _, c := range cfg.Clusters {
		if c.Name == spec.Name {
			seen++
		}
	}
	if seen != 1 {
		return nil, fmt.Errorf("the proposed follower %s appears %d times in the configuration, want exactly once", spec.Name, seen)
	}
	floors, err := steps.FollowerFloors(cfg, spec, src, host)
	if err != nil {
		return nil, err
	}
	var out []followFloorMismatch
	for _, floor := range floors {
		runningValue, ok := running[floor.Name]
		if !ok {
			return nil, fmt.Errorf("%s did not report its effective %s, so the follower's floor cannot be checked", src.Name, floor.Name)
		}
		value, err := strconv.Atoi(floor.Value)
		if err != nil {
			return nil, fmt.Errorf("the rendered floor %s = %q is not an integer: %w", floor.Name, floor.Value, err)
		}
		if value < runningValue {
			out = append(out, followFloorMismatch{Setting: floor.Name, Source: runningValue, Follower: value})
		}
	}
	return out, nil
}

// refuseUnsafeFollowFloors refuses a follower whose configured floors are below
// the source's still effective values. It reports the actual and proposed
// floors and the supported explicit-setting remedy. A retry names the existing
// follower's own settings; a fresh proposal names the source's existing
// settings, because the proposed follower is not in cluster.toml yet and the
// follower inherits the larger of the two. The proposed membership itself
// changes CPU-derived tuning, so a restart of a still one-cluster source would
// not necessarily lower it, and no automatic restart or derived setting is
// safe. Unknown facts fail closed.
func refuseUnsafeFollowFloors(ctx context.Context, cfg config.Cluster, src, spec config.ClusterSpec, host hostinfo.Info, existing bool) error {
	running, err := followerEffectiveFloors(ctx, src)
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"can't verify the running hot-standby floors of "+src.Name+", so "+spec.Name+" can't be followed safely: "+err.Error(), output.ExitPrecondition,
			"Check that "+src.Name+" is running and answers PostgreSQL, then retry vaire db follow "+src.Name+" --name "+spec.Name+".").
			WithDetails(map[string]any{"reason": "source_floors_unknown", "database": spec.Name, "source": src.Name})
	}
	mismatches, err := followFloorMismatches(cfg, src, spec, host, running)
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"can't verify the hot-standby floors "+spec.Name+" would start with, so the follow is refused: "+err.Error(), output.ExitPrecondition,
			"Check cluster.toml's settings for "+src.Name+" and "+spec.Name+", then retry.").
			WithDetails(map[string]any{"reason": "source_floors_unknown", "database": spec.Name, "source": src.Name})
	}
	if len(mismatches) == 0 {
		return nil
	}
	parts := make([]string, 0, len(mismatches))
	settings := make([]string, 0, len(mismatches))
	prerequisite := "Set the follower's explicit floors to the running values"
	if !existing {
		prerequisite = "Set the source's explicit floors to its running values, so the proposed follower inherits them"
	}
	for _, m := range mismatches {
		parts = append(parts, fmt.Sprintf("%s %d > %d", m.Setting, m.Source, m.Follower))
		target := spec.Name
		if !existing {
			target = src.Name
		}
		settings = append(settings, fmt.Sprintf("vaire config set clusters.%s.settings.%s %d", target, m.Setting, m.Source))
	}
	return output.NewError(output.CodePrecondition,
		src.Name+" is still running with hot-standby settings above "+spec.Name+"'s floors ("+strings.Join(parts, ", ")+"), so the follower would pause replay",
		output.ExitPrecondition,
		prerequisite+" ("+strings.Join(settings, "; ")+"), then retry vaire db follow "+src.Name+" --name "+spec.Name+".").
		WithDetails(map[string]any{"reason": "source_floors_above_follower", "database": spec.Name, "source": src.Name, "settings": mismatches})
}

// jobFollow makes a follower's cluster (D2): the slot on the source, the
// copy from the source's stanza or with pg_basebackup, adopted as
// 18/<name> with its whole configuration written before its first start,
// then its own steps (the cluster starts and streams), the poolers,
// metrics and the other databases' changed configuration. A failure before
// the follower is 18/<name> drops the copy, the slot and the entry (or, for
// db recover, leaves it awaiting recovery).
//
// The job holds its node and cluster locks by the time it runs, so it reloads
// cluster.toml and records.json before any effect: a writer that changed or
// removed the target while the job waited is respected, and an operation whose
// entry, source or awaiting record no longer matches the one it was admitted
// for is refused rather than built under stale names.
func jobFollow(ctx context.Context, env *jobEnv) (any, error) {
	var p dbFollowParams
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
	spec, ok := cfg.ClusterByName(p.Name)
	if !ok || !spec.IsFollower() {
		return nil, output.NewError(output.CodePrecondition, p.Name+" is not a follower in cluster.toml", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "not_a_follower", "database": p.Name})
	}
	if spec.Follows != p.Source {
		return nil, output.NewError(output.CodePrecondition,
			p.Name+" now follows "+spec.Follows+", not "+p.Source+", so this follow no longer applies", output.ExitPrecondition,
			"Run vaire db follow for the source cluster.toml names.").
			WithDetails(map[string]any{"reason": "config_changed", "database": p.Name, "source": spec.Follows})
	}
	src, err := clusterOf(cfg, spec.Follows)
	if err != nil {
		return nil, err
	}
	if p.Recover {
		if !recs.IsAwaiting(p.Name) {
			return nil, output.NewError(output.CodePrecondition, p.Name+" isn't awaiting recovery", output.ExitPrecondition,
				"db recover is for databases a rebuilt master hasn't recovered.").
				WithDetails(map[string]any{"reason": "not_awaiting_recovery", "database": p.Name})
		}
		if p.RecoverSince == "" || recs.AwaitingRecovery[p.Name].Since != p.RecoverSince {
			return nil, output.NewError(output.CodePrecondition,
				p.Name+"'s awaiting recovery record changed while this recovery waited, so it no longer applies", output.ExitPrecondition,
				"Run vaire db recover again for the record records.json now has.").
				WithDetails(map[string]any{"reason": "config_changed", "database": p.Name})
		}
		if recs.IsAwaiting(src.Name) {
			return nil, output.NewError(output.CodePrecondition, p.Name+" follows "+src.Name+", which awaits recovery too", output.ExitPrecondition,
				"Recover the source first: vaire db recover "+src.Name+" (vaire db recover --all does sources first).").
				WithDetails(map[string]any{"reason": "source_awaiting_recovery", "database": p.Name, "source": src.Name})
		}
		if err := checkFollowSource(src, "db recover"); err != nil {
			return nil, err
		}
	} else {
		if err := refuseFollowWithUnfollow(recs, p.Name); err != nil {
			return nil, err
		}
		if recs.IsAwaiting(p.Name) {
			return nil, awaitingError(p.Name, "a follow")
		}
		if recs.IsAwaiting(src.Name) {
			return nil, output.NewError(output.CodePrecondition, p.Name+" follows "+src.Name+", which awaits recovery too", output.ExitPrecondition,
				"Recover the source first: vaire db recover "+src.Name+" (vaire db recover --all does sources first).").
				WithDetails(map[string]any{"reason": "source_awaiting_recovery", "database": p.Name, "source": src.Name})
		}
		if p.Generation == "" || spec.FollowGeneration != p.Generation {
			return nil, output.NewError(output.CodePrecondition,
				p.Name+"'s follow generation changed while this job waited, so it no longer prepares this copy", output.ExitPrecondition,
				"Run vaire db follow again for the entry cluster.toml now has.").
				WithDetails(map[string]any{"reason": "config_changed", "database": p.Name})
		}
		if err := checkFollowSource(src, "db follow"); err != nil {
			return nil, err
		}
		// The source may have shrunk its desired floors with its restart
		// deferred while this job waited. Re-read the running facts and
		// compare the exact current entry before ownership, slot, copy or
		// start effects.
		if err := refuseUnsafeFollowFloors(ctx, cfg, src, spec, jobRuntime(env).Host, true); err != nil {
			return nil, err
		}
	}
	owner := followOwner{Name: p.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec}
	if p.Recover {
		owner.Awaiting = recs.AwaitingRecovery[p.Name]
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
		// An adopted copy whose retained slot still holds its WAL is started
		// as it is; every other recovery continues or rebuilds through the
		// same owned build path.
		decision, err := decideFollowerBuild(ctx, cfg, src, spec, true, false)
		if err != nil {
			return nil, err
		}
		build = !decision.StartOnly
	}
	if build {
		if err := followerServerOps.build(ctx, env, cfg, src, spec, p.From, p.Recover); err != nil {
			if p.Recover {
				return nil, err
			}
			return nil, rollbackFailedFollow(p, err)
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
	warnings, st, err := followerServerOps.start(ctx, env, cfg, src, spec, recovering)
	if err != nil {
		return nil, err
	}
	if err := clearFollowGeneration(spec.Name, spec.FollowGeneration); err != nil {
		return nil, err
	}
	if p.Recover {
		// The follower runs with its own configuration: recovery is done and
		// it no longer awaits it. The ownership record outlives this write
		// only as best-effort cleanup.
		if _, err := clearAwaiting(p.Name); err != nil {
			return nil, err
		}
	} else if !env.retainedFollowerReservation {
		if err := writeFollowOwnership(owner, followPhaseStarted); err != nil {
			logLine(env.Report, env.Job, "the completed follow's durable mark was not written: "+err.Error())
		}
	}
	if env.retainedFollowerReservation {
		warnings = append(warnings, output.Warning{Code: "follower_reservation_retained",
			Message: "The follower started, but its historical temporary port reservation and ownership record were retained because their exact identity is unavailable."})
	} else {
		if err := clearFollowOwnership(p.Name); err != nil {
			logLine(env.Report, env.Job, "the interrupted follow record was left: "+err.Error())
		}
	}
	return map[string]any{"source": src.Name, "follows": src.Name, "name": p.Name, "port": spec.Port, "from": p.From,
		"slot": config.FollowerSlot(p.Name), "follower": st, "warnings": warnings}, nil
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
// instead of copying again. It is the preview of the same decision the recovery
// build makes: an adopted or owned completed copy needs a reserved or extended
// slot with a restart position, and a copy nothing proves is the operation's is
// refused rather than removed.
func followerRecoveryResumes(ctx context.Context, cfg config.Cluster, src, spec config.ClusterSpec) (followerResumePreview, error) {
	return followerResumePreviewFor(ctx, cfg, src, spec, true)
}

// followerFollowResume is followerRecoveryResumes for the public retry of an
// ordinary follow: the same build decision, with no awaiting record to bind.
func followerFollowResume(ctx context.Context, cfg config.Cluster, src, spec config.ClusterSpec) (followerResumePreview, error) {
	return followerResumePreviewFor(ctx, cfg, src, spec, false)
}

func followerResumePreviewFor(ctx context.Context, cfg config.Cluster, src, spec config.ClusterSpec, recover bool) (followerResumePreview, error) {
	decision, err := decideFollowerBuild(ctx, cfg, src, spec, recover, true)
	if err != nil {
		return followerResumePreview{}, err
	}
	return followerResumePreview{
		Resume:       decision.Resume || decision.StartOnly,
		RecopyReason: decision.Reason,
		Conditional:  decision.Conditional,
	}, nil
}

// followerBuildDecision is what a follow or recovery build must do with the
// deterministic paths: continue the completed copy's adoption, remove and copy
// again, or copy with no prior local path.
type followerBuildDecision struct {
	// Resume adopts the same completed copy again instead of copying it.
	Resume bool
	// StartOnly is an adopted copy whose slot still retains its WAL: it needs
	// no build work at all, only the follower's start.
	StartOnly bool
	// Clean removes the owned artefacts before a fresh copy.
	Clean bool
	// Copy makes a fresh copy after any cleanup.
	Copy bool
	// ClearAdopted invalidates the start-only marker before this operation
	// destroys or replaces the copy it describes.
	ClearAdopted bool
	// Reason explains a fresh copy in a preview.
	Reason string
	// Conditional marks a preview that could not read the source slot.
	Conditional bool
}

var followFinalRegistration = func(cfg config.Cluster, src, spec config.ClusterSpec) error {
	return backup.ResumeMaterialised(cfg.Server.DataRoot, spec.Name, src.Name, src.Port, engine.Shell{}, "", nil).VerifyFollowerRegistration(spec.Port)
}

// decideFollowerBuild reads the ownership record and the deterministic paths
// and decides the build. A completed copy is one whose registration points at
// its data directory and whose ownership phase is copied, or, for a recovery,
// whose awaiting record says it was adopted. It is only resumed on a slot that
// is reserved or extended with a restart position; every other slot takes the
// proven-owned recopy, and an unreadable slot fails closed. preview reports an
// unread slot as conditional instead of failing.
func decideFollowerBuild(ctx context.Context, cfg config.Cluster, src, spec config.ClusterSpec, recover, preview bool) (followerBuildDecision, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return followerBuildDecision{}, err
	}
	awaiting := backup.Awaiting{}
	if recover {
		awaiting = recs.AwaitingRecovery[spec.Name]
	}
	owner := followOwner{Name: spec.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec, Awaiting: awaiting}
	own, owned, err := readFollowOwnership(owner)
	if err != nil {
		return followerBuildDecision{}, err
	}
	if err := refuseIncompleteFollowAttempt(own); err != nil {
		return followerBuildDecision{}, err
	}
	if owned && own.Phase == followPhaseStarted && !recover {
		return followerBuildDecision{}, output.NewError(output.CodePrecondition, spec.Name+" already completed its follow", output.ExitPrecondition,
			"Run vaire db list to see the follower.").WithDetails(map[string]any{"reason": "database_exists", "database": spec.Name})
	}
	art, err := followerArtefactsOf(cfg, spec.Name)
	if err != nil {
		return followerBuildDecision{}, err
	}
	adopted := recover && awaiting.Adopted
	if !art.any() {
		if own.Attempt != nil {
			return followerBuildDecision{}, errors.New("completed follower attempt has no verifiable artifacts")
		}
		// A stale adopted marker describes no local copy: clear it before
		// making a new one.
		return followerBuildDecision{Copy: true, ClearAdopted: adopted}, nil
	}
	completed := art.Match && art.Data && ((owned && own.Phase == followPhaseCopied) || adopted)
	if !completed {
		if own.Attempt != nil {
			return followerBuildDecision{}, errors.New("follower attempt artifacts do not match")
		}
		if !owned || art.Conf && !art.Match {
			return followerBuildDecision{}, clusterExistsRefusal(spec.Name)
		}
		return followerBuildDecision{}, errors.New("unbound follower artifacts cannot authorize cleanup")
	}
	slot := config.FollowerSlot(spec.Name)
	state, err := followerSlotOps.inspect(ctx, src, slot)
	if err != nil {
		if preview {
			return followerBuildDecision{Reason: "the replication slot " + slot + " on " + src.Name + " could not be read", Conditional: true}, nil
		}
		return followerBuildDecision{}, err
	}
	if state.Usable {
		if adopted {
			if err := followFinalRegistration(cfg, src, spec); err != nil {
				return followerBuildDecision{}, err
			}
		}
		return followerBuildDecision{Resume: true, StartOnly: adopted}, nil
	}
	if owned && own.Attempt != nil && own.Attempt.State == followAttemptCompleted &&
		own.Attempt.ArtifactStage == materialisationRegistered &&
		own.Attempt.ReservationIdentity.Inode != 0 && own.Attempt.ReservationIdentity.Digest != "" {
		return followerBuildDecision{Clean: true, Copy: true, ClearAdopted: adopted,
			Reason: "the replication slot " + slot + " on " + src.Name + " cannot retain the WAL the completed copy needs"}, nil
	}
	return followerBuildDecision{}, errors.New("owned follower copy cannot be recopied without checked cleanup")
}

// clearFollowerAdopted durably clears the start-only marker before a recopy
// removes or replaces the copy it describes. A process that dies after this
// write retries the recopy, never a start of a copy that is gone.
func clearFollowerAdopted(name string) error {
	return backup.UpdateRecords(func(s *backup.Records) error {
		aw, ok := s.AwaitingRecovery[name]
		if !ok || !aw.Adopted {
			return nil
		}
		aw.Adopted = false
		s.AwaitingRecovery[name] = aw
		return nil
	})
}

// clearFollowGeneration durably clears an ordinary follow's prepared-entry
// generation after the follower has started successfully. It is the completion
// marker: ownership is deleted only after this save, and a failed save leaves
// the operation unfinished rather than reporting success.
func clearFollowGeneration(name, generation string) error {
	if generation == "" {
		return nil
	}
	store := followerState()
	cfg, _, err := store.Load()
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name != name {
			continue
		}
		if cfg.Clusters[i].FollowGeneration != generation {
			return output.NewError(output.CodePrecondition,
				name+"'s follow generation changed before its completion was recorded", output.ExitPrecondition,
				"Run vaire db follow again for the entry cluster.toml now has.").
				WithDetails(map[string]any{"reason": "config_changed", "database": name})
		}
		cfg.Clusters[i].FollowGeneration = ""
		found = true
	}
	if !found {
		return output.NewError(output.CodePrecondition, name+" is no longer in cluster.toml, so its follow completion can't be recorded", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "not_a_follower", "database": name})
	}
	raw, err := cfg.Encode()
	if err != nil {
		return err
	}
	return store.Save(raw)
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
// port. A plain follow records ownership before its first local mutation too,
// so a public retry can continue the same operation. A completed copy is
// adopted again instead of recopied, and only when the source slot still
// retains the WAL it needs; a missing, lost, unreserved or unknown slot makes
// the operation remove its own copy and copy again. An adoption error keeps
// the owned copy for the retry. Foreign paths are refused.
func materialiseFollower(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec, from string, recover bool) error {
	recs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	awaiting := backup.Awaiting{}
	if recover {
		awaiting = recs.AwaitingRecovery[spec.Name]
		if _, ok := recs.AwaitingRecovery[spec.Name]; !ok {
			return output.NewError(output.CodePrecondition, spec.Name+" isn't awaiting recovery", output.ExitPrecondition,
				"db recover is for databases a rebuilt master hasn't recovered.").
				WithDetails(map[string]any{"reason": "not_awaiting_recovery", "database": spec.Name})
		}
	}
	decision, err := decideFollowerBuild(ctx, cfg, src, spec, recover, false)
	if err != nil {
		return err
	}
	if decision.StartOnly {
		return nil
	}
	if decision.Resume {
		return resumeFollowerAdoption(ctx, env, cfg, src, spec)
	}
	owner := followOwner{Name: spec.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec, Awaiting: awaiting}
	if !decision.Copy {
		return fmt.Errorf("the follow of %s has nothing to build", spec.Name)
	}
	// Invalidate the start-only marker before anything destructive, then
	// record that this attempt is the copy's owner again. If either write
	// fails nothing local has been touched.
	if decision.ClearAdopted {
		if err := clearFollowerAdopted(spec.Name); err != nil {
			return err
		}
	}
	if decision.Clean {
		// Keep the born identities until exact cleanup has removed the
		// previous copy, before moving its replication slot.
		err := withLockedJobTarget(env, spec.Name, func() error {
			cleanup, err := openFollowCleanup(cfg, spec)
			if err != nil {
				return err
			}
			if cleanup == nil {
				return errors.New("follower cleanup authority missing")
			}
			defer cleanup.close()
			if err := cleanup.remove(ctx, &Env{Cluster: cfg, Report: env.Report}, cfg); err != nil {
				return err
			}
			return cleanup.retire(ctx)
		})
		if err != nil {
			return fmt.Errorf("remove the interrupted copy of %s: %w", spec.Name, err)
		}
	}
	if err := writeFollowOwnership(owner, followPhaseCopying); err != nil {
		return err
	}
	slot := config.FollowerSlot(spec.Name)
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
	ownedCopy, ok := m.(*backup.Materialised)
	if !ok {
		return errors.New("follower materialisation capability required")
	}
	defer ownedCopy.ReleaseForkReservationAuthority()
	destination, err := prepareMaterialisationConfig(cfg, spec)
	if err != nil {
		return err
	}
	ownedCopy.DestinationConfig = destination
	key := followAttemptKey{Job: env.ID, Operation: env.OperationID, SourceStanza: src.Stanza, TemporaryPort: ownedCopy.Port}
	reservationID, err := ownedCopy.ForkReservationIdentity()
	if err != nil {
		return err
	}
	if err := withLockedJobTarget(env, spec.Name, func() error {
		return ownedCopy.WithForkReservation(ctx, func() error { return beginFollowAttemptWithReservation(owner, key, reservationID) })
	}); err != nil {
		return err
	}
	birth, err := newFollowBirth(env, ownedCopy, owner, key, cfg.Server.DataRoot, src.Port)
	if err != nil {
		return err
	}
	defer birth.close()
	ownedCopy.Ownership = birth
	if from == followFromStanza {
		recs, _ := backup.LoadRecords()
		err = followerRestoreCopy(ownedCopy, ctx, driver(src, env.Job, env.Report), backup.RestoreOptions{
			Kind: backup.TargetLatest, Pause: true, Timeline: recs.TargetTimeline(src.Name, src.Stanza),
		})
	} else {
		err = followerStandbyCopy(ownedCopy, ctx, slot)
	}
	if err != nil {
		if reason := recoveryFailureReason(err); reason != "" {
			return errors.Join(err, birth.finish(ctx, reason))
		}
		// A failed birth remains pending rather than authorizing an unsafe
		// legacy name-only drop or slot reset.
		return err
	}
	if err := birth.finish(ctx, ""); err != nil {
		return err
	}
	ownedCopy.ReleaseForkReservationAuthority()
	return resumeFollowerAdoption(ctx, env, cfg, src, spec)
}

// rollbackFailedFollow rolls back only a fresh preparation whose build failed
// before any progress was recorded. A public retry never removes its existing
// entry, and a build that wrote ownership or materialised anything keeps the
// entry and record for that retry. The caller holds the job's exclusive lock.
func rollbackFailedFollow(p dbFollowParams, cause error) error {
	if !p.PreparedNew {
		return cause
	}
	if err := forgetPreparedFollow(p); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// followRollbackAcquire takes the exclusive node lock a cleanup that holds no
// job lock needs. It is a variable so a test proves the wait with its own
// locks instead of the node's /run/lock files.
var followRollbackAcquire = func(ctx context.Context, job, requestID string) (engine.Release, error) {
	return engine.Acquire(ctx, engine.LockExclusive, job+" rollback", requestID, followRollbackWait)
}

// followRollbackWait bounds a skipped job's cleanup wait: the operation that
// held the lock when the job was skipped has to finish first.
const followRollbackWait = time.Minute

// followBeforeRollback runs after the rollback lock is held and before
// cluster.toml is reloaded. A test uses it to hold two rollbacks at the
// boundary and prove they serialize.
var followBeforeRollback = func(string) {}

// rollbackPreparedFollow runs the fresh-preparation cleanup after a job that
// never started. It takes the node lock itself; a retry invocation and any
// progressed entry are never removed.
func rollbackPreparedFollow(ctx context.Context, p dbFollowParams) error {
	if !p.PreparedNew {
		return nil
	}
	release, err := followRollbackAcquire(ctx, "db-follow", p.Name)
	if err != nil {
		return err
	}
	defer release()
	return forgetPreparedFollow(p)
}

// forgetPreparedFollow removes a fresh prepared entry under the caller's
// exclusive node lock, and only when it is still untouched: same generation,
// not completed, no ownership record (progress), no configuration/data
// artefact, not running, and not awaiting recovery. Anything else is preserved
// for a public retry or an operator decision.
func forgetPreparedFollow(p dbFollowParams) error {
	if !p.PreparedNew || p.Name == "" || p.Recover || p.Generation == "" {
		return nil
	}
	followBeforeRollback(p.Name)
	cfg, _, err := followerState().Load()
	if err != nil {
		return err
	}
	spec, ok := cfg.ClusterByName(p.Name)
	if !ok || !spec.IsFollower() || spec.Follows != p.Source || spec.FollowGeneration != p.Generation {
		// Removed, replaced or completed by another writer.
		return nil
	}
	if clusterRunning(spec) {
		return nil
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	if _, ok := recs.Unfollowing[p.Name]; ok {
		// A newer unfollow owns the target: never remove its entry on the
		// strength of this stale preparation.
		return nil
	}
	if recs.IsAwaiting(p.Name) {
		return nil
	}
	art, err := followerArtefactsOf(cfg, p.Name)
	if err != nil {
		return err
	}
	if art.any() {
		return nil
	}
	path, err := followOwnershipPath(p.Name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		// Any ownership record means the attempt progressed or another
		// operation's evidence is present; leave it untouched.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return forgetDatabase(p.Name)
}

// resumeFollowerAdoption finishes an owned copy whose registration a previous
// attempt completed. Adopt stops the temporary cluster and rewrites the same
// files, so it is safe on a partial registration or an already adopted one.
func resumeFollowerAdoption(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec) error {
	m := resumeFollowerCopy(cfg.Server.DataRoot, spec.Name, src.Name, src.Port, env.shell(), env.Job, env.Report)
	resumed, ok := m.(*backup.Materialised)
	if !ok {
		return errors.New("guarded follower resume capability required")
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	owner := followOwner{Name: spec.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec}
	var p dbFollowParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return err
	}
	if p.Recover {
		owner.Awaiting = recs.AwaitingRecovery[spec.Name]
	}
	result, err := resumed.AdoptResumedFollower(ctx, spec.Port, &followResumeGuard{
		env: env, owner: owner, dataRoot: cfg.Server.DataRoot, sourcePort: src.Port, finalPort: spec.Port, sourceStanza: src.Stanza,
	})
	if err != nil {
		return err
	}
	if result.ReservationRetained {
		env.retainedFollowerReservation = true
	}
	return nil
}

// startFollower writes the follower's whole managed configuration, including
// 60-vaire-follower.conf, before its first start under systemd (D2): a standby
// that started with defaults below the source's floors would pause replay at
// once, and one without primary_conninfo would never stream. Then it starts the
// follower and finishes the follow. recovering is a database awaiting recovery
// the steps must see (db recover), empty for a plain follow.
func startFollower(ctx context.Context, env *jobEnv, cfg config.Cluster, src, spec config.ClusterSpec, recovering string) ([]output.Warning, followerReading, error) {
	rt := jobRuntime(env)
	// The engine view leaves an unfinished ordinary follow out until its own
	// job completes; this run is that owned execution, so keep the target in
	// the view exactly as db recover keeps its awaiting database.
	keep := recovering
	if keep == "" {
		keep = spec.Name
	}
	if err := steps.WriteFollowerConf(cfg, spec, rt.Host); err != nil {
		return nil, followerReading{}, err
	}
	others := otherConfigs(ctx, steps.Catalog(), cfg, rt, spec.Name, "")
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, followRuns(spec.Name, others)...))
	logLine(env.Report, env.Job, "starting "+spec.Name+" as a standby of "+src.Name)
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Recovering: keep, Restarter: steps.Restarter(cfg), Report: env.Report})
	if err == nil {
		err = settleExistingTarget(ctx, cfg, rt, spec.Name, keep, env.Report)
	}
	if err != nil {
		return nil, followerReading{}, withHint(err, finishNewCluster(spec.Name))
	}
	var warnings []output.Warning
	if err := applyPoolers(ctx, cfg, rt, keep, env.Report); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the poolers were not reloaded, so " + spec.Name + " is not reachable through them until vaire apply: " + err.Error()})
	}
	if w := applyMetrics(ctx, cfg, rt, env.Report, spec.Name, keep); w != nil {
		warnings = append(warnings, *w)
	}
	warnings = append(warnings, applyOtherConfigs(ctx, steps.Catalog(), cfg, rt, others,
		engine.Options{Recovering: keep, Restarter: steps.Restarter(cfg), Report: env.Report})...)
	logLine(env.Report, env.Job, "waiting for "+spec.Name+" to stream from "+src.Name)
	st := waitStreaming(ctx, spec, src, streamWait)
	if !st.Streaming {
		warnings = append(warnings, output.Warning{Code: "follower_not_streaming",
			Message: spec.Name + " is not streaming from " + src.Name + " yet: vaire doctor --db " + spec.Name + " and vaire logs postgres --db " + spec.Name + " show why"})
	}
	if err := writeFollowerMetrics(ctx, cfg); err != nil {
		warnings = append(warnings, output.Warning{Code: "follower_metrics_failed",
			Message: "the follower metrics were not written: " + err.Error()})
	}
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
		res, err := cfg.Resources(spec)
		if err != nil {
			return Result{}, err
		}
		data := map[string]any{"name": p.Name, "source": src, "slot": config.FollowerSlot(p.Name),
			"renames": unfollowRenames(src, p.Name), "backups": cfg.BackupConfigured(), "dry_run": true,
			"cpu_policy": map[string]string{"mode": res.CPUMode, "quota": res.CPUQuota}}
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
	// A settled retry still clears a pause an earlier attempt left before the
	// alias is exposed again; the fence below replaces this with its own
	// resume while the transition is unsettled.
	resume := func() { followerServerOps.resume(ctx, p.Name) }

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
	if !unfinished.Settled {
		// The poolers' clients logged in as the source's roles. Fence them and
		// render the alias out of both poolers before the promotion, so a
		// client reconnecting or a pooler restart cannot reach the promoted
		// database while its privilege transition is unsettled.
		resume, err = followerServerOps.fence(ctx, env, cfg, p.Name)
		if err != nil {
			if resuming {
				return nil, mayHaveChangedPasswords(err)
			}
			return nil, err
		}
	}
	logLine(env.Report, env.Job, "promoting "+p.Name)
	if _, err := followerServerOps.promote(ctx, local, cfg.Server.DataDirectory(p.Name)); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	// The promoted cluster still holds the source's database and owner names.
	// Settle the protected admin policy against those exact names before the
	// renames expose a primary that an inherited owner membership can write.
	if err := followerServerOps.settle(ctx, local, spec, src, p.Name); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	if err := followerServerOps.rename(ctx, local, src, p.Name, env.Secrets, env.Report, env.Job); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	// A rename can stop part way, so settle again against whichever database
	// and owner names the cluster actually holds now.
	if err := followerServerOps.settle(ctx, local, spec, src, p.Name); err != nil {
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
	// The renames carry the new passwords, the protection policy is settled
	// and the cluster runs its own configuration: only now may the poolers'
	// rendered configuration expose the alias again.
	if err := markUnfollowSettled(p.Name); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	var warnings []output.Warning
	if w := followerServerOps.dropSlot(ctx, cfg, src, p.Name); w != nil {
		warnings = append(warnings, *w)
	}
	more, err := followerServerOps.finish(ctx, env, cfg, spec, src, sync.OnceFunc(resume))
	warnings = append(warnings, more...)
	if err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	if err := endUnfollow(p.Name); err != nil {
		return nil, mayHaveChangedPasswords(err)
	}
	res, err := cfg.Resources(spec)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": p.Name, "source": src, "stanza": spec.Stanza, "port": spec.Port, "warnings": warnings,
		"cpu_policy": map[string]string{"mode": res.CPUMode, "quota": res.CPUQuota}}, nil
}

// finishUnfollow makes the promoted follower independent: its own steps (the
// floors and the replication settings are gone), the poolers with the alias as
// a plain entry and their clients let in again, metrics, and with backups on
// its stanza, first full and timers.
func finishUnfollow(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec, source string, resume func()) ([]output.Warning, error) {
	rt := jobRuntime(env)
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, unfollowRuns(cfg, spec.Name)...))
	logLine(env.Report, env.Job, "restarting "+spec.Name+" with its own configuration")
	if err := settleExistingTarget(ctx, cfg, rt, spec.Name, "", env.Report); err != nil {
		return nil, withHint(err, finishNewCluster(spec.Name))
	}
	var warnings []output.Warning
	if w := unfollowAccessNotice(ctx, cfg, spec); w != nil {
		warnings = append(warnings, *w)
	}
	if err := applyPoolers(ctx, cfg, rt, "", env.Report); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the poolers were not reloaded, so " + spec.Name + " is still routed to " + source + "'s database name until vaire apply: " + err.Error()})
	} else {
		resume()
	}
	if w := applyMetrics(ctx, cfg, rt, env.Report, spec.Name, ""); w != nil {
		warnings = append(warnings, *w)
	}
	if w := firstBackup(ctx, cfg, rt, env.Report, spec.Name); w != nil {
		warnings = append(warnings, *w)
	}
	steps.WriteMetrics(cfg)
	if err := writeFollowerMetrics(ctx, cfg); err != nil {
		warnings = append(warnings, output.Warning{Code: "follower_metrics_failed",
			Message: "the follower metrics were not written: " + err.Error()})
	}
	return warnings, nil
}

// fenceUnfollowPoolers cuts name's pooler clients and renders its alias out
// of both poolers before db unfollow promotes it. The rendered files are the
// durable part: a pooler restart after an interrupted unfollow reads them and
// keeps the alias withheld until it is settled. A failure to pause, kill or
// render resumes what was paused and returns the error, so the job never
// promotes behind a fence it could not establish.
func fenceUnfollowPoolers(ctx context.Context, env *jobEnv, cfg config.Cluster, name string) (func(), error) {
	resume, err := steps.FencePoolerDatabase(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := steps.ApplyPoolerFence(ctx, cfg, jobRuntime(env), env.Report); err != nil {
		// Nothing irreversible has run: a follower is still a read-only
		// standby, so its clients may reconnect to it.
		resume()
		return nil, fmt.Errorf("render the poolers without %s: %w", name, err)
	}
	return resume, nil
}

// settleUnfollowAdminAccess applies the protected admin-access policy to the
// actual database and owner role names a promoted follower holds. Before the
// renames those are the source's names; after one they are the follower's.
// A partially finished rename can leave the database and the owner under
// different names, so both are read from the cluster rather than inferred.
func settleUnfollowAdminAccess(ctx context.Context, local sqlRunner, spec config.ClusterSpec, source, name string) error {
	if !spec.Protected {
		return nil
	}
	database, owner, err := currentUnfollowNames(ctx, local, source, name)
	if err != nil {
		return err
	}
	if err := local.Exec(ctx, "postgres", steps.AdminAccessSQLFor(owner, database, true), nil); err != nil {
		return fmt.Errorf("settle vaire_admin's access to %s: %w", database, err)
	}
	return nil
}

// currentUnfollowNames reads which database and owner role the promoted
// follower currently has: exactly one of the source's name and the follower's
// must exist for each. Anything else is ambiguous and refused rather than
// settling the wrong role.
func currentUnfollowNames(ctx context.Context, local sqlRunner, source, name string) (database, owner string, err error) {
	database, err = currentUnfollowName(ctx, local, "database", "datname", "pg_database", source, name, name)
	if err != nil {
		return "", "", err
	}
	owner, err = currentUnfollowName(ctx, local, "owner role", "rolname", "pg_roles", source+"_owner", name+"_owner", name)
	if err != nil {
		return "", "", err
	}
	return database, owner, nil
}

func currentUnfollowName(ctx context.Context, local sqlRunner, kind, column, catalog, before, after, follower string) (string, error) {
	rows, err := local.Query(ctx, "postgres",
		"SELECT "+column+" FROM "+catalog+" WHERE "+column+" IN ("+quoteLiteral(before)+", "+quoteLiteral(after)+") ORDER BY "+column, nil)
	if err != nil {
		return "", err
	}
	has := map[string]bool{}
	for _, row := range rows {
		has[row[column]] = true
	}
	switch {
	case has[before] && !has[after]:
		return before, nil
	case has[after] && !has[before]:
		return after, nil
	case has[before] && has[after]:
		return "", output.NewError(output.CodePrecondition, "both "+before+" and "+after+" exist as a "+kind+" in "+follower+"'s cluster", output.ExitPrecondition,
			"The unfollow can't settle protection until it knows which one is its own. Check the cluster (psql as postgres on the node), then retry vaire db unfollow "+follower+".")
	default:
		return "", output.NewError(output.CodePrecondition, "neither "+before+" nor "+after+" exists as a "+kind+" in "+follower+"'s cluster", output.ExitPrecondition,
			"The unfollow can't settle protection without it. Check the cluster (psql as postgres on the node), then retry vaire db unfollow "+follower+".")
	}
}

// markUnfollowSettled records that the unfollow has finished its privilege
// transition. Until then both poolers withhold the alias and node SQL refuses
// a protected database being unfollowed.
func markUnfollowSettled(name string) error {
	return backup.UpdateRecords(func(s *backup.Records) error {
		u, ok := s.Unfollowing[name]
		if !ok {
			return fmt.Errorf("the unfollow record for %s is missing", name)
		}
		if u.Settled {
			return nil
		}
		u.Settled = true
		s.Unfollowing[name] = u
		return nil
	})
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
		cfg.Clusters[i].Follows, cfg.Clusters[i].Stanza, cfg.Clusters[i].FollowGeneration = "", stanza, ""
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
	fp := dbFollowParams{Source: src.Name, Name: spec.Name, From: followFrom(ctx, env.Cluster, src), Recover: true,
		RecoverSince: recs.AwaitingRecovery[spec.Name].Since}
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
	_, owned, err := readFollowOwnership(followOwner{Name: spec.Name, Source: spec.Follows, Spec: spec, Awaiting: recs.AwaitingRecovery[spec.Name]})
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
