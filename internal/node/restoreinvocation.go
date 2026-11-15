package node

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

// External effects used during preparation. The record transaction and lock
// implementation remain the production ones; tests replace only host I/O.
var (
	restoreCheckTarget    = checkRestoreTarget
	restoreCheckSpace     = restoreSpace
	restoreCheckNamespace = restoringInTheWay
	restoreWriteMetrics   = steps.WriteMetrics
	restoreApplyPoolers   = applyPoolers
	restoreLaunch         = runJobRPC
	recoverCheckFence     = requireNotFenced
	recoverCheckBackup    = stanzaHasBackup
	recoverCheckWritten   = stanzaStillWritten
	restorePublish        = backup.UpdateRecords
)

// withRestorePreparation excludes both other preparations and running jobs.
// Its caller must launch the job only after this function has returned.
func withRestorePreparation(ctx context.Context, env *Env, db, method string, prepare func() error) error {
	release, err := nodeLockers.node(ctx, engine.LockShared, method, env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return err
	}
	defer release()
	names := []string{db, backup.RestoringName(db)}
	sort.Strings(names)
	for _, name := range names {
		mode := engine.LockShared
		if name == backup.RestoringName(db) {
			mode = engine.LockExclusive
		}
		unlock, err := nodeLockers.cluster(ctx, name, mode, method, env.Req.RequestID, env.Req.LockTimeout())
		if err != nil {
			return err
		}
		defer unlock()
	}
	cfg, err := loadGuardCluster()
	if err != nil {
		return err
	}
	env.Cluster = cfg
	return prepare()
}

func restoreInvocationProvenance(env *Env) (string, string, error) {
	instance, err := currentFollowInstance()
	if err != nil {
		return "", "", err
	}
	operation := env.Req.OperationID
	if operation == "" {
		operation = env.Req.RequestID
	}
	if instance == "" || operation == "" || env.Req.RequestID == "" {
		return "", "", incompleteMaterialisation("restore invocation")
	}
	return instance, operation, nil
}

func matchesRestoreInvocation(r backup.Restore, env *jobEnv, db string) bool {
	operation := env.OperationID
	if operation == "" {
		operation = env.ID
	}
	return db != "" && env.ID != "" && env.installID != "" && operation != "" &&
		r.Database == db && r.ID == env.ID &&
		r.InstanceID == env.installID && r.OperationID == operation &&
		((env.Job == "recover" && r.Recover) || (env.Job == "restore" && !r.Recover))
}

func validateRestoreInvocation(env *jobEnv) error {
	if env.Job != "restore" && env.Job != "recover" {
		return nil
	}
	var p struct {
		Database string `json:"database"`
	}
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return err
	}
	records, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	r, ok := records.Restores[p.Database]
	if !ok || r.Phase != backup.PhaseRestoring || !matchesRestoreInvocation(r, env, p.Database) {
		return incompleteMaterialisation(backup.RestoringName(p.Database))
	}
	if env.Job == "restore" {
		var params restoreParams
		if json.Unmarshal(env.Params, &params) != nil || params.Kind != r.Kind || params.Promote != r.Promote {
			return incompleteMaterialisation(backup.RestoringName(p.Database))
		}
	} else {
		var params recoverParams
		if json.Unmarshal(env.Params, &params) != nil || params.restore().Kind != r.Kind ||
			params.NoPromote == r.Promote || params.NoFull != r.NoFull {
			return incompleteMaterialisation(backup.RestoringName(p.Database))
		}
	}
	return nil
}
