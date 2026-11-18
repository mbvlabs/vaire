package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

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
	// The marker was this job's last authority unless any fence still owns the
	// name. forkJobUnresolved also rechecks the marker; otherwise the exact
	// job record can be pruned with ordinary history.
	if recs, err := backup.LoadRecords(); err == nil {
		if _, fenced := recs.Activation[name]; !fenced {
			releaseResolvedMaterialisationJob(o.Job, "db-fork")
		}
	}

	env.Cluster = after

	return Result{
		Data: map[string]any{"dropped": name, "metadata_only": true},
	}, true, nil
}

// A fork's failed cleanup can have removed its data, configuration and
// materialisation record while its prepared activation fence survived: the
// fence is retired last, so a crash or an older cleanup leaves exactly that
// residue. Only an explicit db drop may retire it. The fence proves intent,
// never ownership of an artifact, so this branch deletes the fence and nothing
// else: no cluster, reservation, configuration, route, stanza or journal entry
// is touched. A fence whose owner is not the terminal db-fork job itself (an
// explicit operation-id override) has no established job linkage and is
// refused rather than correlated by activity records or timestamps.
var forkFenceUpdateRecords = backup.UpdateRecords

func fenceOnlyOwner(f backup.ActivationFence) materialisationOwnership {
	return materialisationOwnership{
		Operation: f.OperationID, Job: f.OperationID, Instance: f.Instance,
		Kind: materialisationFork, Target: f.Database, Phase: materialisationPrepared,
		IntendedPort: f.Port, DataPath: f.DataDir, RegisteredPath: f.ConfDir,
	}
}

// verifyFenceOnlyFork proves the fence is a never-started fork of this
// installation: prepared, with its complete immutable source-to-target stanza
// transition, an ordinary cluster port, the exact live deterministic paths and
// an empty follow ownership. A follower's ownership record for the same name
// is conflicting evidence, not a predecessor to adopt.
func verifyFenceOnlyFork(cfg config.Cluster, name string, f backup.ActivationFence) error {
	refuse := func() error { return materialisationIncomplete(name, nil) }
	if f.Database != name || f.Kind != "fork" || f.Phase != fencePhasePrepared {
		return refuse()
	}

	if err := verifyFenceInstance(f, name); err != nil {
		return err
	}

	if f.TargetStanza == "" || f.NewStanza == "" || f.OldStanza == "" ||
		f.NewStanza != f.TargetStanza || f.OldStanza == f.NewStanza {
		return refuse()
	}

	if f.Port < config.ClusterPortMin || f.Port > config.ClusterPortMax ||
		f.Ownership != "" || f.Registration != fmt.Sprintf("%s:%d:", name, f.Port) ||
		f.Aside != "" ||
		f.DataDir != cfg.Server.DataDirectory(name) ||
		f.ConfDir != filepath.Join(materialisationConfigRoot, name) {
		return refuse()
	}

	p, err := followOwnershipPath(name)
	if err != nil {
		return refuse()
	}

	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		return refuse()
	}

	return nil
}

// checkFenceOnlyForkJob requires the fence owner to be exactly the terminal
// failed db-fork job record for the target. materialisationJob rejects a
// missing, duplicate or retained-conflicting history entry; an active job or
// lease on the target is a refusal. OperationID == JobID is the only durable
// fence-to-job linkage this residue carries.
func checkFenceOnlyForkJob(name string, f backup.ActivationFence) error {
	st, err := forkAbsenceState()
	if err != nil || forkAbsenceActiveJob(st, name) || forkAbsenceActiveLease(name) {
		return materialisationIncomplete(name, nil)
	}

	job, err := materialisationJob(st, f.OperationID)
	if err != nil || job.ID != f.OperationID || job.Job != "db-fork" ||
		job.DB != name || job.State != engine.JobFailed {
		return materialisationIncomplete(name, nil)
	}

	return nil
}

// checkFenceOnlyRecords refuses any other durable operation on the name.
// Historical records (timelines, dropped stanzas, unrelated databases) are
// left untouched by the caller.
func checkFenceOnlyRecords(s *backup.Records, name string) error {
	refuse := func() error { return materialisationIncomplete(name, nil) }
	if _, ok := s.Restores[name]; ok {
		return refuse()
	}

	if _, ok := s.Undoing[name]; ok {
		return refuse()
	}

	if _, ok := s.Drilling[name]; ok {
		return refuse()
	}

	if _, ok := s.DrillAttempts[name]; ok {
		return refuse()
	}

	if _, ok := s.AwaitingRecovery[name]; ok {
		return refuse()
	}

	if _, ok := s.Unfollowing[name]; ok {
		return refuse()
	}

	if len(s.LeftoversOf(name)) != 0 {
		return refuse()
	}

	return nil
}

// fenceOnlyFinalVerify re-proves the pinned absence, the terminal job and the
// absence of the configuration and materialisation record under the final
// allocator lock. It is called from inside UpdateRecords as well.
func fenceOnlyFinalVerify(store state.Store, checker *restoreAbsenceCheck, name string, f backup.ActivationFence) error {
	if err := checker.check(); err != nil {
		return err
	}

	if err := checkFenceOnlyForkJob(name, f); err != nil {
		return err
	}

	fresh, _, err := store.Load()
	if err != nil {
		return err
	}

	if _, ok := fresh.ClusterByName(name); ok {
		return materialisationIncomplete(name, nil)
	}

	if _, found, err := readMaterialisation(name, fresh.Server.DataRoot); err != nil || found {
		return materialisationIncomplete(name, nil)
	}

	return nil
}

// fenceOnlyResidue returns the activation fence only when the target is not
// configured and has no materialisation record. Every other layout is left to
// the ordinary drop machinery.
func fenceOnlyResidue(cfg config.Cluster, name string) (backup.ActivationFence, bool, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.ActivationFence{}, true, err
	}

	f, ok := recs.Activation[name]
	if !ok {
		return backup.ActivationFence{}, false, nil
	}

	if _, ok := cfg.ClusterByName(name); ok {
		return backup.ActivationFence{}, false, nil
	}

	_, found, err := readMaterialisation(name, cfg.Server.DataRoot)
	if err != nil {
		return backup.ActivationFence{}, true, materialisationIncomplete(name, nil)
	}

	if found {
		return backup.ActivationFence{}, false, nil
	}

	return f, true, nil
}

// validateFenceOnlyResidue applies the request protections and refuses any
// fence, job or record that is not exactly the abandoned prepared fork.
func validateFenceOnlyResidue(env *Env, cfg config.Cluster, name string, f backup.ActivationFence) error {
	if err := checkPreparedForkRequest(env, cfg, name, fenceOnlyOwner(f)); err != nil {
		return err
	}

	if err := verifyFenceOnlyFork(cfg, name, f); err != nil {
		return err
	}

	if err := checkFenceOnlyForkJob(name, f); err != nil {
		return err
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		return err
	}

	return checkFenceOnlyRecords(&recs, name)
}

// retireFenceOnlyResidue holds the allocator absence lock across the final
// verification and the records compare-and-set, then deletes only the fence.
func retireFenceOnlyResidue(ctx context.Context, store state.Store, cfg config.Cluster, name string, f backup.ActivationFence) (Result, error) {
	uid, err := materialisationPostgresUID()
	if err != nil {
		return Result{}, materialisationIncomplete(name, nil)
	}

	checker, err := openRestoreAbsenceCheck(cfg, name, uid)
	if err != nil {
		return Result{}, materialisationIncomplete(name, nil)
	}
	defer checker.close()

	unlock, err := forkAbsenceReservation(ctx, name)
	if err != nil || unlock == nil {
		return Result{}, materialisationIncomplete(name, nil)
	}
	defer unlock()

	if err := fenceOnlyFinalVerify(store, checker, name, f); err != nil {
		return Result{}, err
	}

	err = forkFenceUpdateRecords(func(s *backup.Records) error {
		if err := fenceOnlyFinalVerify(store, checker, name, f); err != nil {
			return err
		}

		current, ok := s.ActivationFenceFor(name)
		if !ok || !reflect.DeepEqual(current, f) {
			return materialisationIncomplete(name, nil)
		}

		if err := checkFenceOnlyRecords(s, name); err != nil {
			return err
		}

		delete(s.Activation, name)

		return nil
	})
	if err != nil {
		return Result{}, err
	}
	// The exact owner is no longer needed now that its fence and materialisation
	// record are gone. Unrelated retained jobs are untouched.
	releaseMaterialisationJob(f.OperationID)

	return Result{Data: map[string]any{"dropped": name, "metadata_only": true}}, nil
}

// dropAbandonedForkFence handles only fence-only residue: the target is not in
// cluster.toml and has no materialisation record, but an exact prepared fork
// fence remains. held is false for every other state so the ordinary drop
// machinery, including its protected/source/restore/leftover checks, still
// runs.
func dropAbandonedForkFence(ctx context.Context, env *Env, store state.Store, cfg config.Cluster, name string) (Result, bool, error) {
	f, held, err := fenceOnlyResidue(cfg, name)
	if err != nil || !held {
		return Result{}, held, err
	}

	if err := validateFenceOnlyResidue(env, cfg, name, f); err != nil {
		return Result{}, true, err
	}

	res, err := retireFenceOnlyResidue(ctx, store, cfg, name, f)

	return res, true, err
}
