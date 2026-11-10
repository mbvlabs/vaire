package doctor

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/health"
)

func int64p(v int64) *int64       { return &v }
func float64p(v float64) *float64 { return &v }

func followerState() health.FollowerState {
	return health.FollowerState{Source: "app", Slot: "vaire_app_follower", SourceKnown: true, SelfKnown: true,
		Streaming: true, InRecovery: true, SlotExists: true, SlotUncapped: true,
		LagBytes: int64p(0), LagSeconds: float64p(0)}
}

func followerFactsState(state string, st health.FollowerState) Facts {
	return Facts{BackupConfigured: true, Clusters: []ClusterFacts{
		{Name: "app", State: "running"},
		{Name: "app_follower", State: state, Follows: "app", Follower: &st},
	}}
}

func followerFacts(st health.FollowerState) Facts { return followerFactsState("running", st) }

func statusOf(results []Result, db string) Status {
	for _, r := range results {
		if r.DB == db {
			return r.Status
		}
	}
	return ""
}

func detailOf(results []Result, db string) string {
	for _, r := range results {
		if r.DB == db {
			return r.Detail
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
	ok := followerState()
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
		st.LagSeconds = float64p(lag)
		if s := statusOf(followerLag(followerFacts(st)), "app_follower"); s != want {
			t.Errorf("lag %vs: %s, want %s", lag, s, want)
		}
	}
	for bytes, want := range map[int64]Status{1 << 20: Pass, 2 << 30: Warn, 20 << 30: Fail} {
		st := ok
		st.LagBytes = int64p(bytes)
		if s := statusOf(followerLag(followerFacts(st)), "app_follower"); s != want {
			t.Errorf("lag %d B: %s, want %s", bytes, s, want)
		}
	}
}

// R4-1: an unread side cannot produce a diagnosis from its default values.
func TestFollowerPartialReadings(t *testing.T) {
	sourceOnly := followerState()
	sourceOnly.SelfKnown = false
	sourceOnly.InRecovery = false
	if s := statusOf(followerStreaming(followerFacts(sourceOnly)), "app_follower"); s != Warn {
		t.Fatalf("self unknown: %s", s)
	}
	if detail := detailOf(followerStreaming(followerFacts(sourceOnly)), "app_follower"); strings.Contains(detail, "out of recovery") {
		t.Fatalf("self unknown diagnosed as out of recovery: %s", detail)
	}

	selfOnly := followerState()
	selfOnly.SourceKnown = false
	selfOnly.Streaming = false
	if s := statusOf(followerStreaming(followerFacts(selfOnly)), "app_follower"); s != Warn {
		t.Fatalf("source unknown: %s", s)
	}
	if s := statusOf(followerSlot(followerFacts(selfOnly)), "app_follower"); s != Warn {
		t.Fatalf("source unknown diagnosed as a missing slot: %s", s)
	}

	unknownLag := followerState()
	unknownLag.LagBytes, unknownLag.LagSeconds = nil, nil
	if s := statusOf(followerLag(followerFacts(unknownLag)), "app_follower"); s != Warn {
		t.Fatalf("unknown lag must not pass as zero: %s", s)
	}
	bytesOnly := followerState()
	bytesOnly.LagSeconds = nil
	if detail := detailOf(followerLag(followerFacts(bytesOnly)), "app_follower"); !strings.Contains(detail, "replay delay unknown") {
		t.Fatalf("a missing seconds sample is shown as zero: %s", detail)
	}
	if s := statusOf(followerLag(followerFacts(bytesOnly)), "app_follower"); s != Warn {
		t.Fatalf("an unknown seconds sample must not pass as zero: %s", s)
	}
}

func TestFollowerSlot(t *testing.T) {
	base := followerState()
	cases := map[string]struct {
		edit func(*health.FollowerState)
		want Status
	}{
		"fine":    {func(*health.FollowerState) {}, Pass},
		"missing": {func(s *health.FollowerState) { s.SlotExists = false }, Fail},
		"lost":    {func(s *health.FollowerState) { s.SlotWALStatus = "lost" }, Fail},
		"half": {func(s *health.FollowerState) {
			s.SlotRetainedBytes, s.SlotSafeBytes, s.SlotUncapped = int64p(10), int64p(5), false
		}, Warn},
		"negative": {func(s *health.FollowerState) {
			s.SlotRetainedBytes, s.SlotSafeBytes, s.SlotUncapped = int64p(10), int64p(-2), false
		}, Warn},
	}
	for name, tc := range cases {
		st := base
		tc.edit(&st)
		if s := statusOf(followerSlot(followerFacts(st)), "app_follower"); s != tc.want {
			t.Errorf("%s: %s, want %s", name, s, tc.want)
		}
	}
	// An unknown allowance cannot trigger the half-used warning: it is not a
	// measured cap.
	unknown := base
	unknown.SlotRetainedBytes, unknown.SlotSafeBytes, unknown.SlotUncapped = int64p(1<<30), nil, false
	if s := statusOf(followerSlot(followerFacts(unknown)), "app_follower"); s != Pass {
		t.Errorf("unknown allowance: %s, want %s", s, Pass)
	}
}

// R4-2: a stopped follower's source-side slot check still runs and reports
// the WAL the slot keeps; its self-side checks stay skipped.
func TestFollowerSlotForAStoppedFollower(t *testing.T) {
	base := followerState()
	base.SelfKnown, base.InRecovery, base.Streaming = false, false, false
	for name, tc := range map[string]struct {
		edit func(*health.FollowerState)
		want Status
	}{
		"healthy": {func(*health.FollowerState) {}, Pass},
		"half": {func(s *health.FollowerState) {
			s.SlotRetainedBytes, s.SlotSafeBytes, s.SlotUncapped = int64p(10), int64p(5), false
		}, Warn},
		"lost":    {func(s *health.FollowerState) { s.SlotWALStatus = "lost" }, Fail},
		"missing": {func(s *health.FollowerState) { s.SlotExists = false }, Fail},
	} {
		st := base
		tc.edit(&st)
		if s := statusOf(followerSlot(followerFactsState("stopped", st)), "app_follower"); s != tc.want {
			t.Errorf("stopped, %s: %s, want %s", name, s, tc.want)
		}
	}
	if s := statusOf(followerStreaming(followerFactsState("stopped", base)), "app_follower"); s != Skip {
		t.Errorf("stopped streaming check: %s, want %s", s, Skip)
	}

	unreadable := base
	unreadable.SourceKnown = false
	if s := statusOf(followerSlot(followerFactsState("stopped", unreadable)), "app_follower"); s != Warn {
		t.Errorf("stopped with an unreadable source: %s, want %s", s, Warn)
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

// R4-5: a slot-enumeration failure is reported as unknown, not as "no slot
// without its follower".
func TestFollowerOrphanSlotQueryFailure(t *testing.T) {
	f := Facts{OrphanSlotErrors: []OrphanSlotError{{Source: "app", Port: 5433, Error: "connection timed out"}}}
	res := followerOrphanSlots(f)
	if len(res) != 1 || res[0].Status != Warn || res[0].DB != "app" || !strings.Contains(res[0].Detail, "cannot list") {
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

// A failure one side proves fails the check even when the other side cannot
// be read; an unread side only warns when nothing known fails (R4-1).
func TestFollowerStreamingOneSideUnknown(t *testing.T) {
	ok := followerState()
	selfOnly := func(mut func(*health.FollowerState)) health.FollowerState {
		st := ok
		st.SourceKnown, st.SourceError = false, "connection refused"
		mut(&st)
		return st
	}
	sourceOnly := func(mut func(*health.FollowerState)) health.FollowerState {
		st := ok
		st.SelfKnown, st.SelfError = false, "connection refused"
		mut(&st)
		return st
	}
	cases := []struct {
		name string
		st   health.FollowerState
		want Status
	}{
		{"self-only paused", selfOnly(func(s *health.FollowerState) { s.ReplayPaused = true }), Fail},
		{"self-only out of recovery", selfOnly(func(s *health.FollowerState) { s.InRecovery = false }), Fail},
		{"source-only not streaming", sourceOnly(func(s *health.FollowerState) { s.Streaming = false }), Fail},
		{"self-only healthy", selfOnly(func(*health.FollowerState) {}), Warn},
		{"source-only streaming", sourceOnly(func(*health.FollowerState) {}), Warn},
	}
	for _, tc := range cases {
		if s := statusOf(followerStreaming(followerFacts(tc.st)), "app_follower"); s != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, s, tc.want)
		}
	}
}
