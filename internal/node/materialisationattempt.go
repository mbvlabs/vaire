package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
)

// restoreRecordDigest fixes the complete invocation record while its attempt
// advances independently through artifact birth.
func restoreRecordDigest(r backup.Restore) string {
	r.Attempt = nil
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func restoreRecordMatchesAttempt(r backup.Restore, a *backup.MaterialisationAttempt) bool {
	return a != nil && (a.RecordDigest == "" || restoreRecordDigest(r) == a.RecordDigest)
}

var callerNewMaterialised = backup.NewMaterialised
var callerNewRestoreOwned = backup.NewForkMaterialised
var callerNewDrillOwned = backup.NewForkMaterialised
var callerRestoreMaterialised = func(ctx context.Context, m *backup.Materialised, d backup.Driver, opts backup.RestoreOptions) error {
	return m.Restore(ctx, d, opts)
}
var callerRecoveredTo = func(ctx context.Context, m *backup.Materialised) string { return m.RecoveredTo(ctx) }

// newMaterialisationAttempt is durable intent, not proof of artifact birth.
func newMaterialisationAttempt(env *jobEnv, m *backup.Materialised, stanza, kind string) (backup.MaterialisationAttempt, error) {
	instance, err := currentFollowInstance()
	if err != nil {
		return backup.MaterialisationAttempt{}, err
	}
	operation := env.OperationID
	if operation == "" {
		operation = env.ID
	}
	if env.ID == "" || operation == "" || stanza == "" || m.Port == 0 {
		return backup.MaterialisationAttempt{}, errors.New("materialisation attempt identity is incomplete")
	}
	p := m.OwnershipPaths()
	return backup.MaterialisationAttempt{
		JobID: env.ID, OperationID: operation, InstanceID: instance, Kind: kind,
		Source: p.Source, Target: p.Name, Stanza: stanza, SourcePort: p.SourcePort,
		TemporaryPort: p.Port, DataPath: p.DataDir, ConfigPath: p.ConfigDir,
	}, nil
}

func recoveryFailureReason(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(*output.CLIError); ok { //nolint:errorlint // Check each wrapper before descending.
		if details, ok := e.Details.(map[string]any); ok {
			if reason, ok := details["reason"].(string); ok &&
				(reason == "recovery_requirements_unmet" || reason == "recovery_parameter_mismatch") {
				return reason
			}
		}
	}
	if e, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range e.Unwrap() {
			if reason := recoveryFailureReason(child); reason != "" {
				return reason
			}
		}
		return ""
	}
	return recoveryFailureReason(errors.Unwrap(err))
}

func incompleteMaterialisation(name string) error {
	return output.NewError(output.CodePrecondition, "the materialisation of "+name+" has unresolved ownership", output.ExitPrecondition,
		"Retain the partial target; exact ownership proof is required before cleanup.").
		WithDetails(map[string]any{"reason": "materialisation_ownership_unverifiable", "target": name})
}

// sameAttemptIdentity compares immutable intent only. Reason is a later
// outcome, never part of the identity used to authorize a state transition.
func sameAttemptIdentity(actual, expected backup.MaterialisationAttempt) bool {
	actual.Reason, expected.Reason = "", ""
	return actual == expected
}

func verifyRestoreAttempt(db, job string, expected *backup.MaterialisationAttempt) error {
	records, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	r, ok := records.Restores[db]
	if !ok || r.ID != job || expected == nil || r.Attempt == nil || !sameAttemptIdentity(*r.Attempt, *expected) || !restoreRecordMatchesAttempt(r, expected) {
		return incompleteMaterialisation(backup.RestoringName(db))
	}
	return nil
}

// materialiseDrill persists intent before the first restore effect. A failed
// update of the terminal reason leaves the prior intent blocking cleanup.
func materialiseDrill(ctx context.Context, env *jobEnv, m *backup.Materialised, d backup.Driver, opts backup.RestoreOptions, stanza string) (expected *backup.MaterialisationAttempt, started, retain bool, err error) {
	attempt, err := newMaterialisationAttempt(env, m, stanza, "drill")
	if err != nil {
		return nil, false, false, err
	}
	attempt.ReservationIdentity, err = m.ForkReservationIdentity()
	if err != nil {
		return nil, false, false, err
	}
	db, name := m.Source, m.Name
	if err := withLockedJobTarget(env, name, func() error {
		return m.WithForkReservation(ctx, func() error {
			return backup.UpdateRecords(func(s *backup.Records) error {
				if _, exists := s.Drilling[db]; exists {
					return incompleteMaterialisation(name)
				}
				if s.DrillAttempts == nil {
					s.DrillAttempts = make(map[string]backup.MaterialisationAttempt)
				}
				if _, exists := s.DrillAttempts[db]; exists {
					return incompleteMaterialisation(name)
				}
				s.Drilling[db] = name
				s.DrillAttempts[db] = attempt
				return nil
			})
		})
	}); err != nil {
		return nil, false, false, err
	}
	birth, err := newRestoreBirth(env, m, &attempt)
	if err != nil {
		return &attempt, true, true, err
	}
	m.Ownership = birth
	err = callerRestoreMaterialised(ctx, m, d, opts)
	checkErr := verifyDrillAttempt(db, name, attempt)
	if checkErr != nil {
		return &attempt, true, true, errors.Join(err, checkErr)
	}
	if !backup.RetainRecoveryFailure(err) {
		return &attempt, true, false, err
	}
	reason := recoveryFailureReason(err)
	writeErr := backup.UpdateRecords(func(s *backup.Records) error {
		cur, ok := s.DrillAttempts[db]
		if !ok || cur != attempt || s.Drilling[db] != name {
			return incompleteMaterialisation(name)
		}
		cur.Reason = reason
		s.DrillAttempts[db] = cur
		attempt.Reason = reason
		return nil
	})
	return &attempt, true, true, errors.Join(err, writeErr)
}

func verifyDrillAttempt(db, name string, expected backup.MaterialisationAttempt) error {
	s, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	cur, ok := s.DrillAttempts[db]
	if !ok || cur != expected || s.Drilling[db] != name {
		return incompleteMaterialisation(name)
	}
	return nil
}
