package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/health"
)

var now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// healthy is a fresh server with two databases where every check passes or
// skips.
func healthy() Facts {
	rs := backup.Records{LastBackup: map[string]map[string]backup.BackupResult{}, LastVerify: map[string]backup.VerifyResult{}}
	sample := health.Sample{MaxConnections: 30, Connections: 3, AllowAlterSystem: "off", SSL: "on", DataChecksums: "on", LastArchivedAgeS: 30, LastFailedAgeS: -1}
	return Facts{
		Now: now, OSSupported: true, OSName: "Debian GNU/Linux 13", DiskKnown: true, DiskTotal: 100, DiskFree: 60, DataRoot: "/var/lib/postgresql/18",
		UnattendedActive: true, TimeKnown: true, TimeSynced: true, LockedDownAt: "2026-10-13T10:00:00Z", UFWActive: true,
		TLSMode: "acme", CertKnown: true, CertNotAfter: now.Add(80 * 24 * time.Hour), DNSChecked: true, DNSMatches: true, Hostname: "db1.example.com",
		SecretsAtTime: "2026-10-13T10:05:00Z", MetricsEnabled: true, Exporters: map[string]error{"node_exporter": nil}, EndpointStatus: 401,
		NginxActive: true, PoolersActive: map[string]bool{"pgbouncer": true, "vaire-pgbouncer-session": true}, HasDatabases: true,
		BackupConfigured: true, Records: rs,
		Clusters: []ClusterFacts{
			{Name: "app", Stanza: "app-abc123", State: "running", Sample: sample, PressureKnown: true, CreatedAt: now.Add(-time.Hour), FullInterval: 7 * 24 * time.Hour},
			{Name: "other", Stanza: "other-def456", State: "running", Sample: sample, PressureKnown: true, CreatedAt: now.Add(-time.Hour), FullInterval: 7 * 24 * time.Hour},
		},
	}
}

func byID(results []Result, id, db string) Result {
	for _, r := range results {
		if r.ID == id && r.DB == db {
			return r
		}
	}
	return Result{}
}

func TestFreshServerIsClean(t *testing.T) {
	results := Run(NodeChecks(), healthy(), "")
	for _, r := range results {
		if r.Status == Warn || r.Status == Fail {
			t.Errorf("%s %s: %s %s", r.ID, r.DB, r.Status, r.Detail)
		}
	}
	if byID(results, "backup.full_age", "app").Status != Skip {
		t.Error("a database younger than its first full schedule should skip")
	}
}

func TestDiskThresholds(t *testing.T) {
	for free, want := range map[uint64]Status{60: Pass, 15: Warn, 9: Fail} {
		f := healthy()
		f.DiskFree = free
		if got := byID(Run(NodeChecks(), f, ""), "host.disk_data", "").Status; got != want {
			t.Errorf("%d%% free: %s, want %s", free, got, want)
		}
	}
}

// With no databases the data root isn't made yet; the filesystem measured is
// its parent, and the check passes with an honest detail instead of warning
// that it cannot read the data root.
func TestDiskDataWithoutDataRoot(t *testing.T) {
	f := healthy()
	f.DiskFree, f.DiskTotal = 60, 100
	f.DataRoot, f.DiskPath = "/var/lib/vaire/clusters", "/var/lib"
	r := byID(Run(NodeChecks(), f, ""), "host.disk_data", "")
	if r.Status != Pass || !strings.Contains(r.Detail, "/var/lib") || !strings.Contains(r.Detail, "no databases yet") {
		t.Fatalf("disk_data = %s: %s", r.Status, r.Detail)
	}
	if f.DiskKnown && f.DiskPath == f.DataRoot {
		t.Fatal("the test did not set a distinct parent")
	}
}

func TestTLSExpiry(t *testing.T) {
	for days, want := range map[int]Status{80: Pass, 20: Warn, 5: Fail} {
		f := healthy()
		f.CertNotAfter = now.Add(time.Duration(days) * 24 * time.Hour)
		if got := byID(Run(NodeChecks(), f, ""), "tls.expiry", "").Status; got != want {
			t.Errorf("%d days: %s, want %s", days, got, want)
		}
	}
}

func TestPostgresChecksFlip(t *testing.T) {
	f := healthy()
	f.Clusters[0].HBAWeak = []string{"trust"}
	f.Clusters[0].Overrides = []string{"work_mem"}
	f.Clusters[0].Sample.MaxXIDAge = 1_700_000_000
	f.Clusters[0].Sample.Connections = 25
	f.Clusters[0].Sample.ListenAddresses = "*"
	f.Clusters[1].State = "stopped"
	f.Clusters[1].WouldNotStart = "would_not_start: needs 600 MiB"
	r := Run(NodeChecks(), f, "")
	for _, c := range []struct {
		id, db string
		want   Status
	}{
		{"pg.hba_no_trust", "app", Fail},
		{"pg.config_drift", "app", Warn},
		{"pg.wraparound", "app", Fail},
		{"pg.connections", "app", Warn},
		{"pg.listen_socket_only", "app", Fail},
		{"pg.running", "other", Fail},
		{"pg.ssl", "other", Skip},
		{"pg.would_not_start", "other", Fail},
	} {
		if got := byID(r, c.id, c.db); got.Status != c.want {
			t.Errorf("%s %s: %s (%s), want %s", c.id, c.db, got.Status, got.Detail, c.want)
		}
	}
	if !strings.Contains(byID(r, "pg.config_drift", "app").Detail, "work_mem") {
		t.Error("the override is not named")
	}
}

func TestMemoryPressure(t *testing.T) {
	f := healthy()
	f.Clusters[0].PressureAvg300 = 12
	f.Clusters[1].PrevKnown, f.Clusters[1].PrevHighEvents, f.Clusters[1].HighEvents = true, 4, 9
	r := Run(NodeChecks(), f, "")
	if byID(r, "pg.memory_pressure", "app").Status != Warn || byID(r, "pg.memory_pressure", "other").Status != Warn {
		t.Errorf("pressure %+v %+v", byID(r, "pg.memory_pressure", "app"), byID(r, "pg.memory_pressure", "other"))
	}
}

func TestBackupChecks(t *testing.T) {
	f := healthy()
	f.Clusters[0].CreatedAt = now.Add(-30 * 24 * time.Hour)
	f.Records.LastBackup["app"] = map[string]backup.BackupResult{backup.TypeFull: {FinishedAt: now.Add(-9 * 24 * time.Hour).Format(time.RFC3339)}}
	f.Records.LastVerify["app"] = backup.VerifyResult{OK: true, Stanza: "app-abc123", FinishedAt: now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)}
	f.Records.Drills = []backup.Drill{{Database: "app", Result: backup.DrillFail, Reason: "amcheck", FinishedAt: now.Format(time.RFC3339)}}
	r := Run(NodeChecks(), f, "app")
	for id, want := range map[string]Status{"backup.full_age": Warn, "backup.verify": Warn, "backup.drill": Fail} {
		if got := byID(r, id, "app"); got.Status != want {
			t.Errorf("%s: %s (%s), want %s", id, got.Status, got.Detail, want)
		}
	}
	if byID(r, "pg.running", "other").ID != "" {
		t.Error("--db app still reported other")
	}
	f.Records.LastBackup["app"][backup.TypeFull] = backup.BackupResult{FinishedAt: now.Add(-15 * 24 * time.Hour).Format(time.RFC3339)}
	if got := byID(Run(NodeChecks(), f, ""), "backup.full_age", "app").Status; got != Fail {
		t.Errorf("twice the interval: %s", got)
	}
	f.BackupConfigured = false
	r = Run(NodeChecks(), f, "")
	if byID(r, "backup.offsite", "").Status != Warn || byID(r, "backup.full_age", "app").Status != Skip {
		t.Error("without a bucket: offsite warns, the rest skips")
	}
}

func TestArchivingFailure(t *testing.T) {
	f := healthy()
	f.Clusters[0].Sample.ArchiveFailed, f.Clusters[0].Sample.LastFailedAgeS, f.Clusters[0].Sample.LastArchivedAgeS = 3, 60, 900
	if got := byID(Run(NodeChecks(), f, ""), "backup.archiving", "app").Status; got != Fail {
		t.Errorf("a recent failure with no success since: %s", got)
	}
	f.Clusters[0].Sample.LastArchivedAgeS = 10
	if got := byID(Run(NodeChecks(), f, ""), "backup.archiving", "app").Status; got != Pass {
		t.Errorf("archived after the failure: %s", got)
	}
}

func TestJobs(t *testing.T) {
	f := healthy()
	skipped := engine.JobRecord{Job: "backup-full", DB: "app", State: engine.JobSkipped}
	f.Jobs = []engine.JobRecord{skipped, skipped, skipped, {Job: "backup-diff", DB: "app", State: engine.JobFailed}, {Job: "restore", State: engine.JobFailed}}
	r := Run(NodeChecks(), f, "")
	if got := byID(r, "jobs.skipped", ""); got.Status != Warn || !strings.Contains(got.Detail, "backup-full@app") {
		t.Errorf("skipped %+v", got)
	}
	if got := byID(r, "jobs.failed", ""); got.Status != Warn || strings.Contains(got.Detail, "restore") {
		t.Errorf("failed %+v", got)
	}
}

func TestMetricsAndSecurity(t *testing.T) {
	f := healthy()
	f.Exporters["postgres_exporter"] = errors.New("connection refused")
	f.OpenAllowlists = []string{"network.client_allowlist 0.0.0.0/0"}
	f.LockedDownAt = ""
	r := Run(Filter(NodeChecks(), CatSecurity), f, "")
	for _, x := range r {
		if !x.Security {
			t.Errorf("%s is not a security check", x.ID)
		}
	}
	if byID(r, "metrics.endpoint", "").Status != Warn || byID(r, "net.allowlist_open", "").Status != Warn || byID(r, "ssh.public_closed", "").Status != Fail {
		t.Errorf("security results %+v", r)
	}
	if byID(r, "host.disk_data", "").ID != "" {
		t.Error("disk is not a security check")
	}
}

func TestInitExemptions(t *testing.T) {
	f := healthy()
	f.Init = true
	f.SecretsAtTime = ""
	f.TimeSynced = false
	// --local after the pending lockdown's deadline: public SSH reverted.
	f.LockedDownAt = ""
	r := Run(NodeChecks(), f, "")
	if byID(r, "secrets.confirmed", "").ID != "" {
		t.Error("secrets.confirmed runs during init")
	}
	if byID(r, "backup.full_age", "app").ID != "" {
		t.Error("backup checks run during init")
	}
	if byID(r, "host.time_sync", "").Status != Warn {
		t.Error("an unsynced clock during init is a warning")
	}
	if len(Failed(r)) != 0 {
		t.Errorf("failures during init: %+v", Failed(r))
	}
}

func TestAwaitingRecoverySkips(t *testing.T) {
	f := healthy()
	f.Clusters[0].AwaitingRecovery = true
	f.Clusters[0].State = "stopped"
	for _, x := range Run(NodeChecks(), f, "app") {
		if x.DB == "app" && x.Status != Skip {
			t.Errorf("%s: %s for a database awaiting recovery", x.ID, x.Status)
		}
	}
}

func TestCalendar(t *testing.T) {
	out := `  Original form: Sun *-*-* 01:00:00
Normalized form: Sun *-*-* 01:00:00
    Next elapse: Sun 2026-10-18 01:00:00 CEST
       (in UTC): Sat 2026-10-17 23:00:00 UTC
       From now: 3 days left
       Iter. #2: Sun 2026-10-25 01:00:00 CEST
       (in UTC): Sat 2026-10-24 23:00:00 UTC
`
	times := ParseCalendarIterations(out)
	if len(times) != 2 {
		t.Fatalf("times %v", times)
	}
	d, err := ScheduleInterval(times)
	if err != nil || d != 7*24*time.Hour {
		t.Errorf("interval %v %v", d, err)
	}
	if _, err := ScheduleInterval(times[:1]); err == nil {
		t.Error("one elapse gave an interval")
	}
}

// systemd 259's output (Ubuntu 26.04) for a staggered full and diff
// schedule as timers-v1 renders them: on a UTC server, or with TZ=UTC as the
// node runs it, there is no "(in UTC)" line, and the lines say "Iteration".
// Before, only "(in UTC)" lines were read, so backup.full_age warned "the
// schedule has fewer than two elapses" on every UTC server.
func TestCalendarSystemd259(t *testing.T) {
	cases := []struct {
		name, out string
		want      time.Duration
	}{
		{"weekly full, UTC", `Normalized form: Sun *-*-* 02:41:00
    Next elapse: Sun 2026-10-18 02:41:00 UTC
       From now: 2 days left
   Iteration #2: Sun 2026-10-25 02:41:00 UTC
       From now: 1 week 2 days left
   Iteration #3: Sun 2026-11-01 02:41:00 UTC
       From now: 2 weeks 2 days left
`, 7 * 24 * time.Hour},
		{"daily diff, UTC", `Normalized form: Mon..Sat *-*-* 02:41:00
    Next elapse: Fri 2026-10-16 02:41:00 UTC
       From now: 19h left
   Iteration #2: Sat 2026-10-17 02:41:00 UTC
       From now: 1 day 19h left
   Iteration #3: Mon 2026-10-19 02:41:00 UTC
       From now: 3 days left
`, 48 * time.Hour},
		// A local zone adds "(in UTC)" lines, which win; the DST change
		// makes this week an hour longer in UTC.
		{"weekly full, Europe/Copenhagen", `Normalized form: Sun *-*-* 02:41:00
    Next elapse: Sun 2026-10-18 02:41:00 CEST
       (in UTC): Sun 2026-10-18 00:41:00 UTC
       From now: 2 days left
   Iteration #2: Sun 2026-10-25 02:41:00 CEST
       (in UTC): Sun 2026-10-25 00:41:00 UTC
       From now: 1 week 2 days left
   Iteration #3: Sun 2026-11-01 02:41:00 CET
       (in UTC): Sun 2026-11-01 01:41:00 UTC
       From now: 2 weeks 2 days left
`, 7*24*time.Hour + time.Hour},
	}
	for _, c := range cases {
		times := ParseCalendarIterations(c.out)
		if len(times) != 3 {
			t.Errorf("%s: %d elapses %v", c.name, len(times), times)
			continue
		}
		if d, err := ScheduleInterval(times); err != nil || d != c.want {
			t.Errorf("%s: interval %v %v, want %v", c.name, d, err, c.want)
		}
	}
}

func TestValidOnly(t *testing.T) {
	if !ValidOnly("") || !ValidOnly("security") || ValidOnly("nope") {
		t.Error("ValidOnly")
	}
}
