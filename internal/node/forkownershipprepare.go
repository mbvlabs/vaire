package node

import (
	"path/filepath"

	"github.com/mbvlabs/vaire/internal/config"
)

// forkPrepareOwnership records intent only; it grants no authority over the
// target. The caller holds the node preparation lock until config is saved.
var forkPrepareOwnership = prepareForkOwnership

func prepareForkOwnership(env *Env, cfg config.Cluster, source, target config.ClusterSpec) error {
	instance, err := currentFollowInstance()
	if err != nil {
		return err
	}
	operation := env.Req.OperationID
	if operation == "" {
		operation = env.Req.RequestID
	}
	o := materialisationOwnership{
		Operation: operation, Job: env.Req.RequestID, Instance: instance,
		Kind: materialisationFork, Source: source.Name, Target: target.Name,
		Stanza: source.Stanza, IntendedPort: target.Port,
		DataPath:       filepath.Join(cfg.Server.DataRoot, target.Name),
		RegisteredPath: filepath.Join(materialisationConfigRoot, target.Name),
		Phase:          materialisationPrepared,
	}
	_, err = createMaterialisation(o, cfg.Server.DataRoot)
	return err
}
