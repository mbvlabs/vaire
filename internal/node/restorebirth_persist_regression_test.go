package node

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestRestoreBirthPersistsIdentityAfterCancellation(t *testing.T) {
	env, dir := attemptFixture(t)

	m, err := callerNewRestoreOwned(dir, backup.RestoringName("app"), "app", 5432, engine.Shell{}, env.Job, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(m.ReleaseForkReservationAuthority)

	attempt, err := newMaterialisationAttempt(env, m, "app-stanza", "restore")
	if err != nil {
		t.Fatal(err)
	}

	attempt.ReservationIdentity, err = m.ForkReservationIdentity()
	if err != nil {
		t.Fatal(err)
	}

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{
			ID: env.ID, Database: "app", InstanceID: env.installID, OperationID: env.OperationID,
			Phase: backup.PhaseRestoring, Kind: backup.TargetLatest, Target: "latest",
			MaterialisationPending: true, Attempt: &attempt,
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	birth, err := newRestoreBirth(context.Background(), env, m, &attempt)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(birth.close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldConfigure := materialisationConfigureArtifact

	t.Cleanup(func() { materialisationConfigureArtifact = oldConfigure })

	materialisationConfigureArtifact = func(f *os.File, uid uint32, mode os.FileMode) error {
		if err := oldConfigure(f, uid, mode); err != nil {
			return err
		}
		// Cancel after the directory exists but before its identity is
		// recorded, exactly the window that must not strand the target.
		cancel()

		return nil
	}

	err = birth.CreateData(ctx, m.OwnershipPaths())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create returned %v, want context.Canceled after persisting identity", err)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	got := recs.Restores["app"]
	if got.Attempt == nil || got.Attempt.DataIdentity.Inode == 0 ||
		got.Attempt.ArtifactStage != materialisationDataCreated {
		t.Fatalf("created directory was left unbound: %+v", got.Attempt)
	}

	if _, statErr := os.Stat(m.Data); statErr != nil {
		t.Fatalf("created directory is missing: %v", statErr)
	}
}
