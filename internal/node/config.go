package node

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/state"
	"github.com/mbvlabs/eru/internal/steps"
)

type configSetParams struct {
	TOML         string            `json:"toml"`
	Set          map[string]string `json:"set"`
	BaseRevision string            `json:"base_revision"`
}

type configValidateParams struct {
	TOML string `json:"toml"`
}

func handleConfigGet(context.Context, *Env) (Result, error) {
	store := state.FileStore{}
	raw, err := os.ReadFile(state.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, output.NewError(output.CodeClusterNotInitialized, "this node has no cluster.toml", output.ExitContext, "Run eru server init first.")
		}
		return Result{}, err
	}
	revision, err := store.Revision()
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]string{"toml": string(raw), "revision": revision}}, nil
}

func handleConfigSet(ctx context.Context, env *Env) (Result, error) {
	var p configSetParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	store := state.FileStore{}
	current, err := store.Revision()
	if err != nil {
		return Result{}, err
	}
	if p.BaseRevision != current {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"cluster.toml changed since it was read",
			output.ExitPrecondition,
			"Re-read the configuration and retry.",
		).WithDetails(map[string]any{"reason": "config_changed", "base_revision": p.BaseRevision, "revision": current})
	}
	raw, err := newConfigBytes(store, p)
	if err != nil {
		return Result{}, err
	}
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return Result{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, config.ValidationError(err)
	}
	if err := checkSchedules(ctx, cfg); err != nil {
		return Result{}, err
	}
	if old, _, err := store.Load(); err == nil {
		if err := checkRepoMove(ctx, old, cfg); err != nil {
			return Result{}, err
		}
	}
	// A dry run previews the plan for the new file without saving it.
	revision := current
	if !env.Req.DryRun {
		if err := store.Save(raw); err != nil {
			return Result{}, err
		}
		if err := syncNodeHostname(cfg); err != nil {
			return Result{}, err
		}
		if revision, err = store.Revision(); err != nil {
			return Result{}, err
		}
	}
	env.Cluster = cfg
	plan, planErr := engine.Plan(ctx, steps.Catalog(), cfg, runtimeFor(env, true), engine.Options{})
	data := map[string]any{"revision": revision, "plan": plan, "saved": !env.Req.DryRun}
	var warnings []output.Warning
	if planErr != nil {
		warnings = append(warnings, output.Warning{Code: "plan_unavailable", Message: "the change was saved but the plan preview could not be computed: " + planErr.Error()})
	}
	return Result{Data: data, Warnings: warnings}, nil
}

// newConfigBytes returns the desired-state bytes for a config.set request,
// either the supplied TOML or the current file with the dotted keys applied.
func newConfigBytes(store state.FileStore, p configSetParams) ([]byte, error) {
	if p.TOML != "" && len(p.Set) > 0 {
		return nil, output.NewError(output.CodeUsage, "pass either toml or set, not both", output.ExitUsage, "")
	}
	cfg, _, err := store.Load()
	if err != nil {
		return nil, err
	}
	if p.TOML != "" {
		updated, err := config.ParseCluster([]byte(p.TOML))
		if err != nil {
			return nil, output.NewError(output.CodeConfig, "the supplied cluster.toml does not parse: "+err.Error(), output.ExitConfig, "")
		}
		if changed := config.ImmutableChanges(cfg, updated); len(changed) > 0 {
			return nil, output.NewError(
				output.CodePrecondition,
				"the new cluster.toml changes keys that are immutable after init: "+strings.Join(changed, ", "),
				output.ExitPrecondition,
				config.ImmutableHint(),
			).WithDetails(map[string]any{"reason": "immutable_key", "keys": changed})
		}
		return []byte(p.TOML), nil
	}
	for key, value := range p.Set {
		if err := cfg.Set(key, value); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, config.ValidationError(err)
	}
	return cfg.Encode()
}

func handleConfigValidate(_ context.Context, env *Env) (Result, error) {
	var p configValidateParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	raw := []byte(p.TOML)
	if p.TOML == "" {
		file, err := os.ReadFile(state.Path)
		if err != nil {
			return Result{}, output.NewError(output.CodeClusterNotInitialized, "this node has no cluster.toml", output.ExitContext, "Run eru server init first.")
		}
		raw = file
	}
	problems := validateProblems(raw)
	return Result{Data: map[string]any{"valid": len(problems) == 0, "problems": problems}}, nil
}

// validateProblems returns the parse and validation problems in raw.
func validateProblems(raw []byte) []string {
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return []string{err.Error()}
	}
	if err := cfg.Validate(); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// checkSchedules runs systemd-analyze calendar on every backup schedule, so
// a timer never fails to load: the node has the final say on OnCalendar.
func checkSchedules(ctx context.Context, cfg config.Cluster) error {
	var problems []string
	for key, value := range cfg.ScheduleValues() {
		if _, err := steps.Probe(ctx, "systemd-analyze", "calendar", value); err != nil {
			problems = append(problems, key+" "+strconv.Quote(value)+" is not a valid OnCalendar expression")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return output.NewError(output.CodeValidation, strings.Join(problems, "; "), output.ExitConfig,
		"Use systemd OnCalendar syntax, for example \"Sun *-*-* 01:00:00\" or \"daily\".").WithDetails(map[string]any{"problems": problems})
}

// checkRepoMove refuses to move backups to another endpoint, bucket or
// prefix once the stanza exists there: the restore window would be lost.
func checkRepoMove(ctx context.Context, old, updated config.Cluster) error {
	if !old.BackupConfigured() || !updated.BackupConfigured() {
		return nil
	}
	ob, nb := old.Backup, updated.Backup
	if ob.Endpoint == nb.Endpoint && ob.Bucket == nb.Bucket && ob.Prefix == nb.Prefix {
		return nil
	}
	info, err := driver(old, "config.set", nil).Info(ctx, "")
	if err != nil || (info.StatusCode != 0 && len(info.Sets) == 0) {
		return nil
	}
	return output.NewError(output.CodePrecondition,
		"backups already live in "+ob.Bucket+"/"+ob.Prefix+"; moving them would lose the restore window",
		output.ExitPrecondition,
		"Keep the endpoint, bucket and prefix. Re-running eru backup setup with the same bucket rotates the keys.").
		WithDetails(map[string]any{"reason": "repo_in_use", "bucket": ob.Bucket, "prefix": ob.Prefix})
}

// syncNodeHostname keeps node.toml's hostname in step with cluster.toml: it
// is <node>.<domain>, so it moves when the domain does.
func syncNodeHostname(cfg config.Cluster) error {
	n, err := config.LoadNode(config.NodePath)
	if err != nil {
		if config.IsNotExist(err) {
			return nil
		}
		return err
	}
	hostname := cfg.Hostname(n.NodeName)
	if n.Hostname == hostname {
		return nil
	}
	n.Hostname = hostname
	raw, err := config.EncodeNode(n)
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(config.NodePath, raw, 0o640)
}
