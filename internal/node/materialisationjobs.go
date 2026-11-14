package node

import (
	"context"
	"errors"
	"reflect"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/engine"
)

// materialisationJob rejects ambiguous history and accepts retained evidence
// only when it agrees with any still-visible history entry.
func materialisationJob(st engine.State, id string) (engine.JobRecord, error) {
	var found engine.JobRecord
	count := 0
	for _, rec := range st.Jobs {
		if rec.ID == id {
			found, count = rec, count+1
		}
	}
	if count > 1 {
		return engine.JobRecord{}, errors.New("ambiguous materialisation job")
	}
	if retained, ok := st.RetainedJobs[id]; ok {
		if retained.ID != id || count == 1 && !reflect.DeepEqual(found, retained) {
			return engine.JobRecord{}, errors.New("conflicting materialisation job")
		}
		found, count = retained, 1
	}
	if count != 1 {
		return engine.JobRecord{}, errors.New("missing materialisation job")
	}
	return found, nil
}

var releaseMaterialisationJob = func(id string) {
	_ = engine.UpdateState(func(st *engine.State) error {
		delete(st.RetainedJobs, id)
		return nil
	})
}

// Called outside either state or records locks. Failure to inspect records
// retains evidence rather than discarding it.
func releaseResolvedMaterialisationJob(id, job string) {
	if id == "" || job != "restore" && job != "recover" && job != "restore-drill" {
		return
	}
	records, err := backup.LoadRecords()
	if err != nil {
		return
	}
	for _, r := range records.Restores {
		if r.ID == id && (r.MaterialisationPending || r.Attempt != nil) {
			return
		}
	}
	for _, a := range records.DrillAttempts {
		if a.JobID == id {
			return
		}
	}
	releaseMaterialisationJob(id)
}

var reapDrillJob = reapJobs

func finishedDrillJob(ctx context.Context, id, db string) error {
	st, err := restoreAbsenceState()
	if err != nil {
		return err
	}
	rec, err := materialisationJob(st, id)
	if err != nil || rec.Job != "restore-drill" || rec.DB != db {
		return incompleteMaterialisation(backup.DrillName(db))
	}
	if !rec.Final() {
		original := rec
		if err := reapDrillJob(ctx, id); err != nil {
			return err
		}
		st, err = restoreAbsenceState()
		if err != nil {
			return err
		}
		rec, err = materialisationJob(st, id)
		if err == nil && (rec.ID != original.ID || rec.Job != original.Job || rec.DB != original.DB ||
			rec.Unit != original.Unit || rec.PID != original.PID || rec.InvocationID != original.InvocationID ||
			rec.CreatedAt != original.CreatedAt || rec.StartedAt != original.StartedAt) {
			return incompleteMaterialisation(backup.DrillName(db))
		}
	}
	if err != nil || rec.ID != id || rec.Job != "restore-drill" || rec.DB != db || !rec.Final() {
		return incompleteMaterialisation(backup.DrillName(db))
	}
	return nil
}
