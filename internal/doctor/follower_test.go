package doctor

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/health"
)

func followerFacts(st health.FollowerState) Facts {
	return Facts{BackupConfigured: true, Clusters: []ClusterFacts{
		{Name: "app", State: "running"},
		{Name: "app_follower", State: "running", Follows: "app", Follower: &st},
	}}
}

func statusOf(results []Result, db string) Status {
	for _, r := range results {
		if r.DB == db {
			return r.Status
		}
	}
	return ""
}

func TestFollowerChecksWithoutFollowers(t *testing.T) {
	f := Facts{Clusters: []ClusterFacts{{Name: "app", State: "running"}}}
	for _, c := range FollowerChecks() {
		res := c.Run(f)
		if len(res) != 1 || res[0].Status != Skip {
			t.Errorf("%s without followers: %+v", c.ID, res)
		}
	}
}

func TestFollowerStreamingAndLag(t *testing.T) {
	ok := health.FollowerState{Source: "app", Slot: "vaire_app_follower", Known: true, Streaming: true, InRecovery: true, SlotExists: true, SlotSafeBytes: -1}
	if s := statusOf(followerStreaming(followerFacts(ok)), "app_follower"); s != Pass {
		t.Fatalf("streaming: %s", s)
	}
	paused := ok
	paused.ReplayPaused = true
	if s := statusOf(followerStreaming(followerFacts(paused)), "app_follower"); s != Fail {
		t.Fatalf("paused: %s", s)
	}
	stopped := ok
	stopped.Streaming = false
	if s := statusOf(followerStreaming(followerFacts(stopped)), "app_follower"); s != Fail {
		t.Fatalf("not streaming: %s", s)
	}
	promoted := ok
	promoted.InRecovery = false
	if s := statusOf(followerStreaming(followerFacts(promoted)), "app_follower"); s != Fail {
		t.Fatalf("out of recovery: %s", s)
	}
	for lag, want := range map[float64]Status{0: Pass, 400: Warn, 4000: Fail} {
		st := ok
		st.LagSeconds = lag
		if s := statusOf(followerLag(followerFacts(st)), "app_follower"); s != want {
			t.Errorf("lag %vs: %s, want %s", lag, s, want)
		}
	}
	for bytes, want := range map[int64]Status{1 << 20: Pass, 2 << 30: Warn, 20 << 30: Fail} {
		st := ok
		st.LagBytes = bytes
		if s := statusOf(followerLag(followerFacts(st)), "app_follower"); s != want {
			t.Errorf("lag %d B: %s, want %s", bytes, s, want)
		}
	}
}

func TestFollowerSlot(t *testing.T) {
	base := health.FollowerState{Source: "app", Slot: "vaire_app_follower", Known: true, Streaming: true, InRecovery: true, SlotExists: true, SlotSafeBytes: -1}
	cases := map[string]struct {
		edit func(*health.FollowerState)
		want Status
	}{
		"fine":    {func(*health.FollowerState) {}, Pass},
		"missing": {func(s *health.FollowerState) { s.SlotExists = false }, Fail},
		"lost":    {func(s *health.FollowerState) { s.SlotWALStatus = "lost" }, Fail},
		"half":    {func(s *health.FollowerState) { s.SlotRetainedBytes, s.SlotSafeBytes = 10, 5 }, Warn},
	}
	for name, tc := range cases {
		st := base
		tc.edit(&st)
		if s := statusOf(followerSlot(followerFacts(st)), "app_follower"); s != tc.want {
			t.Errorf("%s: %s, want %s", name, s, tc.want)
		}
	}
}

func TestFollowerUnreadableAndOrphans(t *testing.T) {
	f := followerFacts(health.FollowerState{})
	if s := statusOf(followerStreaming(f), "app_follower"); s != Warn {
		t.Fatalf("unreadable: %s", s)
	}
	f.OrphanSlots = []OrphanSlot{{Source: "app", Port: 5433, Slot: "vaire_gone"}}
	res := followerOrphanSlots(f)
	if len(res) != 1 || res[0].Status != Warn || res[0].DB != "app" {
		t.Fatalf("%+v", res)
	}
}

func TestBackupChecksSkipFollowers(t *testing.T) {
	f := followerFacts(health.FollowerState{})
	for _, run := range []func(Facts) []Result{backupArchiving, backupFullAge, backupVerify, backupDrill} {
		if s := statusOf(run(f), "app_follower"); s != Skip {
			t.Errorf("a backup check on a follower: %s", s)
		}
	}
}
