package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

// A fork whose job failed after the copy left a retained materialisation record
// and a fork activation fence. Public apply may finish that non-secret
// continuation, but only under its real global-exclusive and target-exclusive
// locks. The dispatcher issues this lease after it acquires those locks and
// revokes it before releasing them. A lease is not permission to create,
// copy, drop, rename or publish anything: it only admits the exact target to
// the narrow continuation below.
type materialisationContinuationLease struct {
	self       *materialisationContinuationLease
	env        *Env
	requestID  string
	target     string
	global     engine.LockMode
	cluster    engine.LockMode
	active     atomic.Bool
	dispatcher bool
}

func (l *materialisationContinuationLease) valid() bool {
	return l != nil && l.self == l && l.dispatcher && l.active.Load()
}

func (l *materialisationContinuationLease) revoke() {
	if l != nil && l.self == l {
		l.active.Store(false)
	}
}

type materialisationContinuationLeaseKey struct{}

func withMaterialisationContinuationLease(ctx context.Context, l *materialisationContinuationLease) context.Context {
	return context.WithValue(ctx, materialisationContinuationLeaseKey{}, l)
}

func materialisationContinuationLeaseFrom(ctx context.Context) *materialisationContinuationLease {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(materialisationContinuationLeaseKey{}).(*materialisationContinuationLease)
	if !l.valid() {
		return nil
	}
	return l
}

func materialisationContinuationLeaseMatches(ctx context.Context, o materialisationOwnership) bool {
	l := materialisationContinuationLeaseFrom(ctx)
	return l != nil && l.target == o.Target && o.Kind == materialisationFork
}

// issueMaterialisationContinuationLease admits only `apply` with one explicit
// database, no selected steps and a fork fence in a continuing phase. Every
// fact beyond that admission decision is re-proved after this call under the
// acquired locks.
func issueMaterialisationContinuationLease(env *Env, req rpc.Request) *materialisationContinuationLease {
	if env == nil || req.DryRun || req.Method != "apply" {
		return nil
	}
	target, ok := applyContinuationTarget(req.Params)
	if !ok || env.LockMode != engine.LockExclusive || env.ClusterLock != target ||
		env.ClusterLockMode != engine.LockExclusive || !forkContinuationCandidate(env.Cluster, target) {
		return nil
	}
	l := &materialisationContinuationLease{
		env: env, requestID: req.RequestID, target: target,
		global: engine.LockExclusive, cluster: engine.LockExclusive, dispatcher: true,
	}
	l.self = l
	l.active.Store(true)
	return l
}

func applyContinuationTarget(raw json.RawMessage) (string, bool) {
	db := paramDB(raw)
	if db == "" {
		return "", false
	}
	var p struct {
		Steps []string `json:"steps"`
	}
	if len(raw) != 0 && json.Unmarshal(raw, &p) != nil {
		return "", false
	}
	if len(p.Steps) != 0 {
		return "", false
	}
	return db, true
}

// validForkContinuationRequest is the public-request half of the proof. It is
// deliberately narrower than ordinary apply: only one explicit database and no
// selected steps.
func validForkContinuationRequest(req rpc.Request, target string) bool {
	if req.DryRun || req.Method != "apply" {
		return false
	}
	db, ok := applyContinuationTarget(req.Params)
	return ok && db == target
}

func forkContinuationCandidate(cfg config.Cluster, db string) bool {
	spec, ok := cfg.ClusterByName(db)
	if !ok || spec.IsFollower() {
		return false
	}
	records, err := backup.LoadRecords()
	if err != nil {
		return false
	}
	f, ok := records.ActivationFenceFor(db)
	if !ok || f.Kind != "fork" || f.OperationID == "" {
		return false
	}
	switch f.Phase {
	case fencePhaseAdopting, fencePhaseAdopted, fencePhaseRouting:
	default:
		return false
	}
	if err := verifyFenceInstance(f, db); err != nil {
		return false
	}
	if err := verifyFencePaths(cfg, f, db); err != nil {
		return false
	}
	if f.TargetStanza == "" || f.TargetStanza != spec.Stanza || f.NewStanza != spec.Stanza ||
		f.OldStanza == "" || f.OldStanza == f.NewStanza {
		return false
	}
	o, found, err := readMaterialisation(db, cfg.Server.DataRoot)
	if err != nil {
		return false
	}
	if !found {
		return f.Phase == fencePhaseRouting
	}
	return forkContinuationOwnershipShape(cfg, f, spec, o) == nil
}

// forkContinuationOwnershipShape is the read-only relation between the
// retained record, the fence and current independent destination intent. It
// describes an admission candidate only; birth identities, the original job,
// reservation and registration are proved after locking.
func forkContinuationOwnershipShape(cfg config.Cluster, f backup.ActivationFence, spec config.ClusterSpec, o materialisationOwnership) error {
	refuse := func() error { return materialisationIncomplete(o.Target, &o) }
	if o.Kind != materialisationFork || o.Target != f.Database || o.Instance != f.Instance ||
		o.Job == "" || o.Job != f.OperationID || o.Source == "" || o.Stanza == "" ||
		o.Stanza != f.OldStanza || o.Source == o.Target ||
		o.DataPath != cfg.Server.DataDirectory(o.Target) ||
		o.RegisteredPath != filepath.Join(materialisationConfigRoot, o.Target) ||
		o.IntendedPort != spec.Port || o.TemporaryPort < config.TemporaryPortMin ||
		o.TemporaryPort > config.TemporaryPortMax {
		return refuse()
	}
	if len(f.Renames) != 1 || f.Renames[o.Source] != o.Target {
		return refuse()
	}
	if f.TargetStanza != spec.Stanza || f.NewStanza != spec.Stanza || spec.IsFollower() {
		return refuse()
	}
	if !validForkContinuationState(f.Phase, o) {
		return refuse()
	}
	return nil
}

func validForkContinuationState(fencePhase string, o materialisationOwnership) bool {
	switch fencePhase {
	case fencePhaseAdopting:
		return o.ArtifactStage == materialisationRegistered &&
			(o.Phase == materialisationCopying || o.Phase == materialisationProvisioning)
	case fencePhaseAdopted:
		return o.ArtifactStage == materialisationRegistered && o.Phase == materialisationProvisioning
	case fencePhaseRouting:
		return o.Phase == materialisationProvisioning &&
			(o.ArtifactStage == materialisationRegistered || o.ArtifactStage == materialisationProvisioned)
	}
	return false
}

// forkContinuationScope is the verified, pinned continuation. It carries the
// exact record and fence snapshot, but no copy/drop/rename authority and no
// route publication authority.
type forkContinuationScope struct {
	self        *forkContinuationScope
	lease       *materialisationContinuationLease
	env         *Env
	target      string
	operationID string
	fence       backup.ActivationFence
	spec        config.ClusterSpec
	owner       materialisationOwnership
	hasOwner    bool
	source      config.ClusterSpec
	artifacts   *materialisationArtifacts
}

func (s *forkContinuationScope) close() {
	if s == nil || s.artifacts == nil {
		return
	}
	s.artifacts.close()
	s.artifacts = nil
}

type forkContinuationScopeKey struct{}

func withForkContinuationScope(ctx context.Context, s *forkContinuationScope) context.Context {
	return context.WithValue(ctx, forkContinuationScopeKey{}, s)
}

func forkContinuationScopeFrom(ctx context.Context) *forkContinuationScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(forkContinuationScopeKey{}).(*forkContinuationScope)
	if s == nil || s.self != s || s.lease == nil || !s.lease.valid() {
		return nil
	}
	return s
}

func materialisationContinuationOwner(ctx context.Context, o materialisationOwnership) bool {
	s := forkContinuationScopeFrom(ctx)
	return s != nil && s.hasOwner && s.target == o.Target && s.owner == o && s.artifacts != nil
}

func hasForkContinuationView(ctx context.Context, target string) bool {
	s := forkContinuationScopeFrom(ctx)
	return s != nil && s.hasOwner && s.target == target
}

func materialisationWithholdsRoutes(ctx context.Context, target string) bool {
	return hasForkMaterialisationView(ctx, target) || hasForkContinuationView(ctx, target)
}

// openForkContinuation re-proves the whole continuation under the acquired
// locks. It pins the record's parents and both artifact directories before any
// effect and never mints a forkMaterialisationAuthority.
func openForkContinuation(ctx context.Context, cfg config.Cluster, env *Env, lease *materialisationContinuationLease) (*forkContinuationScope, error) {
	if lease == nil || !lease.valid() || lease.env != env || lease.env == nil ||
		lease.global != engine.LockExclusive || lease.cluster != engine.LockExclusive ||
		lease.target != lease.env.ClusterLock || env.LockMode != lease.global ||
		env.ClusterLock != lease.target || env.ClusterLockMode != lease.cluster ||
		lease.requestID != env.Req.RequestID || !validForkContinuationRequest(env.Req, lease.target) {
		return nil, materialisationIncomplete(leaseTarget(lease), nil)
	}
	db := lease.target
	f, spec, err := loadForkContinuationFence(cfg, db)
	if err != nil {
		return nil, err
	}
	o, found, err := readMaterialisation(db, cfg.Server.DataRoot)
	if err != nil {
		return nil, materialisationIncomplete(db, nil)
	}
	if !found {
		return openAbsentForkContinuation(cfg, env, lease, db, f, spec)
	}
	if err := forkContinuationOwnershipShape(cfg, f, spec, o); err != nil {
		return nil, err
	}
	if err := verifyForkContinuationOwner(ctx, cfg, spec, o); err != nil {
		return nil, err
	}
	src, err := forkContinuationAdoptionSource(cfg, f, o)
	if err != nil {
		return nil, err
	}
	artifacts, err := openForkContinuationArtifacts(cfg.Server.DataRoot, db, o)
	if err != nil {
		return nil, err
	}
	if err := forkContinuationReservationForPhase(ctx, o); err != nil {
		artifacts.close()
		return nil, materialisationIncomplete(db, &o)
	}
	s := &forkContinuationScope{
		lease: lease, env: env, target: db, operationID: f.OperationID,
		fence: f, spec: spec, owner: o, hasOwner: true, source: src, artifacts: artifacts,
	}
	s.self = s
	return s, nil
}

func loadForkContinuationFence(cfg config.Cluster, db string) (backup.ActivationFence, config.ClusterSpec, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, materialisationIncomplete(db, nil)
	}
	fence, ok := recs.ActivationFenceFor(db)
	if !ok || fence.Kind != "fork" || fence.OperationID == "" {
		return backup.ActivationFence{}, config.ClusterSpec{}, materialisationIncomplete(db, nil)
	}
	return verifyActivationFence(cfg, db, fence.OperationID, "fork", fencePhaseAdopting, fencePhaseAdopted, fencePhaseRouting)
}

func openAbsentForkContinuation(cfg config.Cluster, env *Env, lease *materialisationContinuationLease, db string, f backup.ActivationFence, spec config.ClusterSpec) (*forkContinuationScope, error) {
	if f.Phase != fencePhaseRouting {
		return nil, materialisationIncomplete(db, nil)
	}
	if err := materialisationAbsentDurableForRoot(cfg.Server.DataRoot, db); err != nil {
		return nil, err
	}
	return &forkContinuationScope{
		lease: lease, env: env, target: db, operationID: f.OperationID,
		fence: f, spec: spec,
	}, nil
}

func verifyForkContinuationOwner(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, o materialisationOwnership) error {
	if o.Phase == materialisationProvisioning {
		// Already adopted: the actual registration must be final before any
		// history, package, setting, guard, provisioning or publication effect.
		// An unfinished adoption keeps the temporary/final distinction for
		// AdoptResumedFollower itself.
		if err := verifyForkTargetRegistration(cfg, spec); err != nil {
			return err
		}
	}
	return verifyForkContinuationJob(ctx, o)
}

func forkContinuationAdoptionSource(cfg config.Cluster, f backup.ActivationFence, o materialisationOwnership) (config.ClusterSpec, error) {
	if o.Phase != materialisationCopying {
		return config.ClusterSpec{}, nil
	}
	return verifyForkAdoptionSource(cfg, f, o)
}

func openForkContinuationArtifacts(dataRoot, db string, o materialisationOwnership) (*materialisationArtifacts, error) {
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, materialisationIncomplete(db, &o)
	}
	artifacts, err := openMaterialisationArtifacts(db, dataRoot, uid)
	if err != nil {
		return nil, materialisationIncomplete(db, &o)
	}
	if artifacts.owner != o {
		artifacts.close()
		return nil, materialisationIncomplete(db, &o)
	}
	if err := artifacts.validate(); err != nil {
		artifacts.close()
		return nil, materialisationIncomplete(db, &o)
	}
	return artifacts, nil
}

// forkContinuationReservationForPhase admits a copying owner's exact
// reservation and requires it absent after adoption.
func forkContinuationReservationForPhase(ctx context.Context, o materialisationOwnership) error {
	expect := forkReservationAbsent
	if o.Phase == materialisationCopying {
		expect = forkReservationAny
	}
	return checkForkContinuationReservation(ctx, o, expect)
}

func leaseTarget(l *materialisationContinuationLease) string {
	if l == nil {
		return ""
	}
	return l.target
}

func (s *forkContinuationScope) reload(ctx context.Context, cfg config.Cluster) (backup.ActivationFence, config.ClusterSpec, error) {
	if s == nil || s.self != s || s.artifacts == nil || s.owner != s.artifacts.owner {
		return backup.ActivationFence{}, config.ClusterSpec{}, materialisationIncomplete(s.target, nil)
	}
	if !s.lease.valid() || s.lease.env != s.env || s.lease.target != s.target ||
		s.lease.global != engine.LockExclusive || s.lease.cluster != engine.LockExclusive {
		return backup.ActivationFence{}, config.ClusterSpec{}, materialisationIncomplete(s.target, nil)
	}
	f, spec, err := verifyActivationFence(cfg, s.target, s.operationID, "fork", fencePhaseAdopting, fencePhaseAdopted, fencePhaseRouting)
	if err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	current, found, err := readMaterialisation(s.target, cfg.Server.DataRoot)
	if err != nil || !found || current != s.artifacts.owner {
		return backup.ActivationFence{}, config.ClusterSpec{}, materialisationIncomplete(s.target, &s.artifacts.owner)
	}
	if err := forkContinuationOwnershipShape(cfg, f, spec, current); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	if current.Phase == materialisationProvisioning {
		if err := verifyForkTargetRegistration(cfg, spec); err != nil {
			return backup.ActivationFence{}, config.ClusterSpec{}, err
		}
	}
	if err := s.artifacts.validate(); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, materialisationIncomplete(s.target, &current)
	}
	if err := verifyForkContinuationJob(ctx, current); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	if current.Phase != materialisationCopying {
		if err := checkForkContinuationReservation(ctx, current, forkReservationAbsent); err != nil {
			return backup.ActivationFence{}, config.ClusterSpec{}, materialisationIncomplete(s.target, &current)
		}
	}
	s.fence, s.spec = f, spec
	return f, spec, nil
}

// verifyForkContinuationJob requires the exact original terminal db-fork job.
// A stale non-terminal record is reaped by the ordinary reaper and reread; the
// continuation never marks it terminal by fiat.
func verifyForkContinuationJob(ctx context.Context, o materialisationOwnership) error {
	st, err := forkAbsenceState()
	if err != nil || forkAbsenceActiveLease(o.Target) || forkContinuationForeignActiveJob(st, o.Target, o.Job) {
		return materialisationIncomplete(o.Target, &o)
	}
	job, err := materialisationJob(st, o.Job)
	if err != nil || !forkContinuationJobShape(job, o) {
		return materialisationIncomplete(o.Target, &o)
	}
	if !job.Final() {
		original := job
		if err := forkContinuationReapJob(ctx, o.Job); err != nil {
			return materialisationIncomplete(o.Target, &o)
		}
		st, err = forkAbsenceState()
		if err != nil {
			return materialisationIncomplete(o.Target, &o)
		}
		job, err = materialisationJob(st, o.Job)
		if err != nil || job.ID != original.ID || job.Job != original.Job || job.DB != original.DB ||
			job.Unit != original.Unit || job.PID != original.PID || job.InvocationID != original.InvocationID ||
			job.CreatedAt != original.CreatedAt || job.StartedAt != original.StartedAt || !job.Final() {
			return materialisationIncomplete(o.Target, &o)
		}
	}
	if !forkContinuationJobShape(job, o) || !job.Final() {
		return materialisationIncomplete(o.Target, &o)
	}
	return nil
}

func forkContinuationJobShape(job engine.JobRecord, o materialisationOwnership) bool {
	return job.ID == o.Job && job.Job == "db-fork" && job.DB == o.Target
}

func forkContinuationForeignActiveJob(st engine.State, target, id string) bool {
	for _, rec := range st.Jobs {
		if rec.DB == target && !rec.Final() && rec.ID != id {
			return true
		}
	}
	for _, rec := range st.RetainedJobs {
		if rec.DB == target && !rec.Final() && rec.ID != id {
			return true
		}
	}
	return false
}

var forkContinuationReapJob = reapJobs

func verifyForkAdoptionSource(cfg config.Cluster, f backup.ActivationFence, o materialisationOwnership) (config.ClusterSpec, error) {
	src, ok := cfg.ClusterByName(o.Source)
	if !ok || src.Port < 1 || src.Stanza == "" || src.Stanza != o.Stanza || o.Stanza != f.OldStanza {
		return config.ClusterSpec{}, materialisationIncomplete(o.Target, &o)
	}
	dir := filepath.Join(materialisationConfigRoot, o.Source)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return config.ClusterSpec{}, materialisationIncomplete(o.Target, &o)
	}
	if info, err := os.Lstat(filepath.Join(dir, "postgresql.conf")); err != nil || !info.Mode().IsRegular() {
		return config.ClusterSpec{}, materialisationIncomplete(o.Target, &o)
	}
	return src, nil
}

type forkReservationExpect int

const (
	forkReservationAny forkReservationExpect = iota
	forkReservationAbsent
)

var forkContinuationReservationDir = func() string { return backup.TemporaryPortDir }

// checkForkContinuationReservation reads only the exact durable reservation
// under the allocator lock. It never creates, replaces or removes one.
func checkForkContinuationReservation(ctx context.Context, o materialisationOwnership, expect forkReservationExpect) error {
	if o.TemporaryPort < config.TemporaryPortMin || o.TemporaryPort > config.TemporaryPortMax {
		return errors.New("invalid temporary reservation")
	}
	path := filepath.Join(forkContinuationReservationDir(), o.Target)
	_, err := os.Lstat(path)
	present := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if present {
		if err := reservationBirthEvidence(o.ReservationIdentity); err != nil {
			return err
		}
	}
	unlock, err := forkReservationLock(ctx, o.Target, o.TemporaryPort, present)
	if err != nil || unlock == nil {
		return errors.New("reservation is not exactly owned")
	}
	defer unlock()
	info, err := os.Lstat(path)
	if present {
		if err != nil {
			return errors.New("reservation disappeared")
		}
		if err := verifyReservationBirth(path, info, o.ReservationIdentity); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("reservation appeared")
	}
	if expect == forkReservationAbsent && present {
		return errors.New("reservation must be absent")
	}
	return nil
}

func reservationBirthEvidence(id backup.ReservationIdentity) error {
	if id.Inode == 0 || len(id.Digest) != 64 {
		return errors.New("missing reservation birth evidence")
	}
	if _, err := hex.DecodeString(id.Digest); err != nil {
		return errors.New("invalid reservation birth evidence")
	}
	return nil
}

func verifyReservationBirth(path string, info os.FileInfo, id backup.ReservationIdentity) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Nlink != 1 ||
		st.Dev != id.Device || st.Ino != id.Inode ||
		st.Uid != id.UID || info.Size() > 256 {
		return errors.New("reservation replaced")
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256 {
		return errors.New("reservation unreadable")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != id.Digest {
		return errors.New("reservation content replaced")
	}
	return nil
}

func materialisationAbsentDurableForRoot(dataRoot, target string) error {
	if _, found, err := readMaterialisation(target, dataRoot); err != nil || found {
		return materialisationIncomplete(target, nil)
	}
	if err := materialisationSyncStateDir(); err != nil {
		return err
	}
	if _, found, err := readMaterialisation(target, dataRoot); err != nil || found {
		return materialisationIncomplete(target, nil)
	}
	return nil
}

// resumeForkContinuation is the narrow public-apply continuation. The caller
// holds the verified lease and pinned record; this function never reads the
// original job spec or secrets.
func resumeForkContinuation(ctx context.Context, env *Env, s *forkContinuationScope) (Result, error) {
	if s == nil || s.self != s || s.env != env || !s.hasOwner {
		return Result{}, materialisationIncomplete("", nil)
	}
	cfg := env.Cluster
	if _, _, err := s.reload(ctx, cfg); err != nil {
		return Result{}, err
	}
	switch {
	case s.fence.Phase == fencePhaseAdopting && s.owner.Phase == materialisationCopying:
		if err := s.finishAdoption(ctx, env); err != nil {
			return Result{}, err
		}
	case s.fence.Phase == fencePhaseAdopting && s.owner.Phase == materialisationProvisioning:
		if err := s.verifyAdopted(env); err != nil {
			return Result{}, err
		}
		if err := s.advanceToAdopted(ctx, env); err != nil {
			return Result{}, err
		}
	case s.fence.Phase == fencePhaseAdopted && s.owner.Phase == materialisationProvisioning:
	case s.fence.Phase == fencePhaseRouting && s.owner.Phase == materialisationProvisioning:
		return s.finishRouting(ctx, env)
	default:
		return Result{}, materialisationIncomplete(s.target, &s.owner)
	}
	return s.provisionAndSettle(ctx, env)
}

func (s *forkContinuationScope) finishAdoption(ctx context.Context, env *Env) error {
	guard := &forkAdoptionGuard{scope: s, cfg: env.Cluster, src: s.source}
	if err := forkContinuationAdopt(ctx, env.Cluster, env, s.owner, s.source, s.spec, guard); err != nil {
		return err
	}
	if err := verifyForkTargetRegistration(env.Cluster, s.spec); err != nil {
		return err
	}
	next := s.artifacts.owner
	next.Phase, next.Reason = materialisationProvisioning, ""
	if err := s.artifacts.persistMaterialisation(next); err != nil {
		return err
	}
	s.owner = s.artifacts.owner
	return s.advanceToAdopted(ctx, env)
}

func (s *forkContinuationScope) verifyAdopted(env *Env) error {
	return verifyForkTargetRegistration(env.Cluster, s.spec)
}

func (s *forkContinuationScope) advanceToAdopted(ctx context.Context, env *Env) error {
	if err := advanceActivationFence(env.Cluster, s.target, s.operationID, "fork", fencePhaseAdopting, fencePhaseAdopted, s.spec, func(f *backup.ActivationFence) {
		f.Registration = activationRegistration(s.spec)
		f.Port = s.spec.Port
		f.DataDir = env.Cluster.Server.DataDirectory(s.target)
		f.ConfDir = steps.PGConfDir(s.target)
	}); err != nil {
		return err
	}
	_, _, err := s.reload(ctx, env.Cluster)
	return err
}

func verifyForkTargetRegistration(cfg config.Cluster, spec config.ClusterSpec) error {
	if !followConfRegistered(spec.Name) {
		return output.NewError(output.CodePrecondition,
			spec.Name+"'s adoption did not leave a verified postgresql-common registration",
			output.ExitPrecondition, "Retry vaire apply --db "+spec.Name+"; the fork fence stays adopting.").
			WithDetails(map[string]any{"reason": "activation_registration_missing", "database": spec.Name})
	}
	data, err := followRegisteredData(spec.Name)
	if err != nil {
		return err
	}
	if data == "" || data != cfg.Server.DataDirectory(spec.Name) {
		return output.NewError(output.CodePrecondition,
			spec.Name+"'s registration does not name its deterministic data directory",
			output.ExitPrecondition, "Retry the owning operation; vaire will not adopt another cluster's registration.").
			WithDetails(map[string]any{"reason": "activation_registration_mismatch", "database": spec.Name})
	}
	// The registration's actual port and cluster name are part of the
	// published identity: a temporary-port registration is not adopted.
	m := backup.ResumeMaterialisedWithHost(cfg.Server.DataRoot, spec.Name, "source", 0,
		engine.Shell{}, "db-fork", nil, materialisationConfigRoot, forkContinuationResumeHost())
	if err := m.VerifyFollowerRegistration(spec.Port); err != nil {
		return output.NewError(output.CodePrecondition,
			spec.Name+"'s registration still carries another identity: "+err.Error(),
			output.ExitPrecondition, "Retry the owning operation; vaire will not advance a temporary registration as adopted.").
			WithDetails(map[string]any{"reason": "activation_registration_mismatch", "database": spec.Name})
	}
	return nil
}

// verifyForkRoutingBoundary proves the exact routing fence, the final actual
// registration and the durable absence of the retained materialisation record.
// Every public fork publication/finalization path must pass it before normal
// rendering, publication, resume or fence clear.
func verifyForkRoutingBoundary(cfg config.Cluster, db, opID string) error {
	f, spec, err := verifyActivationFence(cfg, db, opID, "fork", fencePhaseRouting)
	if err != nil {
		return err
	}
	if err := verifyForkTargetRegistration(cfg, spec); err != nil {
		return err
	}
	if err := materialisationAbsentDurableForRoot(cfg.Server.DataRoot, f.Database); err != nil {
		return err
	}
	return nil
}

// prepareForkRoutingFences is the pre-engine boundary for ordinary full or
// step-selected apply. It refuses before extension history/packages/settings or
// shared pooler rendering when a selected fork route still lacks a durable
// marker absence or its final actual registration.
func prepareForkRoutingFences(cfg config.Cluster, dbs []string) error {
	for _, db := range dbs {
		recs, err := backup.LoadRecords()
		if err != nil {
			return materialisationIncomplete(db, nil)
		}
		f, ok := recs.ActivationFenceFor(db)
		if !ok || f.Kind != "fork" || f.Phase != fencePhaseRouting {
			continue
		}
		if err := verifyForkRoutingBoundary(cfg, db, f.OperationID); err != nil {
			return err
		}
	}
	return nil
}

type forkAdoptionGuard struct {
	scope *forkContinuationScope
	cfg   config.Cluster
	src   config.ClusterSpec
}

func (g *forkAdoptionGuard) GuardFollowerResume(ctx context.Context, p backup.MaterialisationPaths, final int, effect func(backup.FollowerResumeEvidence) error) error {
	s := g.scope
	if s == nil || s.self != s || effect == nil || final != s.spec.Port || s.source.Name == "" {
		return materialisationIncomplete(s.target, nil)
	}
	o := s.artifacts.owner
	if p.Name != o.Target || p.Source != o.Source || p.DataDir != o.DataPath ||
		p.ConfigDir != o.RegisteredPath || p.Port != 0 || p.SourcePort != g.src.Port {
		return materialisationIncomplete(s.target, &o)
	}
	if _, _, err := s.reload(ctx, g.cfg); err != nil {
		return err
	}
	if err := checkForkContinuationReservation(ctx, o, forkReservationAny); err != nil {
		return materialisationIncomplete(s.target, &o)
	}
	if err := s.artifacts.validate(); err != nil {
		return materialisationIncomplete(s.target, &o)
	}
	effectErr := effect(backup.FollowerResumeEvidence{
		TemporaryPort: o.TemporaryPort, ReservationIdentity: o.ReservationIdentity,
	})
	if effectErr != nil {
		return errors.Join(effectErr, s.artifacts.validate())
	}
	if err := s.artifacts.validate(); err != nil {
		return materialisationIncomplete(s.target, &o)
	}
	if err := verifyForkContinuationJob(ctx, o); err != nil {
		return err
	}
	return checkForkContinuationReservation(ctx, o, forkReservationAbsent)
}

var forkContinuationAdopt = func(ctx context.Context, cfg config.Cluster, env *Env, o materialisationOwnership, src, spec config.ClusterSpec, guard backup.FollowerResumeGuard) error {
	m := backup.ResumeMaterialisedWithHost(cfg.Server.DataRoot, o.Target, o.Source, src.Port,
		runtimeFor(env, false).Shell, "db-fork", env.Report, materialisationConfigRoot, forkContinuationResumeHost())
	_, err := m.AdoptResumedFollower(ctx, spec.Port, guard)
	return err
}

var forkContinuationResumeHost = func() backup.FollowerResumeHost { return nil }

func (s *forkContinuationScope) provisionAndSettle(ctx context.Context, env *Env) (Result, error) {
	cfg := env.Cluster
	if s.owner.Phase != materialisationProvisioning || s.fence.Phase != fencePhaseAdopted {
		return Result{}, materialisationIncomplete(s.target, &s.owner)
	}
	if err := verifyForkTargetRegistration(cfg, s.spec); err != nil {
		return Result{}, err
	}
	plan, err := copyPlanFromFence(cfg, s.spec, s.fence)
	if err != nil {
		return Result{}, err
	}
	// Record newly incorporated durable requirements before any effect; the
	// observation is not backdated.
	if err := recordCopyRequirements(plan.Plan.Stanza, s.spec.Name, plan.Plan); err != nil {
		return Result{}, output.NewError(output.CodePrecondition,
			"recording the fork's complete extension requirements failed: "+err.Error(),
			output.ExitPrecondition, "The continuation was not started. Retry after fixing the requirement history store.").
			WithDetails(map[string]any{"reason": "extension_history_capture_failed", "database": s.target})
	}
	rt := runtimeFor(env, false)
	if err := prepareCopyExtensions(ctx, plan, rt, env.Report); err != nil {
		return Result{}, err
	}
	cfg, settingsWarnings, err := persistCopySettings(s.spec.Name, plan.Plan, s.spec.DatabaseName())
	if err != nil {
		return Result{}, err
	}
	spec, _ := cfg.ClusterByName(s.target)
	dataDir := cfg.Server.DataDirectory(s.target)
	if _, _, err := backup.InstallCopyPlanGuard(liveCopyGuardEndpoint(spec).ConfDir, dataDir, plan.Plan); err != nil {
		return Result{}, err
	}
	if err := s.artifacts.validate(); err != nil {
		return Result{}, materialisationIncomplete(s.target, &s.owner)
	}
	if err := forkProvisionOwned(ctx, cfg, rt, s.target, "", env.Report); err != nil {
		return Result{}, err
	}
	if err := s.artifacts.validate(); err != nil {
		return Result{}, materialisationIncomplete(s.target, &s.owner)
	}
	cfg, more, err := completeForkActivation(ctx, spec, s.fence, plan, rt, env.Report)
	if err != nil {
		return Result{}, err
	}
	env.Cluster = cfg
	if _, _, err := s.reload(ctx, cfg); err != nil {
		return Result{}, err
	}
	res, err := s.finishRouting(ctx, env)
	if len(settingsWarnings) != 0 {
		res.Warnings = append(settingsWarnings, res.Warnings...)
	}
	if len(more) != 0 {
		res.Warnings = append(res.Warnings, more...)
	}
	return res, err
}

func (s *forkContinuationScope) finishRouting(ctx context.Context, env *Env) (Result, error) {
	cfg := env.Cluster
	f, spec, err := s.reload(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	if f.Phase != fencePhaseRouting || s.owner.Phase != materialisationProvisioning {
		return Result{}, materialisationIncomplete(s.target, &s.owner)
	}
	rt := runtimeFor(env, false)
	verify := func() error { return forkVerifyProvision(ctx, cfg, rt, s.target) }
	switch s.artifacts.owner.ArtifactStage {
	case materialisationRegistered:
		if err := s.artifacts.markMaterialisationProvisioned(verify); err != nil {
			return Result{}, err
		}
	case materialisationProvisioned:
		if err := s.artifacts.withOwnedArtifacts(verify); err != nil {
			return Result{}, err
		}
	default:
		return Result{}, materialisationIncomplete(s.target, &s.owner)
	}
	s.owner = s.artifacts.owner
	// The durable routing checkpoint, the final actual registration, the
	// pinned ownership and the exact reservation absence are re-proved
	// immediately before the marker retires.
	if _, _, err := verifyActivationFence(cfg, s.target, s.operationID, "fork", fencePhaseRouting); err != nil {
		return Result{}, err
	}
	if err := verifyForkTargetRegistration(cfg, spec); err != nil {
		return Result{}, err
	}
	if err := s.artifacts.validate(); err != nil {
		return Result{}, materialisationIncomplete(s.target, &s.owner)
	}
	if err := verifyForkContinuationJob(ctx, s.owner); err != nil {
		return Result{}, err
	}
	if err := s.artifacts.retireProvisioned(func(o materialisationOwnership) (func(), error) {
		return forkReservationLock(ctx, o.Target, o.TemporaryPort, false)
	}); err != nil {
		return Result{}, err
	}
	s.hasOwner = false
	if err := materialisationAbsentDurableForRoot(cfg.Server.DataRoot, s.target); err != nil {
		return Result{}, err
	}
	return s.publishAndVerifyRouting(ctx, env, spec)
}

func (s *forkContinuationScope) publishAndVerifyRouting(ctx context.Context, env *Env, spec config.ClusterSpec) (Result, error) {
	if err := verifyForkRoutingBoundary(env.Cluster, spec.Name, s.operationID); err != nil {
		return Result{}, err
	}
	if err := publishRouting(ctx, env.Cluster, runtimeFor(env, false), env.Report); err != nil {
		return Result{Data: map[string]any{"database": spec.Name, "routing": "pending"},
			Warnings: []output.Warning{{Code: "activation_routing_incomplete",
				Message: spec.Name + "'s route is not published yet: " + err.Error()}}}, nil
	}
	if err := verifyRoutingFence(ctx, env.Cluster, spec, s.operationID, "fork"); err != nil {
		return Result{Data: map[string]any{"database": spec.Name, "routing": "pending"},
			Warnings: []output.Warning{{Code: "activation_routing_incomplete",
				Message: spec.Name + " is not routed yet: " + err.Error()}}}, nil
	}
	return Result{Data: map[string]any{"database": spec.Name, "resumed": true}}, nil
}

// resumeForkRoutingOnly finishes a routing fence whose materialisation record
// is already durably absent. It never adopts, starts or retires an artifact.
func resumeForkRoutingOnly(ctx context.Context, env *Env, f backup.ActivationFence, spec config.ClusterSpec) (Result, error) {
	cfg := env.Cluster
	if err := verifyForkRoutingBoundary(cfg, spec.Name, f.OperationID); err != nil {
		return Result{}, err
	}
	if err := publishRouting(ctx, cfg, runtimeFor(env, false), env.Report); err != nil {
		return Result{Data: map[string]any{"database": spec.Name, "routing": "pending"},
			Warnings: []output.Warning{{Code: "activation_routing_incomplete",
				Message: spec.Name + "'s route is not published yet: " + err.Error()}}}, nil
	}
	if err := verifyRoutingFence(ctx, cfg, spec, f.OperationID, "fork"); err != nil {
		return Result{Data: map[string]any{"database": spec.Name, "routing": "pending"},
			Warnings: []output.Warning{{Code: "activation_routing_incomplete",
				Message: spec.Name + " is not routed yet: " + err.Error()}}}, nil
	}
	return Result{Data: map[string]any{"database": spec.Name, "resumed": true}}, nil
}
