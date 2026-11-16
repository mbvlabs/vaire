package node

import (
	"context"
	"encoding/json"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

// The runner adapter will replace this predicate only when it can issue a
// lock-bound capability. Public names and Recovering are never authority.
var materialisationOwner = forkMaterialisationOwner

func materialisationIncomplete(db string, o *materialisationOwnership) error {
	details := map[string]any{"reason": "materialisation_incomplete"}
	if config.CheckDatabaseName(db) == nil {
		details["database"] = db
	}
	if o != nil {
		if validJobID(o.Job) {
			details["owning_job"] = o.Job
		}
		if o.Reason != "" {
			details["failure_reason"] = o.Reason
		}
	}
	return output.NewError(output.CodePrecondition, "incomplete materialisation prevents this operation",
		output.ExitPrecondition, "Inspect the incomplete database before continuing.").WithDetails(details)
}

func guardPublicMaterialisation(ctx context.Context, cfg config.Cluster, db string) error {
	return guardMaterialisation(ctx, cfg, db, false)
}

func guardedMaterialisations(dataRoot string) ([]materialisationOwnership, error) {
	ancestors, err := materialisationAncestry(materialisationStateDir, materialisationRecordUID, false)
	if err != nil {
		return nil, err
	}
	records, err := listMaterialisations(dataRoot)
	if err != nil {
		return nil, err
	}
	if err := validateMaterialisationAncestry(ancestors); err != nil {
		return nil, err
	}
	return records, nil
}

// guardMaterialisation is also the integration hook for database inspection
// and job entry points. It never returns record paths or raw read errors.
func guardMaterialisation(ctx context.Context, cfg config.Cluster, db string, allowOwner bool) error {
	o, found, err := readMaterialisation(db, cfg.Server.DataRoot)
	if err != nil {
		return materialisationIncomplete(db, nil)
	}
	if found && (!allowOwner || !materialisationOwner(ctx, o)) {
		return materialisationIncomplete(db, &o)
	}
	return nil
}

func guardMaterialisationApply(ctx context.Context, cfg config.Cluster, db string) error {
	if db != "" {
		o, found, err := readMaterialisation(db, cfg.Server.DataRoot)
		if err != nil {
			return materialisationIncomplete(db, nil)
		}
		if found {
			// A verified public continuation lease may admit the exact target.
			// The lease is issued only by the dispatcher after it acquired the
			// global and target locks, and openForkContinuation re-proves every
			// fact before any effect.
			if materialisationContinuationLeaseMatches(ctx, o) {
				return nil
			}
			return materialisationIncomplete(db, &o)
		}
		return nil
	}
	records, err := guardedMaterialisations(cfg.Server.DataRoot)
	if err != nil {
		return materialisationIncomplete("", nil)
	}
	if len(records) != 0 {
		return materialisationIncomplete(records[0].Target, &records[0])
	}
	return nil
}

func materialisationGuardView(ctx context.Context, cfg config.Cluster, opts engine.Options) (config.Cluster, error) {
	records, err := guardedMaterialisations(cfg.Server.DataRoot)
	if err != nil {
		return config.Cluster{}, materialisationIncomplete("", nil)
	}
	excluded := make(map[string]bool, len(records))
	for _, o := range records {
		owner := materialisationOwner(ctx, o)
		// A public continuation keeps only its exact per-database target in
		// view. Shared renders (DB empty) and other databases still exclude it,
		// so its routes and timers stay withheld.
		if opts.DB != "" && opts.DB == o.Target && materialisationContinuationOwner(ctx, o) {
			owner = true
		}
		if opts.DB == "" || opts.DB != o.Target || !owner {
			excluded[o.Target] = true
		}
	}
	// RoutingView accounts for newly excluded entries but reconstructs
	// Awaiting; preserve entries excluded by the preceding legacy view.
	awaiting := cfg.Awaiting
	keep := opts.Recovering
	if excluded[keep] {
		keep = ""
	}
	view := cfg.RoutingView(func(name string) bool { return excluded[name] }, keep)
	view.Awaiting = append(view.Awaiting, awaiting...)
	return view, nil
}

var materialisationRefused = map[string]bool{
	"db.create": true, "db.copy": true, "db.resize": true, "db.recover": true,
	"db.credentials": true, "db.write-access": true, "db.fork": true,
	"db.follow": true, "db.unfollow": true, "backup.now": true, "backup.verify": true,
	"restore.start": true, "restore.promote": true, "restore.undo": true,
	"restore.drill": true, "dump.create": true, "maint.run": true,
	"maint.kill": true, "queries.reset": true, "sql": true,
}

func refuseMaterialisationRequest(ctx context.Context, cfg config.Cluster, req rpc.Request) error {
	if req.Method != "apply" && !materialisationRefused[req.Method] {
		return nil
	}
	var params map[string]any
	if len(req.Params) != 0 && json.Unmarshal(req.Params, &params) != nil {
		return nil
	}
	if req.Method == "apply" {
		db, _ := params["db"].(string)
		// The dispatcher reloads this guard before it can acquire locks, so an
		// admission candidate must pass the read-only classifier here. The
		// handler still requires the dispatcher's lock-bound lease and re-proves
		// the complete continuation under those locks.
		if !req.DryRun && db != "" && !requestSelectsSteps(params) && forkContinuationCandidate(cfg, db) {
			return nil
		}
		return guardMaterialisationApply(ctx, cfg, db)
	}
	for _, field := range awaitingParamFields {
		if name, _ := params[field].(string); name != "" {
			if err := guardPublicMaterialisation(ctx, cfg, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func requestSelectsSteps(params map[string]any) bool {
	raw, _ := params["steps"].([]any)
	for _, step := range raw {
		if name, _ := step.(string); name != "" {
			return true
		}
	}
	return false
}
