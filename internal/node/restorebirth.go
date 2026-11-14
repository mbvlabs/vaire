package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/engine"
)

// restoreBirth binds the directories created by this invocation to its
// existing restore record. The record is evidence, not a substitute for the
// live job lease or the allocator capability.
type restoreBirth struct {
	mu                             sync.Mutex
	env                            *jobEnv
	m                              *backup.Materialised
	attempt                        *backup.MaterialisationAttempt
	dataRoot                       string
	uid                            uint32
	dataParent, confParent         *os.Root
	dataParentInfo, confParentInfo os.FileInfo
	dataAncestors, confAncestors   []materialisationAncestor
	data, conf                     *os.File
	populated, registered, started bool
}

var birthDropCluster = func(ctx context.Context, a *restoreBirth) error {
	return a.m.Shell.Exec(ctx, a.env.Job, engine.Command{Name: "pg_dropcluster", Args: []string{"18", a.m.Name, "--stop"}}, a.env.Report)
}

var drillReservationAbsence = backup.LockTemporaryReservationAbsence

func restoreFileID(id materialisationFileID) backup.ArtifactIdentity {
	return backup.ArtifactIdentity{Device: id.Device, Inode: id.Inode, UID: id.UID}
}

func restoreNodeID(id backup.ArtifactIdentity) materialisationFileID {
	return materialisationFileID{Device: id.Device, Inode: id.Inode, UID: id.UID}
}

func newRestoreBirth(env *jobEnv, m *backup.Materialised, attempt *backup.MaterialisationAttempt) (*restoreBirth, error) {
	var a *restoreBirth
	err := withLockedJobTarget(env, m.Name, func() error {
		uid, err := materialisationPostgresUID()
		if err != nil {
			return err
		}
		a = &restoreBirth{env: env, m: m, attempt: attempt, dataRoot: env.Cluster.Server.DataRoot, uid: uid}
		a.dataParent, a.dataParentInfo, a.dataAncestors, err = openMaterialisationParent(a.dataRoot, uid, true)
		if err != nil {
			return err
		}
		a.confParent, a.confParentInfo, a.confAncestors, err = openMaterialisationParent(materialisationConfigRoot, materialisationRecordUID, false)
		if err != nil {
			a.close()
			return err
		}
		if err = a.validate(); err != nil {
			a.close()
			return err
		}
		return nil
	})
	return a, err
}

func (a *restoreBirth) verifyAttempt() error {
	if a.attempt.Kind == "drill" {
		return verifyDrillAttempt(a.attempt.Source, a.attempt.Target, *a.attempt)
	}
	return verifyRestoreAttempt(a.attempt.Source, a.env.ID, a.attempt)
}

func (a *restoreBirth) close() {
	for _, f := range []*os.File{a.data, a.conf} {
		if f != nil {
			_ = f.Close()
		}
	}
	for _, r := range []*os.Root{a.dataParent, a.confParent} {
		if r != nil {
			_ = r.Close()
		}
	}
}

func (a *restoreBirth) validate() error {
	if a.m == nil || a.attempt == nil {
		return errors.New("missing restore authority")
	}
	drill := a.attempt.Kind == "drill"
	if drill {
		if a.env.Job != "restore-drill" || a.env.DB != a.attempt.Source ||
			a.env.ID != a.attempt.JobID || a.env.OperationID != a.attempt.OperationID ||
			a.env.installID != a.attempt.InstanceID {
			return errors.New("drill invocation changed")
		}
	} else if err := validateRestoreInvocation(a.env); err != nil {
		return err
	}
	instance, err := currentFollowInstance()
	if err != nil || instance != a.env.installID {
		return errors.New("restore install changed")
	}
	spec, ok := a.env.Cluster.ClusterByName(a.attempt.Source)
	target := backup.RestoringName(a.attempt.Source)
	if drill {
		target = backup.DrillName(a.attempt.Source)
	}
	if !ok || spec.Port != a.attempt.SourcePort || spec.Stanza != a.attempt.Stanza ||
		a.attempt.Target != target ||
		a.attempt.DataPath != filepath.Join(a.dataRoot, a.attempt.Target) ||
		a.attempt.ConfigPath != filepath.Join(materialisationConfigRoot, a.attempt.Target) {
		return errors.New("restore target changed")
	}
	if err := a.verifyAttempt(); err != nil {
		return err
	}
	if err := a.m.WithForkReservation(context.Background(), func() error { return nil }); err != nil {
		return err
	}
	if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.dataRoot, a.dataParentInfo), validateMaterialisationParent(materialisationConfigRoot, a.confParentInfo)); err != nil {
		return err
	}
	if err := validateMaterialisationDir(a.dataParent, a.attempt.Target, restoreNodeID(a.attempt.DataIdentity), a.data, 0o700); err != nil {
		return err
	}
	return validateMaterialisationDir(a.confParent, a.attempt.Target, restoreNodeID(a.attempt.ConfigIdentity), a.conf, 0o755)
}

func (a *restoreBirth) paths(p backup.MaterialisationPaths) error {
	if p.Name != a.attempt.Target || p.Source != a.attempt.Source || p.Port != a.attempt.TemporaryPort ||
		p.SourcePort != a.attempt.SourcePort || p.DataDir != a.attempt.DataPath || p.ConfigDir != a.attempt.ConfigPath {
		return errors.New("restore materialisation paths changed")
	}
	return nil
}

func (a *restoreBirth) create(p backup.MaterialisationPaths, conf bool) error {
	if err := a.paths(p); err != nil {
		return err
	}
	if err := a.validate(); err != nil {
		return err
	}
	root, mode := a.dataParent, os.FileMode(0o700)
	if conf {
		if a.attempt.ArtifactStage != materialisationDataCreated || !a.populated {
			return errors.New("restore data not bound")
		}
		root, mode = a.confParent, 0o755
	} else if a.attempt.ArtifactStage != "" {
		return errors.New("restore data already bound")
	}
	if err := root.Mkdir(a.attempt.Target, 0o700); err != nil {
		return err
	}
	f, err := openMaterialisationChild(root, a.attempt.Target)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	named, err := root.Lstat(a.attempt.Target)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(named, info) {
		return errors.New("restore artifact replaced")
	}
	id, err := materialisationArtifactID(info)
	if err != nil || id.UID != materialisationRecordUID || info.Mode().Perm() != 0o700 {
		return errors.New("restore artifact creator mismatch")
	}
	if err := materialisationConfigureArtifact(f, a.uid, mode); err != nil {
		return err
	}
	info, err = f.Stat()
	if err != nil {
		return err
	}
	id, err = materialisationArtifactID(info)
	named, ne := root.Lstat(a.attempt.Target)
	if err != nil || ne != nil || !os.SameFile(named, info) || id.UID != a.uid || info.Mode().Perm() != mode {
		return errors.New("restore artifact changed during creation")
	}
	if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.dataRoot, a.dataParentInfo), validateMaterialisationParent(materialisationConfigRoot, a.confParentInfo)); err != nil {
		return err
	}
	if conf {
		if err := validateMaterialisationDir(a.dataParent, a.attempt.Target, restoreNodeID(a.attempt.DataIdentity), a.data, 0o700); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := materialisationSyncArtifactParent(root); err != nil {
		return err
	}
	old := *a.attempt
	next := old
	if conf {
		next.ConfigIdentity, next.ArtifactStage = restoreFileID(id), materialisationRegistered
	} else {
		next.DataIdentity, next.ArtifactStage = restoreFileID(id), materialisationDataCreated
	}
	err = a.m.WithForkReservation(context.Background(), func() error {
		return backup.UpdateRecords(func(s *backup.Records) error {
			if old.Kind == "drill" {
				cur, ok := s.DrillAttempts[old.Source]
				if !ok || cur != old || s.Drilling[old.Source] != old.Target {
					return incompleteMaterialisation(old.Target)
				}
				s.DrillAttempts[old.Source] = next
				return nil
			}
			r, ok := s.Restores[old.Source]
			if !ok || r.Phase != backup.PhaseRestoring || !matchesRestoreInvocation(r, a.env, old.Source) || !restoreRecordMatchesAttempt(r, &old) ||
				r.Attempt == nil || *r.Attempt != old {
				return incompleteMaterialisation(old.Target)
			}
			r.Attempt = &next
			s.Restores[old.Source] = r
			return nil
		})
	})
	if err != nil {
		return err
	}
	*a.attempt = next
	if conf {
		a.conf = f
	} else {
		a.data = f
	}
	f = nil
	return a.validate()
}

func (a *restoreBirth) CreateData(_ context.Context, p backup.MaterialisationPaths) error {
	return withLockedJobTarget(a.env, a.m.Name, func() error { a.mu.Lock(); defer a.mu.Unlock(); return a.create(p, false) })
}
func (a *restoreBirth) CreateConfig(_ context.Context, p backup.MaterialisationPaths) error {
	return withLockedJobTarget(a.env, a.m.Name, func() error { a.mu.Lock(); defer a.mu.Unlock(); return a.create(p, true) })
}
func (a *restoreBirth) Guard(_ context.Context, p backup.MaterialisationPaths, action backup.MaterialisationAction, effect func() error) error {
	return withLockedJobTarget(a.env, a.m.Name, func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if effect == nil {
			return errors.New("missing restore effect")
		}
		if err := a.paths(p); err != nil {
			return err
		}
		switch action {
		case backup.MaterialisationRestore:
			if a.attempt.ArtifactStage != materialisationDataCreated || a.populated {
				return errors.New("restore data not bound")
			}
		case backup.MaterialisationRegister:
			if a.attempt.ArtifactStage != materialisationRegistered || !a.populated || a.registered {
				return errors.New("restore registration not authorized")
			}
		case backup.MaterialisationPrepare, backup.MaterialisationInspect, backup.MaterialisationStart, backup.MaterialisationStop:
			if a.attempt.ArtifactStage != materialisationRegistered || !a.registered || action == backup.MaterialisationStart && a.started {
				return errors.New("restore lifecycle action not authorized")
			}
		default:
			return errors.New("unsupported restore materialisation action")
		}
		if err := a.validate(); err != nil {
			return err
		}
		err := effect()
		post := a.validate()
		if err == nil && post == nil {
			switch action {
			case backup.MaterialisationRestore:
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

// discard handles ordinary (non-retained) failures while the originating
// job still holds its target lease. It never adopts an unbound artifact.
func (a *restoreBirth) discard(ctx context.Context) error {
	return withLockedJobTarget(a.env, a.m.Name, func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if err := a.validate(); err != nil {
			return err
		}
		return a.m.DiscardOwnedReservation(ctx, func() error {
			check := func() error {
				if err := a.verifyAttempt(); err != nil {
					return err
				}
				return errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
					validateMaterialisationParent(a.dataRoot, a.dataParentInfo), validateMaterialisationParent(materialisationConfigRoot, a.confParentInfo))
			}
			if err := check(); err != nil {
				return err
			}
			if a.attempt.ConfigIdentity.Inode != 0 {
				_, err := a.confParent.Lstat(filepath.Join(a.m.Name, "postgresql.conf"))
				if err == nil {
					if err := restoreCleanupRegistration(a.m.Name, a.m.Source, a.dataRoot, a.m.SourcePort, a.m.Port); err != nil {
						return err
					}
					if err := check(); err != nil {
						return err
					}
					if err := errors.Join(
						validateMaterialisationDir(a.dataParent, a.m.Name, restoreNodeID(a.attempt.DataIdentity), a.data, 0o700),
						validateMaterialisationDir(a.confParent, a.m.Name, restoreNodeID(a.attempt.ConfigIdentity), a.conf, 0o755),
					); err != nil {
						return err
					}
					if err := birthDropCluster(ctx, a); err != nil {
						return err
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := check(); err != nil {
				return err
			}
			for _, item := range []struct {
				root *os.Root
				file *os.File
				id   backup.ArtifactIdentity
				mode os.FileMode
			}{{a.dataParent, a.data, a.attempt.DataIdentity, 0o700}, {a.confParent, a.conf, a.attempt.ConfigIdentity, 0o755}} {
				_, err := item.root.Lstat(a.m.Name)
				if err == nil {
					if err := validateMaterialisationDir(item.root, a.m.Name, restoreNodeID(item.id), item.file, item.mode); err != nil {
						return err
					}
					if item.id.Inode == 0 {
						return errors.New("unbound restore artifact")
					}
					if err := item.root.RemoveAll(a.m.Name); err != nil {
						return err
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if item.file != nil {
					info, err := item.file.Stat()
					if err != nil {
						return err
					}
					id, err := materialisationArtifactID(info)
					st, ok := info.Sys().(*syscall.Stat_t)
					if err != nil || !ok || restoreFileID(id) != item.id || st.Nlink != 0 {
						return errors.New("original restore artifact not removed")
					}
				}
			}
			if err := check(); err != nil {
				return err
			}
			return errors.Join(materialisationSyncArtifactParent(a.dataParent), materialisationSyncArtifactParent(a.confParent))
		})
	})
}

// retireDrill only clears the exact record after proving the original
// artifacts and reservation were removed, while holding the job lease.
func (a *restoreBirth) retireDrill(ctx context.Context) error {
	return withLockedJobTarget(a.env, a.m.Name, func() error {
		unlock, err := drillReservationAbsence(ctx, a.m.Name)
		if err != nil {
			return err
		}
		defer unlock()
		if err := a.verifyAttempt(); err != nil {
			return err
		}
		if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
			validateMaterialisationParent(a.dataRoot, a.dataParentInfo), validateMaterialisationParent(materialisationConfigRoot, a.confParentInfo)); err != nil {
			return err
		}
		for _, item := range []struct {
			root *os.Root
			file *os.File
			id   backup.ArtifactIdentity
		}{{a.dataParent, a.data, a.attempt.DataIdentity}, {a.confParent, a.conf, a.attempt.ConfigIdentity}} {
			if _, err := item.root.Lstat(a.m.Name); !errors.Is(err, os.ErrNotExist) {
				return errors.New("drill artifact remains or was replaced")
			}
			if item.file != nil {
				info, err := item.file.Stat()
				if err != nil {
					return err
				}
				id, err := materialisationArtifactID(info)
				st, ok := info.Sys().(*syscall.Stat_t)
				if err != nil || !ok || restoreFileID(id) != item.id || st.Nlink != 0 {
					return errors.New("original drill artifact not removed")
				}
			}
		}
		err = backup.UpdateRecords(func(s *backup.Records) error {
			cur, ok := s.DrillAttempts[a.attempt.Source]
			if !ok || cur != *a.attempt || cur.Reason != "" || s.Drilling[cur.Source] != cur.Target {
				return incompleteMaterialisation(a.m.Name)
			}
			delete(s.DrillAttempts, cur.Source)
			delete(s.Drilling, cur.Source)
			return nil
		})
		if err == nil {
			releaseMaterialisationJob(a.attempt.JobID)
		}
		return err
	})
}
