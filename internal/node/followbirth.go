package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/mbvlabs/vaire/internal/backup"
)

// followBirth binds exclusive directory creation into the existing attempt.
// All effects require the active target lease; metadata alone is not authority.
type followBirth struct {
	mu                               sync.Mutex
	env                              *jobEnv
	m                                *backup.Materialised
	owner                            followOwner
	key                              followAttemptKey
	dataRoot                         string
	sourcePort                       int
	uid                              uint32
	dataParent, configParent         *os.Root
	dataParentInfo, configParentInfo os.FileInfo
	dataAncestors, configAncestors   []materialisationAncestor
	data, conf                       *os.File
	recordInfo                       os.FileInfo
	populated, registered, started   bool
}

func trustedFollowRecord(name string) (os.FileInfo, error) {
	p, err := followOwnershipPath(name)
	if err != nil {
		return nil, err
	}
	ancestors, err := materialisationAncestry(filepath.Dir(p), materialisationRecordUID, false)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Uid != materialisationRecordUID || st.Nlink != 1 {
		return nil, errors.New("untrusted follower record")
	}
	if err := validateMaterialisationAncestry(ancestors); err != nil {
		return nil, err
	}
	return info, nil
}

func newFollowBirth(ctx context.Context, env *jobEnv, m *backup.Materialised, owner followOwner, key followAttemptKey, dataRoot string, sourcePort int) (*followBirth, error) {
	var result *followBirth
	err := withLockedJobTarget(env, owner.Name, func() error {
		info, err := validateFollowBirth(env, owner, key, dataRoot, sourcePort)
		if err != nil {
			return err
		}
		a, err := openFollowBirth(ctx, env, m, owner, key, dataRoot, sourcePort, info)
		if err != nil {
			return err
		}
		result = a
		return nil
	})
	return result, err
}

func validateFollowBirth(env *jobEnv, owner followOwner, key followAttemptKey, dataRoot string, sourcePort int) (os.FileInfo, error) {
	if env.ID != key.Job || env.OperationID != key.Operation || sourcePort < 1 || sourcePort > 65535 ||
		!filepath.IsAbs(dataRoot) || filepath.Clean(dataRoot) != dataRoot {
		return nil, errors.New("follower birth identity mismatch")
	}
	instance, err := currentFollowInstance()
	if err != nil || instance != env.installID {
		return nil, errors.New("follower birth install mismatch")
	}
	info, err := trustedFollowRecord(owner.Name)
	if err != nil {
		return nil, err
	}
	o, err := pendingFollowAttempt(owner, key)
	if err != nil {
		return nil, err
	}
	if o.Attempt.ArtifactStage != "" {
		return nil, errors.New("follower birth already began")
	}
	return info, nil
}

func openFollowBirth(ctx context.Context, env *jobEnv, m *backup.Materialised, owner followOwner, key followAttemptKey, dataRoot string, sourcePort int, info os.FileInfo) (*followBirth, error) {
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, err
	}
	a := &followBirth{env: env, m: m, owner: owner, key: key, dataRoot: dataRoot, sourcePort: sourcePort, uid: uid, recordInfo: info}
	if err := a.validateLeaseIdentity(); err != nil {
		return nil, err
	}
	a.dataParent, a.dataParentInfo, a.dataAncestors, err = openMaterialisationParent(dataRoot, uid, true)
	if err != nil {
		return nil, err
	}
	a.configParent, a.configParentInfo, a.configAncestors, err = openMaterialisationConfigParent(uid)
	if err != nil {
		a.close()
		return nil, err
	}
	if err := a.validate(ctx); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

func (a *followBirth) validateLeaseIdentity() error {
	var p dbFollowParams
	if a.env.Job != "db-follow" || json.Unmarshal(a.env.Params, &p) != nil ||
		p.Name != a.owner.Name || p.Source != a.owner.Source || p.Recover != a.owner.recovering() {
		return errors.New("follower job parameters changed")
	}
	spec, ok := a.env.Cluster.ClusterByName(a.owner.Name)
	if !ok || spec.Follows != a.owner.Source {
		return errors.New("follower target changed")
	}
	source, ok := a.env.Cluster.ClusterByName(a.owner.Source)
	if !ok || source.Port != a.sourcePort || source.Stanza != a.key.SourceStanza {
		return errors.New("follower source changed")
	}
	if p.Recover {
		records, err := backup.LoadRecords()
		if err != nil {
			return err
		}
		awaiting, ok := records.AwaitingRecovery[a.owner.Name]
		if !ok || awaiting.Since != a.owner.Awaiting.Since || awaiting.Stanza != a.owner.Awaiting.Stanza || p.RecoverSince != awaiting.Since {
			return errors.New("follower awaiting identity changed")
		}
	} else if p.Generation == "" || p.Generation != a.owner.Generation || spec.FollowGeneration != p.Generation {
		return errors.New("follower generation changed")
	}
	return nil
}

func (a *followBirth) close() {
	if a.data != nil {
		_ = a.data.Close()
	}
	if a.conf != nil {
		_ = a.conf.Close()
	}
	if a.dataParent != nil {
		_ = a.dataParent.Close()
	}
	if a.configParent != nil {
		_ = a.configParent.Close()
	}
}

func (a *followBirth) validate(ctx context.Context) error {
	if a.m == nil {
		return errors.New("missing follower reservation capability")
	}
	if err := a.m.WithForkReservation(ctx, func() error { return nil }); err != nil {
		return err
	}
	if err := a.validateLeaseIdentity(); err != nil {
		return err
	}
	instance, err := currentFollowInstance()
	if err != nil || instance != a.env.installID {
		return errors.New("follower install changed")
	}
	info, err := trustedFollowRecord(a.owner.Name)
	if err != nil || !os.SameFile(a.recordInfo, info) {
		return errors.New("follower record replaced")
	}
	o, err := pendingFollowAttempt(a.owner, a.key)
	if err != nil {
		return err
	}
	after, err := trustedFollowRecord(a.owner.Name)
	if err != nil || !os.SameFile(a.recordInfo, after) {
		return errors.New("follower record changed")
	}
	if err := validateMaterialisationAncestry(a.dataAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.configAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationParent(a.dataRoot, a.dataParentInfo); err != nil {
		return err
	}
	if err := validateMaterialisationParent(materialisationConfigRoot, a.configParentInfo); err != nil {
		return err
	}
	if err := validateMaterialisationDir(a.dataParent, a.owner.Name, o.Attempt.DataIdentity, a.data, 0o700); err != nil {
		return err
	}
	return validateMaterialisationDir(a.configParent, a.owner.Name, o.Attempt.ConfigIdentity, a.conf, 0o755)
}

func (a *followBirth) finish(ctx context.Context, reason string) error {
	return withLockedJobTarget(a.env, a.owner.Name, func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if reason == "" && !a.started {
			return errors.New("follower copy has not reached readiness")
		}
		if err := a.validate(ctx); err != nil {
			return err
		}
		return a.m.WithForkReservation(ctx, func() error {
			if reason != "" {
				return retainFollowAttempt(a.owner, a.key, reason)
			}
			return completeFollowAttempt(a.owner, a.key)
		})
	})
}

func (a *followBirth) paths(p backup.MaterialisationPaths) error {
	if p.Name != a.owner.Name || p.Source != a.owner.Source ||
		p.DataDir != filepath.Join(a.dataRoot, a.owner.Name) || p.ConfigDir != filepath.Join(materialisationConfigRoot, a.owner.Name) ||
		p.Port != a.key.TemporaryPort || p.SourcePort != a.sourcePort {
		return errors.New("follower materialisation paths changed")
	}
	return nil
}

func (a *followBirth) create(ctx context.Context, p backup.MaterialisationPaths, conf bool) error {
	if err := a.paths(p); err != nil {
		return err
	}
	if err := a.validate(ctx); err != nil {
		return err
	}
	o, err := pendingFollowAttempt(a.owner, a.key)
	if err != nil {
		return err
	}
	root, mode, err := a.createRoot(conf, o)
	if err != nil {
		return err
	}
	f, id, err := a.makeArtifact(root, mode)
	if err != nil {
		return err
	}
	if err := a.checkCreated(conf, o); err != nil {
		_ = f.Close()
		return err
	}
	if err := a.bindCreatedRecord(conf, o, id); err != nil {
		_ = f.Close()
		return err
	}
	if conf {
		a.conf = f
	} else {
		a.data = f
	}
	return a.validate(ctx)
}

func (a *followBirth) createRoot(conf bool, o followOwnership) (*os.Root, os.FileMode, error) {
	if conf {
		if o.Attempt.ArtifactStage != materialisationDataCreated || !a.populated {
			return nil, 0, errors.New("follower data not bound")
		}
		return a.configParent, 0o755, nil
	}
	if o.Attempt.ArtifactStage != "" {
		return nil, 0, errors.New("follower data already bound")
	}
	return a.dataParent, 0o700, nil
}

func (a *followBirth) makeArtifact(root *os.Root, mode os.FileMode) (*os.File, materialisationFileID, error) {
	return createMaterialisationDir(root, a.owner.Name, a.uid, mode)
}

func (a *followBirth) checkCreated(conf bool, o followOwnership) error {
	if err := validateMaterialisationAncestry(a.dataAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.configAncestors); err != nil {
		return err
	}
	if !conf {
		return nil
	}
	return validateMaterialisationDir(a.dataParent, a.owner.Name, o.Attempt.DataIdentity, a.data, 0o700)
}

func (a *followBirth) bindCreatedRecord(conf bool, o followOwnership, id materialisationFileID) error {
	path, err := followOwnershipPath(a.owner.Name)
	if err != nil {
		return err
	}
	before, err := trustedFollowRecord(a.owner.Name)
	if err != nil || !os.SameFile(a.recordInfo, before) {
		return errors.New("follower record replaced")
	}
	current, err := pendingFollowAttempt(a.owner, a.key)
	if err != nil {
		return err
	}
	if *current.Attempt != *o.Attempt {
		return errors.New("follower attempt changed")
	}
	if conf {
		current.Attempt.ConfigIdentity, current.Attempt.ArtifactStage = id, materialisationRegistered
	} else {
		current.Attempt.DataIdentity, current.Attempt.ArtifactStage = id, materialisationDataCreated
	}
	if err := writeFollowRetention(current); err != nil {
		return err
	}
	updated, err := os.Lstat(path)
	if err != nil {
		return err
	}
	a.recordInfo = updated
	return nil
}

func (a *followBirth) CreateData(ctx context.Context, p backup.MaterialisationPaths) error {
	return withLockedJobTarget(a.env, a.owner.Name, func() error { a.mu.Lock(); defer a.mu.Unlock(); return a.create(ctx, p, false) })
}
func (a *followBirth) CreateConfig(ctx context.Context, p backup.MaterialisationPaths) error {
	return withLockedJobTarget(a.env, a.owner.Name, func() error { a.mu.Lock(); defer a.mu.Unlock(); return a.create(ctx, p, true) })
}
func (a *followBirth) Guard(ctx context.Context, p backup.MaterialisationPaths, action backup.MaterialisationAction, effect func() error) error {
	return withLockedJobTarget(a.env, a.owner.Name, func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if effect == nil {
			return errors.New("missing follower effect")
		}
		if err := a.paths(p); err != nil {
			return err
		}
		o, err := pendingFollowAttempt(a.owner, a.key)
		if err != nil {
			return err
		}
		if err := a.guardAction(action, o); err != nil {
			return err
		}
		if err := a.validate(ctx); err != nil {
			return err
		}
		err = effect()
		post := a.validate(ctx)
		if err == nil && post == nil {
			a.recordAction(action)
		}
		return errors.Join(err, post)
	})
}

func (a *followBirth) guardAction(action backup.MaterialisationAction, o followOwnership) error {
	switch action {
	case backup.MaterialisationRestore, backup.MaterialisationBaseBackup:
		if o.Attempt.ArtifactStage != materialisationDataCreated || a.populated {
			return errors.New("follower data not bound")
		}
	case backup.MaterialisationRegister:
		if o.Attempt.ArtifactStage != materialisationRegistered || !a.populated || a.registered {
			return errors.New("follower registration not authorized")
		}
	case backup.MaterialisationInspect, backup.MaterialisationStart, backup.MaterialisationPrepare, backup.MaterialisationStop:
		if o.Attempt.ArtifactStage != materialisationRegistered {
			return errors.New("follower registration not bound")
		}
		if !a.registered || action == backup.MaterialisationStart && a.started {
			return errors.New("follower lifecycle action not authorized")
		}
	case backup.MaterialisationPromote, backup.MaterialisationRename,
		backup.MaterialisationAdopt, backup.MaterialisationDrop:
		return errors.New("unsupported follower materialisation action")
	}
	return nil
}

func (a *followBirth) recordAction(action backup.MaterialisationAction) {
	switch action {
	case backup.MaterialisationRestore, backup.MaterialisationBaseBackup:
		a.populated = true
	case backup.MaterialisationRegister:
		a.registered = true
	case backup.MaterialisationStart:
		a.started = true
	case backup.MaterialisationPrepare, backup.MaterialisationInspect,
		backup.MaterialisationStop, backup.MaterialisationPromote, backup.MaterialisationRename,
		backup.MaterialisationAdopt, backup.MaterialisationDrop:
	}
}
