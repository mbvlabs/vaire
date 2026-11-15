package node

import (
	"context"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/state"
)

// A fork prepares its ownership record before cluster.toml is saved and before
// the job allocates a temporary port or creates a directory. A fork that fails
// before that point leaves a prepared record, a configured target and no
// artifacts. The ordinary failed-fork cleanup requires a retained recovery
// failure and a recorded reservation, so it cannot retire this legacy intent
// and db drop and doctor stay blocked.
//
// This branch is deliberately distinct from db drop's deletion path. It
// removes only the entry the operator confirms and the exact record, after
// proving that both artifact paths are absent and that the allocator holds no
// reservation for the target. It never deletes a cluster, an artifact, a
// follower slot, a stanza, a backup record or a route.
var (
	forkAbsenceState       = func() (engine.State, error) { return engine.LoadState(engine.StatePath) }
	forkAbsenceReservation = backup.LockTemporaryReservationAbsence
	// forkAbsenceBeforePin lets tests substitute the record between the
	// eligibility read and the pinned reopen. Production is a no-op.
	forkAbsenceBeforePin = func() error { return nil }
)

// legacyPreparedFork recognises a strictly never-started prepared fork: no
// failure reason, no artifact stage or identity, no temporary port and no
// reservation. Any other record is left to the existing cleanup paths.
func legacyPreparedFork(o materialisationOwnership) bool {
	return o.Kind == materialisationFork &&
		o.Phase == materialisationPrepared &&
		o.Reason == "" &&
		o.ArtifactStage == "" &&
		o.TemporaryPort == 0 &&
		o.DataIdentity == (materialisationFileID{}) &&
		o.ConfigIdentity == (materialisationFileID{}) &&
		o.ReservationIdentity == (backup.ReservationIdentity{})
}

// forkAbsenceActiveJob reports a job that still owns the target. Job-state
// observations are not execution exclusion on their own, but a non-final
// record means the intent is not settled.
func forkAbsenceActiveJob(st engine.State, name string) bool {
	for _, rec := range st.Jobs {
		if rec.DB == name && !rec.Final() {
			return true
		}
	}
	for _, rec := range st.RetainedJobs {
		if rec.DB == name && !rec.Final() {
			return true
		}
	}
	return false
}

// forkAbsenceActiveLease reports an active runner lease that holds the target
// exclusively. db drop already holds the destructive node lock; this refuses
// the in-process case a lease was left registered.
func forkAbsenceActiveLease(name string) bool {
	lockedJobs.Lock()
	defer lockedJobs.Unlock()
	for lease := range lockedJobs.active {
		if lease.target == name || lease.targets[name] == engine.LockExclusive {
			return true
		}
	}
	return false
}

// checkForkAbsenceJob requires the record's exact owning job to be a terminal
// db-fork of the target. A missing, ambiguous, foreign or unfinished job, or
// any other active job or lease on the target, is a refusal.
func checkForkAbsenceJob(name string, o materialisationOwnership) error {
	st, err := forkAbsenceState()
	if err != nil || forkAbsenceActiveJob(st, name) || forkAbsenceActiveLease(name) {
		return materialisationIncomplete(name, &o)
	}
	job, err := materialisationJob(st, o.Job)
	if err != nil || job.ID != o.Job || job.Job != "db-fork" || job.DB != name || !job.Final() {
		return materialisationIncomplete(name, &o)
	}
	return nil
}

// checkPreparedForkRequest applies the request-level protections db drop
// already enforces for every database. They run again here because this
// branch deliberately bypasses the ordinary deletion machinery.
func checkPreparedForkRequest(env *Env, cfg config.Cluster, name string, o materialisationOwnership) error {
	if env.Req.Confirm != name {
		return confirmRequired("dropping "+name+" deletes all of its data", name)
	}
	if err := CheckNotProtected(cfg, name, "db drop"); err != nil {
		return err
	}
	if err := restoreInProgress(name, "db drop"); err != nil {
		return err
	}
	if err := preflightDropLeftovers(cfg, name); err != nil {
		return err
	}
	if err := refuseSourceDrop(cfg, name); err != nil {
		return err
	}
	if forkAbsenceActiveLease(name) {
		return materialisationIncomplete(name, &o)
	}
	return nil
}

// openPreparedForkAbsence pins the record and both parents, requires both
// target names absent, and proves the exact terminal owning job. Any other
// record, job or artifact is a refusal, never adoption.
func openPreparedForkAbsence(cfg config.Cluster, name string, o materialisationOwnership) (*materialisationArtifacts, error) {
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, materialisationIncomplete(name, &o)
	}
	if err := forkAbsenceBeforePin(); err != nil {
		return nil, materialisationIncomplete(name, &o)
	}
	a, err := openMaterialisationArtifacts(name, cfg.Server.DataRoot, uid)
	if err != nil {
		return nil, materialisationIncomplete(name, &o)
	}
	// The reopened record must be the exact eligibility record, still in the
	// strict never-started shape. A replacement that appeared before pinning
	// is refused rather than authorized by the original record's job proof.
	if a.owner != o || !legacyPreparedFork(a.owner) {
		pinned := a.owner
		a.close()
		return nil, materialisationIncomplete(name, &pinned)
	}
	if err := a.validate(); err != nil {
		a.close()
		return nil, materialisationIncomplete(name, &o)
	}
	if err := checkForkAbsenceJob(name, o); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

// savePreparedForkMetadata removes only name from a freshly loaded
// configuration. An entry already absent is a valid retry after a marker
// retirement failure, so nothing is saved again.
func savePreparedForkMetadata(store state.Store, name string) error {
	fresh, _, err := store.Load()
	if err != nil {
		return err
	}
	if err := CheckNotProtected(fresh, name, "db drop"); err != nil {
		return err
	}
	if err := refuseSourceDrop(fresh, name); err != nil {
		return err
	}
	if _, ok := fresh.ClusterByName(name); !ok {
		return nil
	}
	_, err = saveDroppedConfig(store, fresh, name)
	return err
}

// dropPreparedForkMetadata retires a legacy prepared fork through db drop's
// explicit confirmation. held is false when the name has no such record, so
// the ordinary drop path still handles every other database. When held is
// true, the ordinary path is never reached: the result is either the metadata
// cleanup or its refusal.
func dropPreparedForkMetadata(ctx context.Context, env *Env, store state.Store, cfg config.Cluster, name string) (Result, bool, error) {
	o, found, err := readMaterialisation(name, cfg.Server.DataRoot)
	if err != nil || !found || !legacyPreparedFork(o) {
		return Result{}, false, nil
	}
	if err := checkPreparedForkRequest(env, cfg, name, o); err != nil {
		return Result{}, true, err
	}
	refuse := func() (Result, bool, error) {
		return Result{}, true, materialisationIncomplete(name, &o)
	}
	a, err := openPreparedForkAbsence(cfg, name, o)
	if err != nil {
		return Result{}, true, err
	}
	defer a.close()
	// Absence is re-proved and then held by the allocator for the whole
	// save-and-retire transaction. A zero-port LockForkReservation would
	// require a reservation rather than its absence and is the wrong proof.
	unlock, err := forkAbsenceReservation(ctx, name)
	if err != nil || unlock == nil {
		return refuse()
	}
	defer unlock()
	if err := a.validate(); err != nil {
		return refuse()
	}
	if err := checkForkAbsenceJob(name, o); err != nil {
		return Result{}, true, err
	}
	// Save first, from the freshly loaded configuration, removing only the
	// confirmed name and preserving every unrelated entry.
	if err := savePreparedForkMetadata(store, name); err != nil {
		return Result{}, true, err
	}
	// Revalidate the pinned record, the job proof, the paths and the fresh
	// configuration immediately before retiring the marker.
	after, _, err := store.Load()
	if err != nil {
		return Result{}, true, err
	}
	if _, ok := after.ClusterByName(name); ok {
		return refuse()
	}
	if err := a.validate(); err != nil {
		return refuse()
	}
	if err := checkForkAbsenceJob(name, o); err != nil {
		return Result{}, true, err
	}
	if err := a.removeMaterialisationRecord(); err != nil {
		return Result{}, true, err
	}
	env.Cluster = after
	return Result{
		Data: map[string]any{"dropped": name, "metadata_only": true},
	}, true, nil
}
