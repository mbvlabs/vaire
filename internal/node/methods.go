package node

import (
	"bytes"
	"context"
	"encoding/json"

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
			lock: func(req rpc.Request) engine.LockMode {
				if req.DryRun {
					return engine.LockShared
				}
				return engine.LockExclusive
			},
			cluster: true,
			confirm: restartToken,
			runLog:  func(req rpc.Request) bool { return !req.DryRun },
			handler: handleApply,
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
			lock: func(req rpc.Request) engine.LockMode {
				if rotateRequested(req.Params) {
					return engine.LockExclusive
				}
				return engine.LockShared
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
			name:    "tune.apply",
			lock:    fixedLock(engine.LockExclusive),
			cluster: true,
			confirm: restartToken,
			runLog:  always,
			handler: handleTuneApply,
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

// restartToken is {cluster}/restart.
func restartToken(cfg config.Cluster, _ json.RawMessage) string {
	return cfg.Name() + "/restart"
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
	host, _ := hostinfo.Detect(env.Cluster.Server.DataDirectory)
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
	data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, true), engine.Options{Only: p.Steps})
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
		data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, true), engine.Options{Only: p.Steps})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: data}, nil
	}
	data, warnings, err := engine.Apply(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, false), engine.Options{
		Only: p.Steps,
		// A non-empty confirm was already checked against restartToken.
		Confirmed: env.Req.Confirm != "",
		Restarter: steps.Restarter(),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}

func handleSelfcheck(context.Context, *Env) (Result, error) {
	data, warnings, err := SelfCheck()
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}
