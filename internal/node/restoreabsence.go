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

// retireNeverStartedRestore runs only under the existing preparation/abort
// locks. Neither intent nor a missing job is evidence that a target was born.
func retireNeverStartedRestore(ctx context.Context, cfg config.Cluster, db string, expected backup.Restore) error {
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
	state, err := restoreAbsenceState()
	if err != nil {
		return err
	}
	kind := "restore"
	if expected.Recover {
		kind = "recover"
	}
	job, err := materialisationJob(state, expected.ID)
	if err != nil || job.Job != kind || job.DB != db || !job.Final() {
		return incompleteMaterialisation(name)
	}
	uid, err := materialisationPostgresUID()
	if err != nil {
		return err
	}
	data, dataInfo, dataAncestors, err := openMaterialisationParent(cfg.Server.DataRoot, uid, true)
	if err != nil {
		return err
	}
	defer func() { _ = data.Close() }()
	conf, confInfo, confAncestors, err := openMaterialisationParent(materialisationConfigRoot, materialisationRecordUID, false)
	if err != nil {
		return err
	}
	defer func() { _ = conf.Close() }()
	check := func() error {
		if err := errors.Join(validateMaterialisationAncestry(dataAncestors), validateMaterialisationAncestry(confAncestors),
			validateMaterialisationParent(cfg.Server.DataRoot, dataInfo), validateMaterialisationParent(materialisationConfigRoot, confInfo)); err != nil {
			return err
		}
		for _, root := range []*os.Root{data, conf} {
			if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
				return incompleteMaterialisation(name)
			}
		}
		return nil
	}
	unlock, err := restoreAbsenceReservation(ctx, name)
	if err != nil {
		return err
	}
	if unlock == nil {
		return errors.New("reservation absence lock unavailable")
	}
	defer unlock()
	if err := check(); err != nil {
		return err
	}
	err = backup.UpdateRecords(func(s *backup.Records) error {
		current, ok := s.Restores[db]
		if !ok || !reflect.DeepEqual(current, expected) {
			return incompleteMaterialisation(name)
		}
		if err := check(); err != nil {
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
