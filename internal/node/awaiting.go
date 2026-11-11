package node

import (
	"encoding/json"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

// Databases awaiting recovery (Phase 5, D9). After server init --restore
// every database in cluster.toml is recorded in records.json as awaiting
// recovery: the server has no cluster for it until db recover materialises
// one from its stanza. Steps never see such a database, and the commands
// that need its cluster refuse.

func init() {
	engine.ClusterView = engineView
}

// stepsView is cluster.toml without the databases that have no cluster yet,
// for the step engine only. engineView carries the one database an owned
// lifecycle operation is building (db recover, or an ordinary follow that has
// just materialised). A records file that can't be read changes nothing about
// an unfinished ordinary follow: its generation is in cluster.toml and still
// leaves it out.
func stepsView(cfg config.Cluster) config.Cluster { return engineView(cfg, "") }

func engineView(cfg config.Cluster, keep string) config.Cluster {
	recs, err := backup.LoadRecords()
	if err != nil {
		// The awaiting entries are unknown, so they cannot narrow the view;
		// an unfinished follow's generation still does.
		recs = backup.Records{}
	}
	return withoutUnfinished(cfg, recs, keep)
}

// withoutUnfinished is the steps' view: a database awaiting recovery or an
// unfinished ordinary follow has no cluster of its own yet, so no step may
// create, start, expose or time it. Both are counted in Awaiting for the
// resource math. keep is the database an owned follow/recovery execution is
// deliberately building; the saved configuration is never narrowed.
func withoutUnfinished(cfg config.Cluster, recs backup.Records, keep string) config.Cluster {
	return cfg.RoutingView(recs.IsAwaiting, keep)
}

// diagnosticsView is the read-only status, doctor and metrics view. A database
// awaiting recovery still has no cluster, but a configured follower whose
// ordinary follow has not completed is deliberately kept whatever its
// follow_generation: if it materialised before the completion save failed, the
// source's slot pressure and the follower's paused or lost state remain
// measurable (Q3). This view must never select steps; the convergence view
// above is the only one the engine may route, create, start and time from.
func diagnosticsView(cfg config.Cluster) config.Cluster {
	recs, err := backup.LoadRecords()
	if err != nil {
		// The awaiting entries are unknown, so they cannot narrow the view.
		return cfg.Clone()
	}
	return diagnosticsViewWith(cfg, recs)
}

// diagnosticsViewWith is diagnosticsView from an already-loaded records
// snapshot, so status and doctor do not read the file twice while building the
// same report.
func diagnosticsViewWith(cfg config.Cluster, recs backup.Records) config.Cluster {
	out := cfg.Clone()
	out.Clusters, out.Awaiting = nil, nil
	for _, c := range cfg.Clusters {
		c = c.Clone()
		if recs.IsAwaiting(c.Name) {
			out.Awaiting = append(out.Awaiting, c)
		} else {
			out.Clusters = append(out.Clusters, c)
		}
	}
	return out
}

// awaitingError refuses action on db while it awaits recovery.
func awaitingError(db, action string) error {
	return output.NewError(output.CodePrecondition,
		"database "+db+" awaits recovery after the master was rebuilt, so "+action+" is refused", output.ExitPrecondition,
		"Recover it first: vaire db recover "+db).
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
	"db.follow": "a follow", "db.unfollow": "an unfollow",
	"ext.add": "an extension change", "ext.remove": "an extension change",
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
