package node

import (
	"encoding/json"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/rpc"
)

// Databases awaiting recovery (Phase 5, D9). After server init --restore
// every database in cluster.toml is recorded in records.json as awaiting
// recovery: the server has no cluster for it until db recover materialises
// one from its stanza. Steps never see such a database, and the commands
// that need its cluster refuse.

func init() {
	engine.ClusterView = engineView
}

// stepsView is cluster.toml without the databases awaiting recovery, for the
// step engine only; keep is one db recover is building. The left-out
// databases go to Awaiting, which the tuning counts. A records file that
// can't be read changes nothing.
func stepsView(cfg config.Cluster) config.Cluster { return engineView(cfg, "") }

func engineView(cfg config.Cluster, keep string) config.Cluster {
	recs, err := backup.LoadRecords()
	if err != nil || len(recs.AwaitingRecovery) == 0 {
		return cfg
	}
	return withoutAwaiting(cfg, recs, keep)
}

func withoutAwaiting(cfg config.Cluster, recs backup.Records, keep string) config.Cluster {
	out := cfg
	out.Clusters, out.Awaiting = nil, nil
	for _, c := range cfg.Clusters {
		if c.Name == keep || !recs.IsAwaiting(c.Name) {
			out.Clusters = append(out.Clusters, c)
		} else {
			out.Awaiting = append(out.Awaiting, c)
		}
	}
	return out
}

// awaitingError refuses action on db while it awaits recovery.
func awaitingError(db, action string) error {
	return output.NewError(output.CodePrecondition,
		"database "+db+" awaits recovery after the master was rebuilt, so "+action+" is refused", output.ExitPrecondition,
		"Recover it first: eru db recover "+db).
		WithDetails(map[string]any{"reason": "awaiting_recovery", "database": db})
}

// awaitingRefused are the methods that need a database's cluster, with the
// action named in the refusal. db.drop, config.set and db.recover are not
// among them (D9).
var awaitingRefused = map[string]string{ //nolint:gosec // G101: method names and actions, not credentials.
	"restore.start": "a restore", "restore.promote": "restore promote", "restore.undo": "restore undo",
	"restore.drill": "a restore drill", "db.fork": "a fork", "dump.create": "a dump",
	"backup.now": "a backup", "backup.verify": "a verify", "db.resize": "a resize",
	"db.credentials": "rotating its credentials", "maint.run": "maintenance", "maint.kill": "maint kill",
	"maint.report": "a maintenance report", "queries.reset": "a statistics reset", "apply": "apply --db",
	"sql": "sql", "db.write-access": "writing to it",
}

// awaitingParamFields are the params that name a database.
var awaitingParamFields = []string{"db", "database", "name", "source"}

// refuseAwaiting refuses req when its method needs a cluster and a database
// it names awaits recovery.
func refuseAwaiting(req rpc.Request) error {
	action, ok := awaitingRefused[req.Method]
	if !ok {
		return nil
	}
	var params map[string]any
	if json.Unmarshal(req.Params, &params) != nil {
		return nil
	}
	recs, err := backup.LoadRecords()
	if err != nil || len(recs.AwaitingRecovery) == 0 {
		return nil
	}
	return refuseAwaitingIn(params, recs, action)
}

func refuseAwaitingIn(params map[string]any, recs backup.Records, action string) error {
	for _, f := range awaitingParamFields {
		if name, _ := params[f].(string); name != "" && recs.IsAwaiting(name) {
			return awaitingError(name, action)
		}
	}
	return nil
}
