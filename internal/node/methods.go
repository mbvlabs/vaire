package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

// methods is the node's method table. Later phases add entries here.
func methods() map[string]method {
	list := []method{
		{
			name:    "plan",
			lock:    fixedLock(engine.LockShared),
			cluster: true,
			runLog:  always,
			handler: handlePlan,
		},
		{
			name: "apply",
			// apply --db changes one cluster: the global lock shared and that
			// cluster's exclusive, so applies to other clusters run beside it.
			// A source with followers whose selected steps can execute an
			// extension dependency transition takes the global exclusive
			// envelope and lets the handler take the sorted target locks.
			lockWithConfig:        applyLockWithConfig,
			clusterLockWithConfig: applyClusterLockWithConfig,
			cluster:               true,
			confirmAny:            restartTokens,
			runLog:                func(req rpc.Request) bool { return !req.DryRun },
			handler:               handleApply,
		},
		{
			name:    "selfcheck",
			handler: handleSelfcheck,
		},
		{
			name:    "cluster.bootstrap",
			lock:    fixedLock(engine.LockExclusive),
			confirm: bootstrapToken,
			runLog:  always,
			handler: handleClusterBootstrap,
		},
		{
			name:    "cluster.confirm-secrets",
			lock:    fixedLock(engine.LockExclusive),
			cluster: true,
			runLog:  always,
			handler: handleConfirmSecrets,
		},
		{
			name:    "ssh.lockdown",
			lock:    fixedLock(engine.LockExclusive),
			cluster: true,
			runLog:  always,
			handler: handleSSHLockdown,
		},
		{
			name:    "ssh.confirm",
			lock:    fixedLock(engine.LockExclusive),
			cluster: true,
			runLog:  always,
			handler: handleSSHConfirm,
		},
		{
			name:    "ssh.status",
			handler: handleSSHStatus,
		},
		{
			name:          "upgrade.install",
			lock:          fixedLock(engine.LockExclusive),
			runLog:        always,
			versionExempt: true,
			handler:       handleUpgradeInstall,
		},
		{
			name:    "config.get",
			handler: handleConfigGet,
		},
		{
			name: "config.set",
			lock: func(req rpc.Request) engine.LockMode {
				if req.DryRun {
					return engine.LockShared
				}
				return engine.LockExclusive
			},
			runLog:  func(req rpc.Request) bool { return !req.DryRun },
			handler: handleConfigSet,
		},
		{
			name:    "config.validate",
			handler: handleConfigValidate,
		},
		{
			// No run log: the result carries the new TOTP secret.
			name:    "protect.init",
			lock:    fixedLock(engine.LockExclusive),
			handler: handleProtectInit,
		},
		{
			name:    "protect.confirm",
			lock:    fixedLock(engine.LockExclusive),
			runLog:  always,
			handler: handleProtectConfirm,
		},
		{
			name:    "protect.status",
			handler: handleProtectStatus,
		},
		{
			// No run log: its request carries a TOTP code.
			name:    "protect.verify",
			handler: handleProtectVerify,
		},
		{
			name:    "db.list",
			lock:    fixedLock(engine.LockShared),
			cluster: true,
			handler: handleDBList,
		},
		{
			name:    "db.create",
			lock:    fixedLock(engine.LockExclusive),
			cluster: true,
			runLog:  always,
			handler: handleDBCreate,
		},
		{
			name:    "db.drop",
			lock:    fixedLock(engine.LockExclusive),
			cluster: true,
			confirm: dbToken,
			runLog:  always,
			handler: refuseProtected("db drop", "name", nil, handleDBDrop),
		},
		{
			// db resize changes cluster.toml and the kernel's huge pages, so it
			// holds the global lock; the restart is that cluster's alone.
			name: "db.resize",
			lock: func(req rpc.Request) engine.LockMode {
				if req.DryRun {
					return engine.LockShared
				}
				return engine.LockExclusive
			},
			clusterLock: func(req rpc.Request) (string, engine.LockMode) {
				if req.DryRun {
					return "", engine.LockNone
				}
				return paramName(req.Params), engine.LockExclusive
			},
			cluster:    true,
			confirmAny: restartTokens,
			runLog:     func(req rpc.Request) bool { return !req.DryRun },
			handler:    handleDBResize,
		},
		{
			name: "db.credentials",
			lock: fixedLock(engine.LockShared),
			clusterLock: func(req rpc.Request) (string, engine.LockMode) {
				if !rotateRequested(req.Params) {
					return "", engine.LockNone
				}
				return paramName(req.Params), engine.LockExclusive
			},
			cluster: true,
			confirm: dbCredentialsToken,
			runLog:  always,
			handler: refuseProtected("rotating its credentials", "name", rotateRequested, handleDBCredentials),
		},
		{
			name:    "tune.show",
			cluster: true,
			handler: handleTuneShow,
		},
		{
			name:       "tune.apply",
			lock:       fixedLock(engine.LockExclusive),
			cluster:    true,
			confirmAny: restartTokens,
			runLog:     always,
			handler:    handleTuneApply,
		},
		{
			name:    "ext.list",
			lock:    fixedLock(engine.LockShared),
			cluster: true,
			handler: handleExtList,
		},
		{
			name:        "ext.add",
			lock:        extensionMutationLock,
			clusterLock: extensionClusterLock,
			cluster:     true,
			confirmAny:  extensionAddTokens,
			runLog:      func(req rpc.Request) bool { return !req.DryRun },
			handler:     handleExtAdd,
		},
		{
			name:        "ext.remove",
			lock:        extensionMutationLock,
			clusterLock: extensionClusterLock,
			cluster:     true,
			confirmAny:  extensionRemoveTokens,
			runLog:      func(req rpc.Request) bool { return !req.DryRun },
			handler:     handleExtRemove,
		},
		{
			name:    "logs",
			cluster: true,
			handler: handleLogs,
		},
		{
			name:    "tls.status",
			cluster: true,
			handler: handleTLSStatus,
		},
		{name: "backup.now", cluster: true, runLog: always, handler: handleBackupNow},
		{name: "backup.list", lock: fixedLock(engine.LockShared), cluster: true, handler: handleBackupList},
		{name: "backup.verify", cluster: true, runLog: always, handler: handleBackupVerify},
		{name: "job.list", handler: handleJobList},
		{name: "job.follow", handler: handleJobFollow},
		{name: "restore.start", cluster: true, confirm: restoreToken, runLog: always, handler: refuseProtected("restore --promote", "database", restorePromotes, handleRestoreStart)},
		{name: "restore.status", cluster: true, handler: handleRestoreStatus},
		{name: "restore.promote", cluster: true, confirm: restoreToken, runLog: always, handler: refuseProtected("restore promote", "database", nil, handleRestorePromote)},
		{name: "restore.undo", cluster: true, confirm: restoreToken, runLog: always, handler: refuseProtected("restore undo", "database", nil, handleRestoreUndo)},
		{name: "restore.abort", lock: fixedLock(engine.LockShared), clusterLock: dbClusterLock, cluster: true, runLog: always, handler: handleRestoreAbort},
		{name: "restore.cleanup", lock: fixedLock(engine.LockShared), clusterLock: dbClusterLock, cluster: true, confirm: cleanupToken, runLog: always, handler: handleRestoreCleanup},
		{name: "restore.drill", cluster: true, runLog: always, handler: handleRestoreDrill},
		{name: "db.fork", cluster: true, runLog: always, handler: handleDBFork},
		// Phase 6: followers. Both start jobs, which take their own locks;
		// db.follow takes the exclusive lock itself to add the entry.
		{name: "db.follow", cluster: true, runLog: always, handler: handleDBFollow},
		{name: "db.unfollow", cluster: true, confirm: unfollowToken, runLog: always, handler: handleDBUnfollow},
		{name: "dump.create", cluster: true, runLog: always, handler: handleDumpCreate},
		{
			name:    "sql",
			lock:    fixedLock(engine.LockShared),
			cluster: true,
			handler: defaultProtectGuard.requireWriteCode("writing to it", "db", sqlWrites, handleSQL),
		},
		{
			// No run log, like sql, whose writes it serves.
			name:    "db.write-access",
			cluster: true,
			handler: handleWriteAccess,
		},
		// Phase 4: observability and maintenance.
		{name: "metrics.show", cluster: true, handler: handleMetricsShow},
		// status takes no lock: it must answer while an apply holds the
		// global one.
		{name: "status", cluster: true, handler: handleStatus},
		// Activity history reads take no lock and write no run log: querying
		// history must not become an operation recorded in that history.
		{name: "activity.list", handler: handleActivityList},
		{name: "activity.show", handler: handleActivityShow},
		{name: "activity.health", handler: handleActivityHealth},
		{name: "doctor.run", cluster: true, handler: handleDoctorRun},
		{name: "queries.top", cluster: true, handler: handleQueriesTop},
		{name: "maint.report", cluster: true, handler: handleMaintReport},
		// maint.run starts a job, which takes its own locks. Only a repack
		// needs the <db> token; vacuum, analyze and reindex keep no token.
		{name: "maint.run", cluster: true, confirm: maintRunToken, runLog: func(req rpc.Request) bool { return !req.DryRun }, handler: handleMaintRun},
		{name: "maint.kill", cluster: true, confirm: killToken, runLog: func(req rpc.Request) bool { return !req.DryRun }, handler: defaultProtectGuard.requireWriteCode("terminating its sessions", "db", nil, handleMaintKill)},
		{name: "queries.reset", cluster: true, confirm: statsToken, runLog: always, handler: handleQueriesReset},
		{name: "metrics.rotate", lock: fixedLock(engine.LockExclusive), cluster: true, confirm: metricsToken, runLog: always, handler: handleMetricsRotate},
		// Phase 5. The master backup takes no node lock (D3): it has its own.
		{name: "master-backup.now", cluster: true, runLog: func(req rpc.Request) bool { return !req.DryRun }, handler: handleMasterBackupNow},
		{name: "master-backup.list", cluster: true, handler: handleMasterBackupList},
		{name: "master-backup.get", cluster: true, handler: handleMasterBackupGet},
		{name: "db.recover", cluster: true, confirm: recoverToken, runLog: always, handler: refuseProtected("db recover --time", "database", recoverAtTime, handleDBRecover)},
	}
	table := make(map[string]method, len(list))
	for _, m := range list {
		table[m.name] = m
	}
	return table
}

// paramDB is params.db, or "".
func paramDB(params json.RawMessage) string {
	var p struct {
		DB string `json:"db"`
	}
	_ = json.Unmarshal(params, &p)
	return p.DB
}

// paramName is params.name, or "".
func paramName(params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params, &p)
	return p.Name
}

// dbClusterLock takes params.database's cluster lock exclusive: restore
// abort and cleanup change one database and leave the others alone. Promote
// and undo lock in their handlers (lockForCutover) instead, so they can let
// the lock go before the full backup starts. A dry run and a request without
// a database take none.
func dbClusterLock(req rpc.Request) (string, engine.LockMode) {
	var p struct {
		Database string `json:"database"`
	}
	if req.DryRun || json.Unmarshal(req.Params, &p) != nil {
		return "", engine.LockNone
	}
	return p.Database, engine.LockExclusive
}

// restartTokens are {server}/restart, which restarts every cluster one at a
// time, and <cluster>/restart for each cluster (RFC 11.2).
func restartTokens(cfg config.Cluster, _ json.RawMessage) []string {
	out := []string{cfg.Name() + "/restart"}
	for _, c := range cfg.Clusters {
		out = append(out, c.Name+"/restart")
	}
	return out
}

// applySelectionMayTransition reports whether the selected steps can execute
// an extension dependency transition (configuration, roles or packages), or
// the request selects everything.
func applySelectionMayTransition(req rpc.Request) bool {
	var p stepParams
	if json.Unmarshal(req.Params, &p) != nil || len(p.Steps) == 0 {
		return true
	}
	for _, id := range p.Steps {
		base, _ := engine.SplitID(id)
		switch base {
		case "postgres-config-v1", "postgres-roles-v1":
			return true
		}
	}
	return false
}

func applyLockWithConfig(cfg config.Cluster, req rpc.Request) engine.LockMode {
	if req.DryRun {
		return engine.LockShared
	}
	db := paramDB(req.Params)
	if db == "" {
		return engine.LockExclusive
	}
	// A fork/adoption/restore continuation saves settings and finalizes
	// routing. Its global envelope is exclusive from the pre-lock
	// configuration and activation-records snapshot; unreadable records are
	// unknown state and never fall back to shared.
	recs, err := backup.LoadRecords()
	if err != nil {
		return engine.LockExclusive
	}
	if len(recs.Activation) > 0 {
		return engine.LockExclusive
	}
	if _, ok := cfg.ClusterByName(db); ok && len(cfg.FollowerNames(db)) > 0 && applySelectionMayTransition(req) {
		return engine.LockExclusive
	}
	return engine.LockShared
}

func applyClusterLockWithConfig(cfg config.Cluster, req rpc.Request) (string, engine.LockMode) {
	if req.DryRun {
		return "", engine.LockNone
	}
	db := paramDB(req.Params)
	if db == "" {
		return "", engine.LockNone
	}
	if _, ok := cfg.ClusterByName(db); ok && len(cfg.FollowerNames(db)) > 0 && applySelectionMayTransition(req) {
		// The handler takes the complete source/follower set once, sorted.
		return "", engine.LockNone
	}
	return db, engine.LockExclusive
}

// restartConfirmation turns the request's checked restart tokens into what
// they allow.
func restartConfirmation(cfg config.Cluster, req rpc.Request) engine.Confirmation {
	var c engine.Confirmation
	for _, token := range req.ConfirmTokens() {
		name, _ := strings.CutSuffix(token, "/restart")
		if token == cfg.Name()+"/restart" {
			c.Server = true
			continue
		}
		c.Clusters = append(c.Clusters, name)
	}
	return c
}

// dbToken is the confirm token for db.drop and db.credentials: <db>.
func dbToken(_ config.Cluster, params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return p.Name
}

// dbCredentialsToken is <db> only when the request rotates a password.
func dbCredentialsToken(cfg config.Cluster, params json.RawMessage) string {
	if !rotateRequested(params) {
		return ""
	}
	return dbToken(cfg, params)
}

func rotateRequested(params json.RawMessage) bool {
	var p struct {
		Rotate bool `json:"rotate"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	return p.Rotate
}

type stepParams struct {
	Steps []string `json:"steps"`
	// DB limits plan and apply to one cluster's step instances.
	DB string `json:"db,omitempty"`
	// RotateBackupSecrets lets pgbackrest-v1 replace the bucket keys on
	// disk with the request's; only vaire backup setup sets it.
	RotateBackupSecrets bool `json:"rotate_backup_secrets,omitempty"`
	// SkipDoctor skips the doctor checks after a full apply (Phase 4).
	SkipDoctor bool `json:"skip_doctor,omitempty"`
}

// decodeParams decodes params strictly. Methods that cross versions decode
// leniently instead.
func decodeParams(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return output.NewError(output.CodeUsage, "params do not decode: "+err.Error(), output.ExitUsage, "")
	}
	return nil
}

// detectHost is hostinfo.Detect for a request's runtime facts. It is a
// variable so tests supply fixed host facts without weakening the admission
// checks that read them.
var detectHost = hostinfo.Detect

func runtimeFor(env *Env, dryRun bool) engine.Runtime {
	host, _ := detectHost(env.Cluster.Server.DataRoot)
	return engine.Runtime{
		DryRun:       dryRun,
		Shell:        engine.Shell{DryRun: dryRun, Secrets: secrets.Default, Log: env.Log},
		Secrets:      secrets.Default,
		SecretValues: env.Req.Secrets,
		Log:          env.Log,
		Host:         host,
	}
}

func handlePlan(ctx context.Context, env *Env) (Result, error) {
	var p stepParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := refuseUnfinishedFollowApply(env.Cluster, p.DB); err != nil {
		return Result{}, err
	}
	rt := runtimeFor(env, true)
	data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, rt, engine.Options{Only: p.Steps, DB: p.DB})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: oversubscriptionWarnings(env.Cluster, rt.Host)}, nil
}

// applyReloadCluster reads cluster.toml again after the global lock is held.
// It is a variable so tests can model a concurrent membership change.
var applyReloadCluster = func() (config.Cluster, error) {
	cfg, _, err := (state.FileStore{}).Load()
	return cfg, err
}

func handleApply(ctx context.Context, env *Env) (Result, error) {
	var p stepParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if !env.Req.DryRun {
		reloaded, err := applyReloadCluster()
		if err != nil {
			return Result{}, err
		}
		actualMode := env.LockMode
		if actualMode == engine.LockNone {
			// A direct handler test did not come through the dispatcher; use
			// the same pre-lock classification it would have retained.
			actualMode = applyLockWithConfig(env.Cluster, env.Req)
		}
		newMode := applyLockWithConfig(reloaded, env.Req)
		oldCluster, oldClusterMode := applyClusterLockWithConfig(env.Cluster, env.Req)
		newCluster, newClusterMode := applyClusterLockWithConfig(reloaded, env.Req)
		if actualMode != newMode || oldCluster != newCluster || oldClusterMode != newClusterMode {
			return Result{}, output.NewError(output.CodePrecondition,
				"cluster.toml changed while apply waited for its lock, so the locked target set is stale",
				output.ExitPrecondition, "Retry vaire apply; it will take the current envelope and target locks.").
				WithDetails(map[string]any{"reason": "apply_lock_class_changed", "database": p.DB})
		}
		env.Cluster = reloaded
	}
	if !env.Req.DryRun && p.DB != "" && len(p.Steps) == 0 {
		if f, ok, err := forkFenceFor(env.Cluster, p.DB); err != nil {
			return Result{}, err
		} else if ok {
			return resumeForkSettlementAt(ctx, env, f)
		}
	}
	if err := refuseActiveActivationFences(env.Cluster, p); err != nil {
		return Result{}, err
	}
	if err := refuseUnfinishedFollowApply(env.Cluster, p.DB); err != nil {
		return Result{}, err
	}
	var transitionWarnings []output.Warning
	if !env.Req.DryRun {
		warnings, release, err := applyExtensionTransitions(ctx, env, p)
		if err != nil {
			var pending *extensionApplyPending
			if errors.As(err, &pending) {
				if release != nil {
					release()
				}
				return Result{Data: pending.Data, Warnings: append(pending.Warnings, NoOffsiteWarning(env.Cluster)...)}, nil
			}
			return Result{}, err
		}
		if release != nil {
			defer release()
		}
		transitionWarnings = warnings
	}
	if env.Req.DryRun {
		rt := runtimeFor(env, true)
		data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, rt, engine.Options{Only: p.Steps, DB: p.DB})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: data, Warnings: append(NoOffsiteWarning(env.Cluster), oversubscriptionWarnings(env.Cluster, rt.Host)...)}, nil
	}
	rt := runtimeFor(env, false)
	rt.RotateSecrets = p.RotateBackupSecrets
	data, warnings, err := engine.Apply(ctx, steps.Catalog(), env.Cluster, rt, engine.Options{
		Only: p.Steps,
		DB:   p.DB,
		// The tokens were already checked against restartTokens.
		Confirm:   restartConfirmation(env.Cluster, env.Req),
		Restarter: steps.RestarterFor(env.Cluster, rt, env.Report),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, err
	}
	for _, r := range data.Restarts {
		clearDeferred(r.Cluster)
	}
	if len(data.RestartsSettled) > 0 {
		clearDeferred(data.RestartsSettled...)
	}
	warnings = append(warnings, finalizeRoutingFences(ctx, env, routingFencesFor(p))...)
	warnings = append(warnings, NoOffsiteWarning(env.Cluster)...)
	warnings = append(warnings, oversubscriptionWarnings(env.Cluster, rt.Host)...)
	warnings = append(transitionWarnings, warnings...)
	out := applyResult{ApplyData: data}
	// Step 18 (Phase 4 D10): a full apply ends with the doctor checks; a
	// failing one is a warning, since blocking apply would block its fix.
	if len(p.Steps) == 0 && !p.SkipDoctor {
		gate, doctorWarnings, _ := doctorGate(ctx, env.Cluster, runtimeFor(env, true), false)
		out.Doctor = gate
		warnings = append(warnings, doctorWarnings...)
	}
	return Result{Data: out, Warnings: warnings}, nil
}

// applyResult is apply's result: the steps and, after a full apply, the
// doctor gate.
type applyResult struct {
	engine.ApplyData
	Doctor *doctorData `json:"doctor,omitempty"`
}

func handleSelfcheck(context.Context, *Env) (Result, error) {
	data, warnings, err := SelfCheck()
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}
