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

func newFollowBirth(env *jobEnv, m *backup.Materialised, owner followOwner, key followAttemptKey, dataRoot string, sourcePort int) (*followBirth, error) {
	var result *followBirth
	err := withLockedJobTarget(env, owner.Name, func() error {
		if env.ID != key.Job || env.OperationID != key.Operation || sourcePort < 1 || sourcePort > 65535 ||
			!filepath.IsAbs(dataRoot) || filepath.Clean(dataRoot) != dataRoot {
			return errors.New("follower birth identity mismatch")
		}
		instance, err := currentFollowInstance()
		if err != nil || instance != env.installID {
			return errors.New("follower birth install mismatch")
		}
		info, err := trustedFollowRecord(owner.Name)
		if err != nil {
			return err
		}
		o, err := pendingFollowAttempt(owner, key)
		if err != nil {
			return err
		}
		if o.Attempt.ArtifactStage != "" {
			return errors.New("follower birth already began")
		}
		uid, err := materialisationPostgresUID()
		if err != nil {
			return err
		}
		a := &followBirth{env: env, m: m, owner: owner, key: key, dataRoot: dataRoot, sourcePort: sourcePort, uid: uid, recordInfo: info}
		if err := a.validateLeaseIdentity(); err != nil {
			return err
		}
		a.dataParent, a.dataParentInfo, a.dataAncestors, err = openMaterialisationParent(dataRoot, uid, true)
		if err != nil {
			return err
		}
		a.configParent, a.configParentInfo, a.configAncestors, err = openMaterialisationParent(materialisationConfigRoot, materialisationRecordUID, false)
		if err != nil {
			a.close()
			return err
		}
		if err := a.validate(); err != nil {
			a.close()
			return err
		}
		result = a
		return nil
	})
	return result, err
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

func (a *followBirth) validate() error {
	if a.m == nil {
		return errors.New("missing follower reservation capability")
	}
	if err := a.m.WithForkReservation(context.Background(), func() error { return nil }); err != nil {
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
		if err := a.validate(); err != nil {
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

func (a *followBirth) create(p backup.MaterialisationPaths, conf bool) error {
	if err := a.paths(p); err != nil {
		return err
	}
	if err := a.validate(); err != nil {
		return err
	}
	o, err := pendingFollowAttempt(a.owner, a.key)
	if err != nil {
		return err
	}
	root, mode := a.dataParent, os.FileMode(0o700)
	if conf {
		if o.Attempt.ArtifactStage != materialisationDataCreated || !a.populated {
			return errors.New("follower data not bound")
		}
		root, mode = a.configParent, 0o755
	} else if o.Attempt.ArtifactStage != "" {
		return errors.New("follower data already bound")
	}
	if err := root.Mkdir(a.owner.Name, 0o700); err != nil {
		return err
	}
	f, err := openMaterialisationChild(root, a.owner.Name)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	named, err := root.Lstat(a.owner.Name)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(named, info) {
		return errors.New("follower artifact replaced")
	}
	id, err := materialisationArtifactID(info)
	if err != nil || id.UID != materialisationRecordUID || info.Mode().Perm() != 0o700 {
		return errors.New("follower artifact creator mismatch")
	}
	if err := materialisationConfigureArtifact(f, a.uid, mode); err != nil {
		return err
	}
	info, err = f.Stat()
	if err != nil {
		return err
	}
	id, err = materialisationArtifactID(info)
	named, namedErr := root.Lstat(a.owner.Name)
	if err != nil || namedErr != nil || !os.SameFile(named, info) || id.UID != a.uid || info.Mode().Perm() != mode {
		return errors.New("follower artifact changed during creation")
	}
	if err := validateMaterialisationAncestry(a.dataAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.configAncestors); err != nil {
		return err
	}
	if conf {
		if err := validateMaterialisationDir(a.dataParent, a.owner.Name, o.Attempt.DataIdentity, a.data, 0o700); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	parent, err := root.Open(".")
	if err != nil {
		return err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return err
	}
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
	if conf {
		a.conf = f
	} else {
		a.data = f
	}
	f = nil
	return a.validate()
}

func (a *followBirth) CreateData(_ context.Context, p backup.MaterialisationPaths) error {
	return withLockedJobTarget(a.env, a.owner.Name, func() error { a.mu.Lock(); defer a.mu.Unlock(); return a.create(p, false) })
}
func (a *followBirth) CreateConfig(_ context.Context, p backup.MaterialisationPaths) error {
	return withLockedJobTarget(a.env, a.owner.Name, func() error { a.mu.Lock(); defer a.mu.Unlock(); return a.create(p, true) })
}
func (a *followBirth) Guard(_ context.Context, p backup.MaterialisationPaths, action backup.MaterialisationAction, effect func() error) error {
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
		default:
			return errors.New("unsupported follower materialisation action")
		}
		if err := a.validate(); err != nil {
			return err
		}
		err = effect()
		post := a.validate()
		if err == nil && post == nil {
			switch action {
			case backup.MaterialisationRestore, backup.MaterialisationBaseBackup:
				a.populated = true
			case backup.MaterialisationRegister:
				a.registered = true
			case backup.MaterialisationStart:
				a.started = true
			}
		}
		return errors.Join(err, post)
	})
}
