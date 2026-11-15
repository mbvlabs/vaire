package node

import (
	"context"
	"errors"
	"os"
	"reflect"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

var restoreAbsenceState = func() (engine.State, error) { return engine.LoadState(engine.StatePath) }
var restoreAbsenceReservation = backup.LockTemporaryReservationAbsence

func validateNeverStartedRestore(db string, expected backup.Restore) error {
	name := backup.RestoringName(db)
	if expected.Database != db || expected.ID == "" || expected.OperationID == "" || expected.InstanceID == "" ||
		!expected.MaterialisationPending || expected.MaterialisationCleaned || expected.Attempt != nil ||
		expected.Port != 0 || expected.Aside != "" || expected.Kind == targetDump ||
		(expected.Phase != backup.PhaseRestoring && expected.Phase != backup.PhaseFailed) {
		return incompleteMaterialisation(name)
	}
	instance, err := currentFollowInstance()
	if err != nil || instance != expected.InstanceID {
		return incompleteMaterialisation(name)
	}
	return nil
}

func checkRestoreJobAbsence(state engine.State, db string, expected backup.Restore) error {
	kind := "restore"
	if expected.Recover {
		kind = "recover"
	}
	job, err := materialisationJob(state, expected.ID)
	if err != nil || job.Job != kind || job.DB != db || !job.Final() {
		return incompleteMaterialisation(backup.RestoringName(db))
	}
	return nil
}

type restoreAbsenceCheck struct {
	cfg, name                    string
	data, conf                   *os.Root
	dataInfo, confInfo           os.FileInfo
	dataAncestors, confAncestors []materialisationAncestor
}

func openRestoreAbsenceCheck(cfg config.Cluster, name string, uid uint32) (*restoreAbsenceCheck, error) {
	c := &restoreAbsenceCheck{cfg: cfg.Server.DataRoot, name: name}
	var err error
	c.data, c.dataInfo, c.dataAncestors, err = openMaterialisationParent(c.cfg, uid, true)
	if err != nil {
		return nil, err
	}
	c.conf, c.confInfo, c.confAncestors, err = openMaterialisationConfigParent(uid)
	if err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *restoreAbsenceCheck) close() {
	if c.data != nil {
		_ = c.data.Close()
	}
	if c.conf != nil {
		_ = c.conf.Close()
	}
}

func (c *restoreAbsenceCheck) check() error {
	if err := errors.Join(validateMaterialisationAncestry(c.dataAncestors), validateMaterialisationAncestry(c.confAncestors),
		validateMaterialisationParent(c.cfg, c.dataInfo), validateMaterialisationParent(materialisationConfigRoot, c.confInfo)); err != nil {
		return err
	}
	for _, root := range []*os.Root{c.data, c.conf} {
		if _, err := root.Lstat(c.name); !errors.Is(err, os.ErrNotExist) {
			return incompleteMaterialisation(c.name)
		}
	}
	return nil
}

// retireNeverStartedRestore runs only under the existing preparation/abort
// locks. Neither intent nor a missing job is evidence that a target was born.
func retireNeverStartedRestore(ctx context.Context, cfg config.Cluster, db string, expected backup.Restore) error {
	name := backup.RestoringName(db)
	if err := validateNeverStartedRestore(db, expected); err != nil {
		return err
	}
	state, err := restoreAbsenceState()
	if err != nil {
		return err
	}
	if err := checkRestoreJobAbsence(state, db, expected); err != nil {
		return err
	}
	uid, err := materialisationPostgresUID()
	if err != nil {
		return err
	}
	checker, err := openRestoreAbsenceCheck(cfg, name, uid)
	if err != nil {
		return err
	}
	defer checker.close()
	unlock, err := restoreAbsenceReservation(ctx, name)
	if err != nil {
		return err
	}
	if unlock == nil {
		return errors.New("reservation absence lock unavailable")
	}
	defer unlock()
	if err := checker.check(); err != nil {
		return err
	}
	err = backup.UpdateRecords(func(s *backup.Records) error {
		current, ok := s.Restores[db]
		if !ok || !reflect.DeepEqual(current, expected) {
			return incompleteMaterialisation(name)
		}
		if err := checker.check(); err != nil {
			return err
		}
		delete(s.Restores, db)
		return nil
	})
	if err == nil {
		releaseMaterialisationJob(expected.ID)
	}
	return err
}
