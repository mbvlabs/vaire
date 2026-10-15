package render

import (
	"encoding/json"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/mbvlabs/eru/internal/config"
)

// ExporterUnit is one exporter's systemd unit (Phase 4).
type ExporterUnit struct {
	Description     string
	User            string
	Slice           string
	Binary          string
	Args            []string
	EnvironmentFile string
	MemoryMax       string
	After           []string
}

// ExporterService renders an exporter's unit.
func ExporterService(u ExporterUnit) []byte { return execute("exporter.service.tmpl", u) }

// ExportersSlice renders eru-exporters.slice.
func ExportersSlice() []byte { return execute("eru-exporters.slice.tmpl", nil) }

// ProbeTarget is the DSN postgres_exporter's /probe connects to for a
// cluster (Phase 4 D4): its socket and port, the application database (so
// stat_user_tables and stat_statements see the application's tables) as
// eru_monitor with peer auth. The role's statement_timeout bounds a probe.
func ProbeTarget(socketDir string, spec config.ClusterSpec) string {
	return "host=" + socketDir + " port=" + strconv.Itoa(spec.Port) + " dbname=" + spec.Name +
		" user=eru_monitor sslmode=disable connect_timeout=5"
}

// MetricsLocation is one exact location of the /metrics endpoint.
type MetricsLocation struct {
	Path string
	// Upstream is the exporter URL nginx proxies to.
	Upstream string
	// Args replaces the client's query string (postgres_exporter's probe
	// target), already URL-encoded.
	Args string
	// Alias serves a static file instead (targets.json).
	Alias string
	// Labels describe the path in targets.json.
	Labels map[string]string
}

// MetricsConf renders /etc/nginx/eru-metrics.conf from the locations, with
// the basic-auth file and the optional allowlist (CIDRs, masked). A set
// allowlist also admits loopback, so doctor's probe through 443 gets the 401
// it checks for: the address comes from the PROXY header the 443 hop writes
// from the real peer, so only the node itself can be 127.0.0.1, and it can
// reach the exporters directly anyway.
func MetricsConf(locations []MetricsLocation, htpasswd string, allowlist []string) []byte {
	var allow []string
	if len(allowlist) > 0 {
		allow = append(allow, "127.0.0.1", "::1")
	}
	for _, cidr := range allowlist {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			allow = append(allow, p.Masked().String())
		} else if a, err := netip.ParseAddr(cidr); err == nil {
			allow = append(allow, a.String())
		}
	}
	return execute("eru-metrics.conf.tmpl", map[string]any{"Locations": locations, "Htpasswd": htpasswd, "Allow": allow})
}

// ProbeArgs is the query string of a probe of target.
func ProbeArgs(target string) string { return "target=" + url.QueryEscape(target) }

// sdTarget is one entry of an http_sd target list.
type sdTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// TargetsJSON renders the Prometheus http_sd (and Grafana Alloy
// discovery.http) target list (Phase 4 D6): one entry per location with a
// __metrics_path__, so a scraper configured once follows db create and db
// drop. host is the node's hostname; locations without labels (targets.json
// itself) are left out.
func TargetsJSON(host string, locations []MetricsLocation) []byte {
	out := []sdTarget{}
	for _, l := range locations {
		if l.Labels == nil {
			continue
		}
		labels := map[string]string{"__metrics_path__": l.Path, "__scheme__": "https"}
		for k, v := range l.Labels {
			labels[k] = v
		}
		out = append(out, sdTarget{Targets: []string{host + ":443"}, Labels: labels})
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}
