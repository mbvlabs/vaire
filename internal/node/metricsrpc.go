package node

import (
	"context"
	"encoding/json"
	"os"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

// metricsPath is one path of the /metrics endpoint.
type metricsPath struct {
	Path      string            `json:"path"`
	Component string            `json:"component"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// metricsShowData is metrics.show's result: what the endpoint serves.
type metricsShowData struct {
	Enabled     bool          `json:"enabled"`
	Hostname    string        `json:"hostname"`
	PathPrefix  string        `json:"path_prefix"`
	TargetsPath string        `json:"targets_path"`
	Paths       []metricsPath `json:"paths"`
	Allowlist   []string      `json:"allowlist"`
	Credential  bool          `json:"credential_installed"`
}

// handleMetricsShow describes the /metrics endpoint as the node renders it.
func handleMetricsShow(_ context.Context, env *Env) (Result, error) {
	cfg := env.Cluster
	data := metricsShowData{
		Enabled:    cfg.Metrics.Enabled,
		Hostname:   steps.NodeHostname(cfg),
		PathPrefix: cfg.Metrics.PathPrefix,
		Allowlist:  cfg.Metrics.Allowlist,
		Paths:      []metricsPath{},
	}
	if _, err := os.Stat(steps.MetricsHtpasswdPath); err == nil {
		data.Credential = true
	}
	for _, l := range steps.MetricsLocations(cfg) {
		if l.Labels == nil {
			data.TargetsPath = l.Path
			continue
		}
		data.Paths = append(data.Paths, metricsPath{Path: l.Path, Component: l.Labels["vaire_component"], Labels: l.Labels})
	}
	var warnings []output.Warning
	if !cfg.Metrics.Enabled {
		warnings = append(warnings, output.Warning{Code: "metrics_disabled", Message: "[metrics] enabled is false; the exporters are stopped and /metrics answers 404"})
	}
	return Result{Data: data, Warnings: warnings}, nil
}

// metricsToken is vaire metrics rotate's confirm token: <server>/metrics.
func metricsToken(cfg config.Cluster, _ json.RawMessage) string { return cfg.Name() + "/metrics" }

// handleMetricsRotate replaces the /metrics credential's hash with the one
// the request carries (Phase 4 D5). nginx reads the file on every request,
// so the new password works at once and the old one stops.
func handleMetricsRotate(_ context.Context, env *Env) (Result, error) {
	if env.Req.DryRun {
		return Result{Data: map[string]any{"dry_run": true, "path": steps.MetricsHtpasswdPath}}, nil
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("rotating the /metrics credential locks every scraper out until it gets the new one", metricsToken(env.Cluster, nil))
	}
	rt := runtimeFor(env, false)
	if rt.SecretValues[steps.SecretMetricsHash] == "" {
		return Result{}, output.NewError(output.CodeUsage, "metrics.rotate needs the new credential's hash", output.ExitUsage, "")
	}
	if err := steps.WriteHtpasswd(rt); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"rotated": true, "path": steps.MetricsHtpasswdPath}}, nil
}
