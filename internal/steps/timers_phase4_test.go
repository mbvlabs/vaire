package steps

import (
	"strings"
	"testing"
)

func TestServerTimersAtWindowStart(t *testing.T) {
	cfg := metricsCluster()
	cfg.Maintenance.Window = "23:30-01:00"
	files := timerFiles(cfg)
	daily := string(files["eru-maint-daily.timer"])
	if !strings.Contains(daily, "OnCalendar=*-*-* 23:30:00") {
		t.Errorf("daily timer:\n%s", daily)
	}
	if !strings.Contains(string(files["eru-maint-weekly.timer"]), "OnCalendar=Sat *-*-* 23:30:00") {
		t.Error("weekly timer")
	}
	refresh := string(files["eru-metrics-refresh.timer"])
	if !strings.Contains(refresh, "OnCalendar=minutely") || !strings.Contains(refresh, "RandomizedDelaySec=0") {
		t.Errorf("refresh timer:\n%s", refresh)
	}
	if !strings.Contains(string(files["eru-doctor.service"]), "eru node run doctor") {
		t.Error("doctor service")
	}
	if _, ok := files["eru-stanza-expiry.timer"]; ok {
		t.Error("no backups, but the expiry timer")
	}
	for _, name := range []string{"eru-maint-daily.service", "eru-metrics-refresh.service", "eru-doctor.service"} {
		if !ownedService.MatchString(name) {
			t.Errorf("%s is not owned, so a removed timer would leave it", name)
		}
	}
	cfg.Maintenance.Window = "garbage"
	if !strings.Contains(string(timerFiles(cfg)["eru-maint-daily.timer"]), "02:00:00") {
		t.Error("an unparseable window falls back to 02:00")
	}
}
