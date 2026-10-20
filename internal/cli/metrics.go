package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

// metricsPathInfo is one path the endpoint serves.
type metricsPathInfo struct {
	Path      string            `json:"path"`
	Component string            `json:"component"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// metricsShowData is metrics.show's result, plus the operator's side.
type metricsShowData struct {
	Enabled     bool              `json:"enabled"`
	Hostname    string            `json:"hostname"`
	PathPrefix  string            `json:"path_prefix"`
	TargetsPath string            `json:"targets_path"`
	Paths       []metricsPathInfo `json:"paths"`
	Allowlist   []string          `json:"allowlist"`
	Credential  bool              `json:"credential_installed"`
	BaseURL     string            `json:"base_url"`
	TargetsURL  string            `json:"targets_url"`
	Username    string            `json:"username,omitempty"`
	Password    string            `json:"password,omitempty"`
	Prometheus  string            `json:"prometheus_config"`
	Alloy       string            `json:"alloy_config"`
}

func metricsCommands() cmdSpec {
	return cmdSpec{
		use: "metrics",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Show or rotate the authenticated /metrics endpoint",
			WhenToUse: []string{"Point a Prometheus or Grafana Alloy at the server, or replace the endpoint's credential."},
			Examples:  []string{"vaire metrics show"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "show",
				run: runMetricsShow,
				flags: []fl{
					flagBool("reveal", "Print the endpoint's password from this machine's context"),
				},
				meta: CommandMeta{
					Summary:   "Show the /metrics paths, the service-discovery URL and scrape configs",
					WhenToUse: []string{"Configure an external scraper. The Prometheus and Alloy snippets use targets.json, so new databases appear without edits."},
					WhenNotToUse: []string{
						"Agents: never pass --reveal or print the password; the human copies it from the secrets handoff.",
					},
					Examples: []string{"vaire metrics show", "vaire metrics show --json --jq .data.prometheus_config"},
					Remote:   true,
					Next:     []string{"vaire doctor --only metrics"},
				},
			},
			{
				use: "rotate",
				run: runMetricsRotate,
				meta: CommandMeta{ //nolint:gosec // G101: help text, not a credential.
					Summary:        "Replace the /metrics credential",
					WhenToUse:      []string{"The endpoint's password leaked. Scrapers fail until they get the new one."},
					Examples:       []string{"vaire metrics rotate --confirm prod/metrics"},
					Mutating:       true,
					Destructive:    true,
					SupportsDryRun: true,
					Remote:         true,
					ConfirmToken:   "{server}/metrics",
					ErrorCodes:     []string{"unsafe_action_requires_confirmation", "input_required"},
					Next:           []string{"vaire secrets export", "vaire metrics show"},
				},
			},
		},
	}
}

func runMetricsShow(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "metrics.show", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data metricsShowData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	creds, err := contexts.LoadCredentials(s.ctx.Name)
	if err != nil {
		return err
	}
	data.BaseURL = metricsBaseURL(data.Hostname, data.PathPrefix)
	if data.Hostname != "" {
		data.TargetsURL = "https://" + data.Hostname + data.TargetsPath
	}
	user, password := contexts.MetricsUser, "<password from the secrets handoff>"
	if creds.Metrics != nil {
		data.Username = creds.Metrics.Username
		user = creds.Metrics.Username
		if flagTrue(cmd, "reveal") {
			data.Password = creds.Metrics.Password
			password = creds.Metrics.Password
		}
	}
	data.Prometheus = prometheusSnippet(s.ctx.Name, data.TargetsURL, user, password)
	data.Alloy = alloySnippet(s.ctx.Name, data.TargetsURL, user, password)
	lines := []string{fmt.Sprintf("/metrics on %s (user %s)", data.BaseURL, user)}
	if !data.Enabled {
		lines = []string{"/metrics is off ([metrics] enabled = false)"}
	}
	for _, path := range data.Paths {
		lines = append(lines, "  "+path.Path)
	}
	lines = append(lines, "", "service discovery: "+data.TargetsURL, "", "Prometheus:", indent(data.Prometheus), "", "Grafana Alloy:", indent(data.Alloy))
	if note := metricsPasswordNote(creds.Metrics != nil, flagTrue(cmd, "reveal")); note != "" {
		lines = append(lines, "", note)
	}
	if !data.Credential {
		lines = append(lines, "", "the node has no credential yet; run vaire apply")
	}
	warnings := append(s.warnings, reply.Warnings...)
	if creds.Metrics == nil {
		warnings = append(warnings, output.Warning{Code: "metrics_credential_missing",
			Message: "this machine's context has no /metrics credential; run vaire apply, then vaire secrets export"})
	}
	return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: warnings})
}

// metricsPasswordNote tells a human how to see the password the snippets
// leave out. It is empty when the password is shown or this machine has none.
func metricsPasswordNote(hasCredential, revealed bool) string {
	if !hasCredential || revealed {
		return ""
	}
	return "the password is hidden; vaire metrics show --reveal prints it, or copy it from the secrets handoff"
}

func indent(block string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// prometheusSnippet is a scrape_configs entry that follows targets.json.
func prometheusSnippet(server, targetsURL, user, password string) string {
	return fmt.Sprintf(`scrape_configs:
  - job_name: vaire-%s
    scheme: https
    basic_auth:
      username: %s
      password: %s
    http_sd_configs:
      - url: %s
        refresh_interval: 5m
        basic_auth:
          username: %s
          password: %s
`, server, user, password, targetsURL, user, password)
}

// alloySnippet is the Grafana Alloy equivalent.
func alloySnippet(server, targetsURL, user, password string) string {
	id := strings.ReplaceAll(server, "-", "_")
	return fmt.Sprintf(`discovery.http "vaire_%s" {
  url              = "%s"
  refresh_interval = "5m"
  basic_auth {
    username = "%s"
    password = "%s"
  }
}

prometheus.scrape "vaire_%s" {
  targets    = discovery.http.vaire_%s.targets
  scheme     = "https"
  basic_auth {
    username = "%s"
    password = "%s"
  }
  forward_to = [prometheus.remote_write.default.receiver]
}
`, id, targetsURL, user, password, id, id, user, password)
}

func runMetricsRotate(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	token := s.ctx.Server + "/metrics"
	if s.ctx.Server == "" {
		token = s.ctx.Name + "/metrics"
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]any{"dry_run": true}, Message: "dry run: would replace the /metrics credential", Warnings: s.warnings})
	}
	if err := Confirm(cmd, token); err != nil {
		return err
	}
	creds, err := contexts.LoadCredentials(s.ctx.Name)
	if err != nil {
		return err
	}
	old := creds.Metrics
	creds.Metrics = nil
	values := map[string]string{}
	if _, err := addMetricsSecret(&creds.Metrics, values); err != nil {
		return err
	}
	// Save before sending: a node holding a hash of a password the operator
	// lost would lock every scraper out until the next rotate.
	if err := contexts.SaveCredentials(s.ctx.Name, creds); err != nil {
		return err
	}
	reply, err := p.call(commandContext(cmd), s.node, "metrics.rotate", nil, remote.CallOptions{Secrets: values, Confirm: token, OnEvent: p.event})
	if err != nil {
		creds.Metrics = old
		_ = contexts.SaveCredentials(s.ctx.Name, creds)
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	warnings = append(warnings, output.Warning{Code: "secrets_document_stale",
		Message: "the /metrics credential changed; run vaire secrets export to refresh the secrets document, and update your scrapers"})
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"rotated": true, "username": creds.Metrics.Username},
		Message:  "replaced the /metrics credential; vaire metrics show --reveal prints the new password",
		Warnings: warnings,
		Next:     []string{"vaire secrets export", "vaire metrics show --reveal"},
	})
}
