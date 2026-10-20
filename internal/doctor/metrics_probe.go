package doctor

import (
	"fmt"
	"strings"
)

// PGProbe is one database's postgres_exporter probe, scraped by the node on
// loopback with the same target nginx sends.
type PGProbe struct {
	// Up is pg_up's value; Known is false when the probe gave no pg_up.
	Up    bool
	Known bool
	// Error is why the probe didn't answer (HTTP error, no pg_up line).
	Error string
}

// metricsPGUp reports pg_up per database. On a failed probe the detail
// carries postgres_exporter's last errors from its journal, so the cause
// shows in vaire doctor without sudo on the node.
func metricsPGUp(f Facts) []Result {
	if !f.MetricsEnabled {
		return one(Skip, "[metrics] enabled is false", "")
	}
	if len(f.Clusters) == 0 {
		return one(Skip, "no databases", "")
	}
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		p, ok := f.PGProbes[c.Name]
		switch {
		case !ok:
			return Skip, "not probed", ""
		case p.Known && p.Up:
			return Pass, "pg_up 1", ""
		}
		detail := "pg_up 0: postgres_exporter can't connect"
		if !p.Known {
			detail = "the probe failed: " + p.Error
		}
		if len(f.PGExporterLog) > 0 {
			detail += fmt.Sprintf("; postgres_exporter's last log lines: %s", strings.Join(f.PGExporterLog, " | "))
		}
		return Warn, detail, "vaire logs postgres_exporter"
	})
}
