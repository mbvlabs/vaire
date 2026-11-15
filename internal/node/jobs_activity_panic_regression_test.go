package node

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/config"
)

type panicActivityStore struct {
	events []activity.Event
}

func (s *panicActivityStore) Append(_ context.Context, ev activity.Event) (activity.Event, error) {
	s.events = append(s.events, ev)
	return ev, nil
}
func (*panicActivityStore) Query(context.Context, activity.Query) (activity.Page, error) {
	return activity.Page{}, nil
}
func (*panicActivityStore) Health(context.Context) (activity.Health, error) {
	return activity.Health{}, nil
}

func TestRunJobPanicAfterActivityStartRecordsUnknown(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) { runJobPanicRegression(t, "backup-full") })
	t.Run("special", func(t *testing.T) { runJobPanicRegression(t, "certs-renew") })
}

func runJobPanicRegression(t *testing.T, job string) {
	t.Helper()
	store := &panicActivityStore{}
	nowCalls := 0
	oldFactory, oldNodeStat, oldUID := activityRecorderFactory, jobNodeStat, jobEffectiveUID
	oldLoad, oldSettle := jobLoadCluster, settleJobRecord
	t.Cleanup(func() {
		activityRecorderFactory, jobNodeStat, jobEffectiveUID = oldFactory, oldNodeStat, oldUID
		jobLoadCluster, settleJobRecord = oldLoad, oldSettle
	})
	activityRecorderFactory = func() *activityRecorder {
		return &activityRecorder{
			store: store, storeOK: true,
			now: func() time.Time {
				nowCalls++
				if nowCalls == 1 {
					panic("injected activity start panic")
				}
				return time.Now()
			},
		}
	}
	jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(t.TempDir()) }
	jobEffectiveUID = func() int { return 0 }
	jobLoadCluster = func(string) (config.Cluster, error) {
		return config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}}}, nil
	}
	settleJobRecord = func(string, bool) error { return nil }

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("the injected panic was swallowed")
		}
		if len(store.events) == 0 {
			t.Fatal("no terminal activity event was recorded")
		}
		last := store.events[len(store.events)-1]
		if last.Outcome != activity.OutcomeUnknown || last.ReasonCode != "panic" {
			t.Fatalf("terminal event = %+v, want unknown/panic", last)
		}
	}()
	_, _, _ = RunJob(context.Background(), job, JobOptions{DB: "app"}, nil)
}
