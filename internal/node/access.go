package node

import (
	"context"
	"errors"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

var accessMaterialisationState = func(cfg config.Cluster, name string) (bool, error) {
	_, found, err := readMaterialisation(name, cfg.Server.DataRoot)
	return found, err
}

// unfollowAccessNotice is read-only. Promoting a follower does not authorize
// widening its inherited ACLs; failed or unavailable measurement is unknown.
func unfollowAccessNotice(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) *output.Warning {
	status := "unknown"
	incomplete, err := accessMaterialisationState(cfg, spec.Name)
	if err == nil && !incomplete {
		if coverage, err := accessInspect(ctx, spec); err == nil {
			switch coverage.Status {
			case "installed", "pending":
				status = coverage.Status
			}
		}
	}
	if status == "installed" {
		return nil
	}
	return &output.Warning{
		Code: "access_coverage_" + status,
		Message: "automatic access coverage for " + spec.Name + " is " + status +
			"; before migrations, inspect vaire db access show " + spec.Name +
			" and preview vaire plan --db " + spec.Name +
			"; if expansion is needed, confirm with vaire apply --db " + spec.Name + " --confirm " + spec.Name + "/access",
	}
}

var accessLoadRecords = backup.LoadRecords

var accessInspect = func(ctx context.Context, spec config.ClusterSpec) (steps.AccessCoverage, error) {
	local := steps.ClusterPG(spec)
	name := spec.DatabaseName()
	return steps.InspectAccess(ctx, name, func(ctx context.Context, sql string) (string, error) {
		rows, err := local.Query(ctx, name, sql, nil)
		if err != nil {
			return "", err
		}
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0]["coverage"] == "" {
			return "", errors.New("access inspection did not return one coverage row")
		}
		return rows[0]["coverage"], nil
	})
}

type databaseAccess struct {
	Database string               `json:"database"`
	Desired  string               `json:"desired"`
	State    string               `json:"state,omitempty"`
	Source   string               `json:"source,omitempty"`
	Owner    string               `json:"owner"`
	App      string               `json:"app"`
	RO       string               `json:"ro"`
	Coverage steps.AccessCoverage `json:"coverage"`
	Notice   string               `json:"notice"`
}

func handleDBAccessShow(ctx context.Context, env *Env) (Result, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	spec, ok := env.Cluster.ClusterByName(p.Name)
	if !ok {
		return Result{}, databaseNotFound(p.Name)
	}
	name := spec.DatabaseName()
	data := databaseAccess{
		Database: spec.Name, Desired: "automatic",
		Owner: name + "_owner", App: name + "_app", RO: name + "_ro",
		Coverage: steps.AccessCoverage{Status: "unknown"},
		Notice:   "Global owner defaults have no private-schema exclusions; existing grants cover only eligible owner-owned schemas and objects. Other creators are not covered.",
	}
	if spec.IsFollower() {
		data.Source = spec.Follows
	}
	incomplete, err := accessMaterialisationState(env.Cluster, spec.Name)
	if err != nil {
		data.State = "materialisation_state_unknown"
		return Result{Data: data}, nil
	}
	if incomplete {
		data.State = "materialisation_incomplete"
		return Result{Data: data}, nil
	}
	records, err := accessLoadRecords()
	if err != nil {
		data.State = "recovery_state_unknown"
		return Result{Data: data}, nil
	}
	if records.IsAwaiting(spec.Name) {
		data.State = "awaiting_recovery"
		return Result{Data: data}, nil
	}
	if spec.IsFollower() && spec.FollowGeneration != "" {
		data.State = "follow_incomplete"
		return Result{Data: data}, nil
	}
	coverage, err := accessInspect(ctx, spec)
	if err == nil {
		data.Coverage = coverage
	}
	return Result{Data: data}, nil
}
