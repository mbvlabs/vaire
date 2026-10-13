package node

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/steps"
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
			lock: func(req rpc.Request) engine.LockMode {
				if req.DryRun || paramDB(req.Params) != "" {
					return engine.LockShared
				}
				return engine.LockExclusive
			},
			clusterLock: func(req rpc.Request) (string, engine.LockMode) {
				if req.DryRun {
					return "", engine.LockNone
				}
				return paramDB(req.Params), engine.LockExclusive
			},
			cluster:    true,
			confirmAny: restartTokens,
			runLog:     func(req rpc.Request) bool { return !req.DryRun },
			handler:    handleApply,
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
			handler: handleDBDrop,
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
			handler: handleDBCredentials,
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
		{name: "restore.start", cluster: true, confirm: restoreToken, runLog: always, handler: handleRestoreStart},
		{name: "restore.status", cluster: true, handler: handleRestoreStatus},
		{name: "restore.promote", lock: fixedLock(engine.LockShared), clusterLock: dbClusterLock, cluster: true, confirm: restoreToken, runLog: always, handler: handleRestorePromote},
		{name: "restore.undo", lock: fixedLock(engine.LockShared), clusterLock: dbClusterLock, cluster: true, confirm: restoreToken, runLog: always, handler: handleRestoreUndo},
		{name: "restore.abort", lock: fixedLock(engine.LockShared), clusterLock: dbClusterLock, cluster: true, runLog: always, handler: handleRestoreAbort},
		{name: "restore.cleanup", lock: fixedLock(engine.LockShared), clusterLock: dbClusterLock, cluster: true, confirm: cleanupToken, runLog: always, handler: handleRestoreCleanup},
		{name: "restore.drill", cluster: true, runLog: always, handler: handleRestoreDrill},
		{name: "db.fork", cluster: true, runLog: always, handler: handleDBFork},
		{name: "dump.create", cluster: true, runLog: always, handler: handleDumpCreate},
		{
			name:    "sql",
			lock:    fixedLock(engine.LockShared),
			cluster: true,
			handler: handleSQL,
		},
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

// dbClusterLock takes params.database's cluster lock exclusive: the restore
// methods change one database and leave the others alone. A dry run and a
// request without a database take none.
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
	// disk with the request's; only eru backup setup sets it.
	RotateBackupSecrets bool `json:"rotate_backup_secrets,omitempty"`
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

func runtimeFor(env *Env, dryRun bool) engine.Runtime {
	host, _ := hostinfo.Detect(env.Cluster.Server.DataRoot)
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
	data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, true), engine.Options{Only: p.Steps, DB: p.DB})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data}, nil
}

func handleApply(ctx context.Context, env *Env) (Result, error) {
	var p stepParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, true), engine.Options{Only: p.Steps, DB: p.DB})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: data, Warnings: NoOffsiteWarning(env.Cluster)}, nil
	}
	rt := runtimeFor(env, false)
	rt.RotateSecrets = p.RotateBackupSecrets
	data, warnings, err := engine.Apply(ctx, steps.Catalog(), env.Cluster, rt, engine.Options{
		Only: p.Steps,
		DB:   p.DB,
		// The tokens were already checked against restartTokens.
		Confirm:   restartConfirmation(env.Cluster, env.Req),
		Restarter: steps.Restarter(env.Cluster),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: append(warnings, NoOffsiteWarning(env.Cluster)...)}, nil
}

func handleSelfcheck(context.Context, *Env) (Result, error) {
	data, warnings, err := SelfCheck()
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}
