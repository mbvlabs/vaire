package node

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// TestHandleApplyReleasesTransitionTargetLocksOnFailure proves the caller
// releases the extra target locks that applyExtensionTransitions returned on
// every outcome, not only success. The first apply fails after the target lock
// is held; the second apply must acquire it again.
func TestHandleApplyReleasesTransitionTargetLocksOnFailure(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	targetHeld, transitionReleased, targetAcquires := false, 0, 0
	oldTransitions, oldLoad, oldReload := applyTransitions, loadGuardCluster, applyReloadCluster
	oldAcquire, oldAcquireCluster, oldRunLog := serveAcquire, serveAcquireCluster, openServeRunLog
	t.Cleanup(func() {
		applyTransitions, loadGuardCluster, applyReloadCluster = oldTransitions, oldLoad, oldReload
		serveAcquire, serveAcquireCluster, openServeRunLog = oldAcquire, oldAcquireCluster, oldRunLog
	})
	loadGuardCluster = func() (config.Cluster, error) { return cfg, nil }
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }
	serveAcquire = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		return func() {}, nil
	}
	serveAcquireCluster = func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		if targetHeld {
			t.Fatal("the target lock was not released before reacquisition")
		}
		targetAcquires++
		targetHeld = true
		return func() { targetHeld = false }, nil
	}
	openServeRunLog = func(method, id string) (*engine.RunLog, error) {
		return engine.OpenRunLogFile(filepath.Join(t.TempDir(), method+"-"+id+".log"))
	}
	fail := true
	applyTransitions = func(context.Context, *Env, stepParams) ([]output.Warning, func(), error) {
		if !targetHeld {
			t.Fatal("the transition ran without its target lock")
		}
		release := func() {
			if !targetHeld {
				t.Fatal("the transition lock was released twice")
			}
			transitionReleased++
			targetHeld = false
		}
		if fail {
			return nil, release, errors.New("follower runtime verification failed")
		}
		return nil, release, &extensionApplyPendingError{Message: "waiting for the follower restart token"}
	}
	withServeActivity(t, &fakeActivityStore{})
	req := matchingBuildRequest("apply")
	req.Params = json.RawMessage(`{"db":"app"}`)
	out, code := serveRequest(t, req)
	if code == 0 || !strings.Contains(out, "follower runtime verification failed") {
		t.Fatalf("failed apply = %d\n%s", code, out)
	}
	if transitionReleased != 1 || targetHeld {
		t.Fatalf("failure leaked the transition lock: released=%d held=%v", transitionReleased, targetHeld)
	}
	fail = false
	out, code = serveRequest(t, req)
	if code != 0 {
		t.Fatalf("retry apply = %d\n%s", code, out)
	}
	if targetAcquires != 2 || transitionReleased != 2 || targetHeld {
		t.Fatalf("retry locks: acquires=%d released=%d held=%v", targetAcquires, transitionReleased, targetHeld)
	}
}
