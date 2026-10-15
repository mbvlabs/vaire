package top

import (
	"strings"
	"testing"
	"time"
)

func TestStatementDeltasAcrossReset(t *testing.T) {
	prev := map[string]Statement{"1": {QueryID: "1", Calls: 10, TotalMS: 100}, "2": {QueryID: "2", Calls: 50, TotalMS: 900}}
	cur := map[string]Statement{"1": {QueryID: "1", Calls: 15, TotalMS: 400}, "2": {QueryID: "2", Calls: 3, TotalMS: 30}, "3": {QueryID: "3", Calls: 1, TotalMS: 5}}
	got := StatementDeltas("app", prev, cur, 10)
	if len(got) != 3 || got[0].QueryID != "1" || got[0].Calls != 5 || got[0].TotalMS != 300 {
		t.Fatalf("%+v", got)
	}
	if got[1].QueryID != "2" || got[1].Calls != 3 {
		t.Errorf("a reset statement counts from zero: %+v", got[1])
	}
	if StatementDeltas("app", cur, cur, 10) != nil {
		t.Error("no new calls, but deltas")
	}
}

func TestBlockingChains(t *testing.T) {
	sessions := ParseSessions("app", []map[string]string{
		{"pid": "100", "role": "app_migrate", "state": "idle in transaction", "secs": "30"},
		{"pid": "200", "role": "app_app", "state": "active", "secs": "12", "blocked_by": "100"},
		{"pid": "300", "role": "app_app", "state": "active", "blocked_by": "100,999"},
	})
	got := BlockingChains(sessions)
	if len(got) != 2 || got[0] != "app: 200 (app_app, 12s) waits for 100 (app_migrate, idle in transaction)" || !strings.HasSuffix(got[1], ", 999") {
		t.Errorf("%q", got)
	}
}

func TestBuild(t *testing.T) {
	t0 := time.Unix(1000, 0)
	prev := map[string]Sample{"app": {DB: "app", At: t0, Counters: Counters{Xacts: 100, BlksHit: 90, BlksRead: 10}}}
	cur := map[string]Sample{
		"app":   {DB: "app", At: t0.Add(2 * time.Second), Counters: Counters{Xacts: 120, BlksHit: 189, BlksRead: 11, MaxConnections: 30}, Sessions: ParseSessions("app", []map[string]string{{"pid": "1", "state": "active", "secs": "4"}, {"pid": "2", "state": "idle in transaction (aborted)", "secs": "9"}})},
		"other": {DB: "other", At: t0, Error: "connection refused"},
	}
	s := Build(prev, cur, nil, "", t0)
	if len(s.Databases) != 2 {
		t.Fatalf("%+v", s.Databases)
	}
	app := s.Databases[0]
	if app.TPS == nil || *app.TPS != 10 || app.CacheHit == nil || *app.CacheHit != 0.99 || app.Active != 1 || app.IdleInTx != 1 || app.LongestXact != 9 {
		t.Errorf("app %+v", app)
	}
	if s.Databases[1].Error == "" || s.Databases[1].TPS != nil {
		t.Errorf("other %+v", s.Databases[1])
	}
	out := Render(s, "", "time", 120, 40)
	if !strings.Contains(out, "connection refused") || !strings.Contains(out, "SESSIONS") {
		t.Errorf("render:\n%s", out)
	}
	filtered := Build(prev, cur, nil, "other", t0)
	if len(filtered.Sessions) != 0 {
		t.Error("the filter kept app's sessions")
	}
}

func TestParsePools(t *testing.T) {
	p := ParsePools("session", []map[string]string{{"database": "pgbouncer"}, {"database": "app__auth"}, {"database": "app", "cl_active": "2", "maxwait": "1"}})
	if len(p) != 1 || p[0].ClActive != 2 || p[0].Pooler != "session" {
		t.Errorf("%+v", p)
	}
}

func TestClip(t *testing.T) {
	if clip("abcdef", 4) != "abc…" || clip("ab", 4) != "ab" {
		t.Error("clip")
	}
}
