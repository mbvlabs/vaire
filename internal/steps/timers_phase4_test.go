package steps

import (
	"strings"
	"testing"
)

func TestServerTimersAtWindowStart(t *testing.T) {
	cfg := metricsCluster()
	cfg.Maintenance.Window = "23:30-01:00"
	files := timerFiles(cfg)
	daily := string(files["vaire-maint-daily.timer"])
	if !strings.Contains(daily, "OnCalendar=*-*-* 23:30:00") {
		t.Errorf("daily timer:\n%s", daily)
	}
	if !strings.Contains(string(files["vaire-maint-weekly.timer"]), "OnCalendar=Sat *-*-* 23:30:00") {
		t.Error("weekly timer")
	}
	refresh := string(files["vaire-metrics-refresh.timer"])
	if !strings.Contains(refresh, "OnCalendar=minutely") || !strings.Contains(refresh, "RandomizedDelaySec=0") {
		t.Errorf("refresh timer:\n%s", refresh)
	}
	if !strings.Contains(string(files["vaire-doctor.service"]), "vaire node run doctor") {
		t.Error("doctor service")
	}
	if _, ok := files["vaire-stanza-expiry.timer"]; ok {
		t.Error("no backups, but the expiry timer")
	}
	for _, name := range []string{"vaire-maint-daily.service", "vaire-metrics-refresh.service", "vaire-doctor.service"} {
		if !ownedService.MatchString(name) {
			t.Errorf("%s is not owned, so a removed timer would leave it", name)
		}
	}
	cfg.Maintenance.Window = "garbage"
	if !strings.Contains(string(timerFiles(cfg)["vaire-maint-daily.timer"]), "02:00:00") {
		t.Error("an unparseable window falls back to 02:00")
	}
}
