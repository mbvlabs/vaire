package doctor

import (
	"strings"
	"testing"
)

func TestMetricsPGUp(t *testing.T) {
	f := Facts{MetricsEnabled: true, Clusters: []ClusterFacts{{Name: "app", State: "running"}, {Name: "other", State: "running"}, {Name: "off", State: "stopped"}}}
	f.PGProbes = map[string]PGProbe{"app": {Known: true, Up: true}, "other": {Known: true}}
	f.PGExporterLog = []string{`level=ERROR msg="Error opening connection to database" err="invalid URL escape"`}
	got := map[string]Result{}
	for _, r := range metricsPGUp(f) {
		got[r.DB] = r
	}
	if got["app"].Status != Pass || got["off"].Status != Skip {
		t.Errorf("results %+v", got)
	}
	if o := got["other"]; o.Status != Warn || !strings.Contains(o.Detail, "invalid URL escape") || o.Fix != "eru logs postgres_exporter" {
		t.Errorf("failed probe %+v: want the exporter's error in the detail", o)
	}
	if r := metricsPGUp(Facts{}); r[0].Status != Skip {
		t.Errorf("metrics off: %+v", r)
	}
}
