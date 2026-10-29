package node

import (
	"context"
	"slices"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/health"
	"github.com/mbvlabs/vaire/internal/promfile"
	"github.com/mbvlabs/vaire/internal/steps"
)

// followerReading is one follower's replication (Phase 6, D13).
type followerReading = health.FollowerState

// FollowersProm is vaire-followers.prom, which metrics-refresh writes.
const FollowersProm = "vaire-followers.prom"

// followerPG is steps.ClusterPG, a seam for tests that make one query fail.
var followerPG = func(spec config.ClusterSpec) sqlRunner { return steps.ClusterPG(spec) }

// readFollower reads spec's replication from its source src and from
// itself; a side that isn't running or doesn't answer is left out, and its
// error is kept.
func readFollower(ctx context.Context, spec, src config.ClusterSpec) followerReading {
	slot := config.FollowerSlot(spec.Name)
	var srcRow, selfRow map[string]string
	var srcErr, selfErr string
	if clusterRunning(src) {
		qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		rows, err := followerPG(src).Query(qctx, "postgres", health.FollowerSourceSQL(spec.Name, slot), nil)
		cancel()
		switch {
		case err != nil:
			srcErr = src.Name + ": " + err.Error()
		case len(rows) > 0:
			srcRow = rows[0]
		default:
			srcErr = src.Name + ": no replication row"
		}
	} else {
		srcErr = src.Name + " is not running"
	}
	if clusterRunning(spec) {
		qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		rows, err := followerPG(spec).Query(qctx, "postgres", health.FollowerSelfSQL, nil)
		cancel()
		switch {
		case err != nil:
			selfErr = spec.Name + ": " + err.Error()
		case len(rows) > 0:
			selfRow = rows[0]
		default:
			selfErr = spec.Name + ": no recovery row"
		}
	} else {
		selfErr = spec.Name + " is not running"
	}
	st := health.ParseFollower(src.Name, slot, srcRow, selfRow)
	st.SourceError, st.SelfError = srcErr, selfErr
	return st
}

// readFollowers reads every follower of cfg (only db when set), by name.
func readFollowers(ctx context.Context, cfg config.Cluster, db string) map[string]followerReading {
	out := map[string]followerReading{}
	for _, spec := range cfg.Clusters {
		if !spec.IsFollower() || (db != "" && spec.Name != db) {
			continue
		}
		src, ok := cfg.ClusterByName(spec.Follows)
		if !ok {
			continue
		}
		out[spec.Name] = readFollower(ctx, spec, src)
	}
	return out
}

// orphanSlot is an vaire_* slot on a source that no follower owns.
type orphanSlot struct {
	Source string `json:"source"`
	Port   int    `json:"port"`
	Slot   string `json:"slot"`
}

// slotQueryError is a source whose slot enumeration failed, so doctor cannot
// say whether it has slots no follower owns.
type slotQueryError struct {
	Source string `json:"source"`
	Port   int    `json:"port"`
	Error  string `json:"error"`
}

// orphanSlots lists the vaire_* slots on cfg's running clusters that no
// follower in cluster.toml owns (D4), and reports every source whose slots
// could not be listed.
func orphanSlots(ctx context.Context, cfg config.Cluster) ([]orphanSlot, []slotQueryError) {
	var out []orphanSlot
	var failures []slotQueryError
	for _, spec := range cfg.Clusters {
		if spec.IsFollower() || !clusterRunning(spec) {
			continue
		}
		qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		rows, err := followerPG(spec).Query(qctx, "postgres", health.SlotsSQL, nil)
		cancel()
		if err != nil {
			failures = append(failures, slotQueryError{Source: spec.Name, Port: spec.Port, Error: err.Error()})
			continue
		}
		owned := ownedSlots(cfg, spec.Name)
		for _, r := range rows {
			if !slices.Contains(owned, r["slot_name"]) {
				out = append(out, orphanSlot{Source: spec.Name, Port: spec.Port, Slot: r["slot_name"]})
			}
		}
	}
	return out, failures
}

// ownedSlots are the slots db's followers own (an entry awaiting recovery
// included: db recover makes its slot again).
func ownedSlots(cfg config.Cluster, db string) []string {
	var out []string
	for _, f := range slices.Concat(cfg.Clusters, cfg.Awaiting) {
		if f.Follows == db {
			out = append(out, config.FollowerSlot(f.Name))
		}
	}
	return out
}

// followerFamilies are vaire-followers.prom's families (D13). A sample is
// written only for a value that was measured: unknown lag, retention or slot
// state is omitted, never published as zero.
func followerFamilies(readings map[string]followerReading) []*promfile.Family {
	streaming := promfile.Gauge("vaire_follower_streaming", "Whether each follower streams from its source.")
	lagBytes := promfile.Gauge("vaire_follower_lag_bytes", "WAL each follower's source wrote that it hasn't replayed.")
	lagSeconds := promfile.Gauge("vaire_follower_lag_seconds", "PostgreSQL's replay delay for each follower.")
	retained := promfile.Gauge("vaire_follower_slot_retained_bytes", "WAL each follower's slot keeps on its source.")
	lost := promfile.Gauge("vaire_follower_slot_lost", "Whether each follower's slot on its source is lost or missing.")
	for _, name := range promfile.SortedKeys(readings) {
		r := readings[name]
		labels := promfile.L("db", name, "source", r.Source)
		if r.SourceKnown {
			streaming.Add(boolFloat(r.Streaming), labels...)
			lost.Add(boolFloat(!r.SlotExists || r.SlotWALStatus == "lost"), labels...)
		}
		if r.LagBytes != nil {
			lagBytes.Add(float64(*r.LagBytes), labels...)
		}
		if r.LagSeconds != nil {
			lagSeconds.Add(*r.LagSeconds, labels...)
		}
		if r.SlotExists && r.SlotRetainedBytes != nil {
			retained.Add(float64(*r.SlotRetainedBytes), labels...)
		}
	}
	return []*promfile.Family{streaming, lagBytes, lagSeconds, retained, lost}
}

// writeFollowerProm is promfile.Write, a seam for a failed follower write.
var writeFollowerProm = promfile.Write

// writeFollowerMetrics rewrites vaire-followers.prom (empty families without
// followers). Its write error is returned: a stale follower file must fail
// refresh, not keep publishing healthy samples.
func writeFollowerMetrics(ctx context.Context, cfg config.Cluster) error {
	return writeFollowerProm(FollowersProm, followerFamilies(readFollowers(ctx, stepsView(cfg), "")))
}
