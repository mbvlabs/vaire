package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
)

var drillCleanupUpdateRecords = backup.UpdateRecords

// drillCleanup pins the artifacts born by an ended drill. The caller holds
// either the node-exclusive drop lock or the new drill's exact target lease.
type drillCleanup struct {
	ctx                          context.Context
	attempt                      backup.MaterialisationAttempt
	dataRoot                     string
	dataParent, confParent       *os.Root
	dataInfo, confInfo           os.FileInfo
	dataAncestors, confAncestors []materialisationAncestor
	data, conf                   *os.File
}

func (a *drillCleanup) close() {
	for _, f := range []*os.File{a.data, a.conf} {
		if f != nil {
			_ = f.Close()
		}
	}
	for _, root := range []*os.Root{a.dataParent, a.confParent} {
		if root != nil {
			_ = root.Close()
		}
	}
}

func openDrillCleanup(ctx context.Context, cfg config.Cluster, t backup.MaterialisationAttempt) (*drillCleanup, error) {
	if err := validateDrillCleanupAttempt(cfg, t); err != nil {
		return nil, err
	}
	return openDrillCleanupArtifacts(ctx, cfg, t)
}

func validateDrillCleanupAttempt(cfg config.Cluster, t backup.MaterialisationAttempt) error {
	if err := validateDrillCleanupIdentity(t); err != nil {
		return err
	}
	if err := validateDrillCleanupPaths(cfg, t); err != nil {
		return err
	}
	if err := validateDrillCleanupStage(t); err != nil {
		return err
	}
	if err := validateDrillCleanupContext(cfg, t); err != nil {
		return err
	}
	return nil
}

func validateDrillCleanupIdentity(t backup.MaterialisationAttempt) error {
	fail := incompleteMaterialisation(t.Target)
	if config.CheckDatabaseName(t.Source) != nil || config.CheckDatabaseName(t.Target) != nil ||
		t.Target != backup.DrillName(t.Source) || t.Kind != "drill" ||
		t.JobID == "" || t.OperationID == "" || t.InstanceID == "" || t.Stanza == "" || t.SourcePort <= 0 ||
		t.TemporaryPort < config.TemporaryPortMin || t.TemporaryPort > config.TemporaryPortMax ||
		t.ReservationIdentity.Inode == 0 || t.ReservationIdentity.Digest == "" ||
		t.DataIdentity.Inode == 0 {
		return fail
	}
	return nil
}

func validateDrillCleanupPaths(cfg config.Cluster, t backup.MaterialisationAttempt) error {
	if t.DataPath != filepath.Join(cfg.Server.DataRoot, t.Target) || t.ConfigPath != filepath.Join(materialisationConfigRoot, t.Target) {
		return incompleteMaterialisation(t.Target)
	}
	return nil
}

func validateDrillCleanupStage(t backup.MaterialisationAttempt) error {
	fail := incompleteMaterialisation(t.Target)
	switch t.ArtifactStage {
	case materialisationDataCreated:
		if t.ConfigIdentity != (backup.ArtifactIdentity{}) {
			return fail
		}
	case materialisationRegistered:
		if t.ConfigIdentity.Inode == 0 {
			return fail
		}
	default:
		return fail
	}
	if t.Reason != "" && t.Reason != "recovery_requirements_unmet" && t.Reason != "recovery_parameter_mismatch" {
		return fail
	}
	return nil
}

func validateDrillCleanupContext(cfg config.Cluster, t backup.MaterialisationAttempt) error {
	fail := incompleteMaterialisation(t.Target)
	instance, err := currentFollowInstance()
	spec, ok := cfg.ClusterByName(t.Source)
	_, targetConfigured := cfg.ClusterByName(t.Target)
	if err != nil || instance != t.InstanceID || !ok || targetConfigured || spec.Port != t.SourcePort || spec.Stanza != t.Stanza {
		return fail
	}
	uid, err := materialisationPostgresUID()
	if err != nil || t.DataIdentity.UID != uid ||
		(t.ConfigIdentity.Inode != 0 && t.ConfigIdentity.UID != uid) {
		return fail
	}
	return nil
}

func openDrillCleanupArtifacts(ctx context.Context, cfg config.Cluster, t backup.MaterialisationAttempt) (*drillCleanup, error) {
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, err
	}
	a := &drillCleanup{ctx: ctx, attempt: t, dataRoot: cfg.Server.DataRoot}
	a.dataParent, a.dataInfo, a.dataAncestors, err = openMaterialisationParent(a.dataRoot, uid, true)
	if err != nil {
		return nil, err
	}
	a.confParent, a.confInfo, a.confAncestors, err = openMaterialisationConfigParent(uid)
	if err != nil {
		a.close()
		return nil, err
	}
	a.data, err = openMaterialisationChild(a.dataParent, t.Target)
	if err == nil && t.ConfigIdentity.Inode != 0 {
		a.conf, err = openMaterialisationChild(a.confParent, t.Target)
	}
	if err == nil {
		err = a.validate(false)
	}
	if err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

type drillArtifact struct {
	root *os.Root
	file *os.File
	id   backup.ArtifactIdentity
	mode os.FileMode
}

func (a *drillCleanup) artifacts() []drillArtifact {
	t := a.attempt
	return []drillArtifact{
		{a.dataParent, a.data, t.DataIdentity, 0o700},
		{a.confParent, a.conf, t.ConfigIdentity, 0o755},
	}
}

func (a *drillCleanup) validate(removed bool) error {
	t := a.attempt
	if err := verifyDrillAttempt(t.Source, t.Target, t); err != nil {
		return err
	}
	instance, err := currentFollowInstance()
	if err != nil || instance != t.InstanceID {
		return incompleteMaterialisation(t.Target)
	}
	if err := finishedDrillJob(a.ctx, t.JobID, t.Source); err != nil {
		return err
	}
	if err := a.validateParents(); err != nil {
		return err
	}
	for _, item := range a.artifacts() {
		if err := a.validateArtifact(item, removed); err != nil {
			return err
		}
	}
	return nil
}

func (a *drillCleanup) validateParents() error {
	return errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.dataRoot, a.dataInfo), validateMaterialisationParent(materialisationConfigRoot, a.confInfo))
}

func (a *drillCleanup) validateArtifact(item drillArtifact, removed bool) error {
	if !removed {
		return validateMaterialisationDir(item.root, a.attempt.Target, restoreNodeID(item.id), item.file, item.mode)
	}
	if _, err := item.root.Lstat(a.attempt.Target); !errors.Is(err, os.ErrNotExist) {
		return errors.New("drill cleanup path remains or was replaced")
	}
	if item.file == nil {
		return nil
	}
	info, err := item.file.Stat()
	if err != nil {
		return err
	}
	id, err := materialisationArtifactID(info)
	st, ok := info.Sys().(*syscall.Stat_t)
	if err != nil || !ok || restoreFileID(id) != item.id || st.Nlink != 0 {
		return errors.New("original drill artifact not removed")
	}
	return nil
}

func (a *drillCleanup) remove(ctx context.Context, env *Env) error {
	t := a.attempt
	err := restoreCleanupReservation(ctx, t.Target, t.TemporaryPort, t.ReservationIdentity, func() error {
		if err := a.validate(false); err != nil {
			return err
		}
		if err := a.dropRegistered(ctx, env); err != nil {
			return err
		}
		if err := a.removeArtifacts(); err != nil {
			return err
		}
		if err := a.validate(true); err != nil {
			return err
		}
		return errors.Join(materialisationSyncArtifactParent(a.dataParent), materialisationSyncArtifactParent(a.confParent))
	})
	if err != nil {
		return err
	}
	unlock, err := restoreCleanupAbsence(ctx, t.Target, t.TemporaryPort, false)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.validate(true); err != nil {
		return err
	}
	err = a.forgetRecord()
	if err == nil {
		releaseMaterialisationJob(t.JobID)
	}
	return err
}

func (a *drillCleanup) dropRegistered(ctx context.Context, env *Env) error {
	t := a.attempt
	if t.ConfigIdentity.Inode == 0 {
		return nil
	}
	_, err := a.confParent.Lstat(filepath.Join(t.Target, "postgresql.conf"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := restoreCleanupRegistration(t.Target, t.Source, a.dataRoot, t.SourcePort, t.TemporaryPort); err != nil {
		return err
	}
	if err := a.validate(false); err != nil {
		return err
	}
	return restoreCleanupDrop(ctx, env, t.Target)
}

// removeArtifacts removes the still-pinned births after rechecking the exact
// record. pg_dropcluster may already have removed the directories.
func (a *drillCleanup) removeArtifacts() error {
	t := a.attempt
	for _, item := range a.artifacts() {
		if err := verifyDrillAttempt(t.Source, t.Target, t); err != nil {
			return err
		}
		if err := a.validateParents(); err != nil {
			return err
		}
		_, err := item.root.Lstat(t.Target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if item.id.Inode == 0 {
			return incompleteMaterialisation(t.Target)
		}
		if err := validateMaterialisationDir(item.root, t.Target, restoreNodeID(item.id), item.file, item.mode); err != nil {
			return err
		}
		if err := item.root.RemoveAll(t.Target); err != nil {
			return err
		}
	}
	return nil
}

func (a *drillCleanup) forgetRecord() error {
	t := a.attempt
	return drillCleanupUpdateRecords(func(s *backup.Records) error {
		if current, ok := s.DrillAttempts[t.Source]; !ok || current != t || s.Drilling[t.Source] != t.Target {
			return incompleteMaterialisation(t.Target)
		}
		delete(s.DrillAttempts, t.Source)
		delete(s.Drilling, t.Source)
		return nil
	})
}
