package node

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// Existing abstract pooler/extension tests model no PostgreSQL cluster. The
// credential implementation itself is exercised with a real scratch snapshot
// and SQL runner in restore_credentials_test.go.
func stubRestoreCredentialLifecycle(t *testing.T) {
	t.Helper()
	oldCapture, oldApply, oldClear, oldAbort := restoreCredentialsCapture, restoreCredentialsApply, restoreCredentialsClear, restoreCredentialsClearAborted
	t.Cleanup(func() {
		restoreCredentialsCapture, restoreCredentialsApply, restoreCredentialsClear, restoreCredentialsClearAborted = oldCapture, oldApply, oldClear, oldAbort
	})
	restoreCredentialsCapture = func(context.Context, *restorePromotion) error { return nil }
	restoreCredentialsApply = func(context.Context, *restorePromotion) error { return nil }
	restoreCredentialsClear = func(*restorePromotion) error { return nil }
	restoreCredentialsClearAborted = func(config.Cluster, backup.Restore, string) error { return nil }
}

func TestRestoreCredentialFailuresKeepPromotionFencedAndRetryable(t *testing.T) {
	for _, stage := range []string{"capture", "apply", "retirement"} {
		t.Run(stage, func(t *testing.T) {
			cfg, published := swappedPromotionFixture(t)
			fail, cleared := true, false
			stubCredentialFailure(t, stage, &fail, &cleared)
			_, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {})
			if err == nil || *published || cleared {
				t.Fatalf("credential failure exposed or retired target: error=%v published=%v cleared=%v", err, *published, cleared)
			}
			restoreCredentialRetry(t, cfg, stage, &fail, published, &cleared)
		})
	}
}

func stubCredentialFailure(t *testing.T, stage string, fail, cleared *bool) {
	t.Helper()
	marker := errors.New("credential lifecycle failure")
	restoreCredentialsCapture = func(context.Context, *restorePromotion) error {
		if *fail && stage == "capture" {
			return marker
		}
		return nil
	}
	restoreCredentialsApply = func(_ context.Context, p *restorePromotion) error {
		if *fail && stage == "apply" {
			return marker
		}
		if *fail && stage == "retirement" {
			return backup.UpdateRecords(func(recs *backup.Records) error {
				r := recs.Restores[p.db]
				r.Aside = "another-operation-aside"
				recs.Restores[p.db] = r
				return nil
			})
		}
		return nil
	}
	restoreCredentialsClear = func(p *restorePromotion) error {
		recs, err := backup.LoadRecords()
		if err != nil {
			return err
		}
		if _, open := recs.Restores[p.db]; open {
			t.Fatal("credential snapshot removed before durable retirement")
		}
		*cleared = true
		return nil
	}
}

func restoreCredentialRetry(t *testing.T, cfg config.Cluster, stage string, fail, published, cleared *bool) {
	t.Helper()
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	fence, fenced := recs.ActivationFenceFor("app")
	if _, open := recs.Restores["app"]; !open || !fenced || fence.Phase == fencePhaseRouting {
		t.Fatalf("credential failure lost its owning restore/fence: %+v", recs)
	}
	if stage == "retirement" {
		if err := backup.UpdateRecords(func(recs *backup.Records) error {
			r := recs.Restores["app"]
			r.Aside = "app__pre_restore_20260101T000000"
			recs.Restores["app"] = r
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	*fail = false
	if _, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	if !*published || !*cleared {
		t.Fatalf("credential retry incomplete: published=%v cleared=%v", *published, *cleared)
	}
}
