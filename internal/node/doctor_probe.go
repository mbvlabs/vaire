package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/doctor"
	"github.com/mbvlabs/vaire/internal/steps"
)

// pgUpLine matches postgres_exporter's pg_up sample.
var pgUpLine = regexp.MustCompile(`^pg_up(\{[^}]*\})? (\S+)$`)

// postgresProbes scrapes each database's /probe on loopback with the target
// nginx sends (doctor runs as root, so no credential), and when one fails
// reads postgres_exporter's last errors from its journal.
func postgresProbes(ctx context.Context, cfg config.Cluster) (map[string]doctor.PGProbe, []string) {
	out := map[string]doctor.PGProbe{}
	failed := false
	for _, l := range steps.MetricsLocations(cfg) {
		db := l.Labels["db"]
		if db == "" || l.Args == "" {
			continue
		}
		url := "http://" + steps.PostgresExporterAddr + "/probe?" + l.Args
		p := probePGUp(ctx, url)
		if !p.Up {
			// A scraper's probe may hold vaire_monitor's connection slots;
			// one retry tells that apart from a real failure.
			time.Sleep(2 * time.Second)
			p = probePGUp(ctx, url)
		}
		out[db] = p
		failed = failed || !p.Up
	}
	if !failed {
		return out, nil
	}
	return out, exporterErrors(ctx)
}

func probePGUp(ctx context.Context, url string) doctor.PGProbe {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return doctor.PGProbe{Error: err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return doctor.PGProbe{Error: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return doctor.PGProbe{Error: fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))}
	}
	return parsePGUp(io.LimitReader(resp.Body, 16<<20))
}

// parsePGUp reads pg_up from a probe's exposition.
func parsePGUp(r io.Reader) doctor.PGProbe {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if m := pgUpLine.FindStringSubmatch(sc.Text()); m != nil {
			return doctor.PGProbe{Known: true, Up: m[2] == "1"}
		}
	}
	return doctor.PGProbe{Error: "the probe has no pg_up"}
}

// exporterErrors is postgres_exporter's last few warnings and errors from
// the journal (its per-probe info lines left out), or its last lines when
// there are none.
func exporterErrors(ctx context.Context) []string {
	out, err := steps.Probe(ctx, "journalctl", "--unit", steps.PostgresExporterService, "--lines", "200", "--since", "-1h", "--no-pager", "--output", "cat")
	if err != nil {
		return []string{"journalctl: " + err.Error()}
	}
	return lastExporterErrors(out, 5)
}

func lastExporterErrors(journal string, n int) []string {
	var all, errs []string
	for _, line := range strings.Split(journal, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 400 {
			line = line[:400] + "..."
		}
		if exporterStartupNoise(line) {
			continue
		}
		all = append(all, line)
		// promslog writes level=ERROR, or level=error in go-kit style.
		if l := strings.ToLower(line); strings.Contains(l, "level=error") || strings.Contains(l, "level=warn") {
			errs = append(errs, line)
		}
	}
	if len(errs) == 0 {
		errs = all
	}
	if len(errs) > n {
		errs = errs[len(errs)-n:]
	}
	return errs
}

// exporterStartupNoise reports the warning postgres_exporter logs at every
// start in vaire's probe-only setup: with no DATA_SOURCE_NAME its legacy
// /metrics collector has no DSN. It says nothing about a failing probe. The
// "Error loading config" warning is gone at the source (metrics-v1 renders
// --config.file) but is left out too, for a unit not yet re-rendered.
func exporterStartupNoise(line string) bool {
	return strings.Contains(line, `msg="Failed to create PostgresCollector"`) && strings.Contains(line, "empty dsn") ||
		strings.Contains(line, `msg="Error loading config"`)
}
