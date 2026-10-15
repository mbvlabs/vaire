package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/doctor"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/spf13/cobra"
)

type doctorReply struct {
	Results []doctor.Result       `json:"results"`
	Summary map[doctor.Status]int `json:"summary"`
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	only := stringFlag(cmd, "only")
	if !doctor.ValidOnly(only) {
		return output.NewError(output.CodeUsage, "unknown --only "+only, output.ExitUsage, "Use one of "+strings.Join(doctor.Categories, ", ")+".")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	params := map[string]any{}
	if only != "" {
		params["only"] = only
	}
	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}
	reply, err := p.call(commandContext(cmd), s.node, "doctor.run", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data doctorReply
	if err := reply.Decode(&data); err != nil {
		return err
	}
	data.Results = append(data.Results, operatorChecks(commandContext(cmd), s, only)...)
	data.Summary = doctor.Summary(data.Results)
	message := doctorMessage(data.Results)
	warnings := append(s.warnings, reply.Warnings...)
	if failed := doctor.Failed(data.Results); len(failed) > 0 {
		ids := make([]string, 0, len(failed))
		for _, r := range failed {
			ids = append(ids, checkName(r))
		}
		// A human sees the whole report before the error; structured
		// output carries it in details.
		if opts, err := output.ParseOptions(cmd); err == nil && !output.UsesStructuredOutput(opts) && !opts.Quiet {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), message)
		}
		return output.NewError(output.CodeDoctorFailed, doctorFailedMessage(ids), output.ExitStep,
			"Run the fix shown under each failing check, then eru doctor again.").
			WithDetails(map[string]any{"checks": data.Results, "summary": data.Summary, "report": message})
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings})
}

// doctorFailedMessage is "1 doctor check fails: x" or "2 doctor checks
// fail: x, y".
func doctorFailedMessage(ids []string) string {
	if len(ids) == 1 {
		return "1 doctor check fails: " + ids[0]
	}
	return fmt.Sprintf("%d doctor checks fail: %s", len(ids), strings.Join(ids, ", "))
}

func checkName(r doctor.Result) string {
	if r.DB != "" {
		return r.ID + "@" + r.DB
	}
	return r.ID
}

// doctorMessage renders the results: problems first with their fix, then a
// count of what passed.
func doctorMessage(results []doctor.Result) string {
	var lines []string
	for _, want := range []doctor.Status{doctor.Fail, doctor.Warn} {
		for _, r := range results {
			if r.Status != want {
				continue
			}
			line := fmt.Sprintf("%-4s %-26s %s", strings.ToUpper(string(r.Status)), checkName(r), r.Detail)
			if r.Fix != "" {
				line += "\n     fix: " + r.Fix
			}
			lines = append(lines, line)
		}
	}
	s := doctor.Summary(results)
	lines = append(lines, fmt.Sprintf("%d pass · %d warn · %d fail · %d skip", s[doctor.Pass], s[doctor.Warn], s[doctor.Fail], s[doctor.Skip]))
	return strings.Join(lines, "\n")
}

// operatorChecks are the checks only the operator can run: its context's
// file modes, the node's eru version against this one, and an authenticated
// scrape of /metrics through the tunnel.
func operatorChecks(ctx context.Context, s *session, only string) []doctor.Result {
	var out []doctor.Result
	add := func(id, category string, security bool, status doctor.Status, detail, fix string) {
		if only != "" && only != category && (only != doctor.CatSecurity || !security) {
			return
		}
		out = append(out, doctor.Result{ID: id, Category: category, Security: security, Status: status, Detail: detail, Fix: fix})
	}
	if loose := looseContextFiles(s.ctx.Name); len(loose) > 0 {
		add("ctx.file_modes", doctor.CatHost, true, doctor.Warn, "readable by others: "+strings.Join(loose, ", "), "chmod 600 those files")
	} else {
		add("ctx.file_modes", doctor.CatHost, true, doctor.Pass, "the context's files are private", "")
	}
	add("eru.version_skew", doctor.CatHost, false, doctor.Pass, "the node runs this CLI's build (checked when the session opened)", "")
	status, detail, fix := metricsAuth(ctx, s)
	add("metrics.auth", doctor.CatMetrics, true, status, detail, fix)
	return out
}

// looseContextFiles lists files in the context directory readable by group
// or others.
func looseContextFiles(name string) []string {
	var out []string
	entries, err := os.ReadDir(contexts.Dir(name))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		if info.Mode().Perm()&0o077 != 0 {
			out = append(out, filepath.Join(contexts.Dir(name), e.Name()))
		}
	}
	return out
}

// metricsAuth scrapes /metrics/node with the context's credential over the
// tunnel, verifying the node's certificate as a scraper would.
func metricsAuth(ctx context.Context, s *session) (doctor.Status, string, string) {
	creds, err := contexts.LoadCredentials(s.ctx.Name)
	if err != nil || creds.Metrics == nil {
		return doctor.Warn, "this context has no /metrics credential", "eru apply, then eru secrets export"
	}
	n, ok := s.ctx.Master()
	if !ok || n.Hostname == "" {
		return doctor.Skip, "no hostname for the node", ""
	}
	code, err := scrapeMetrics(ctx, s.dialer, n.WGAddress, n.Hostname, s.ctx.TLSMode, creds.Metrics)
	switch {
	case err != nil:
		return doctor.Warn, "scraping /metrics/node failed: " + err.Error(), "eru doctor --only metrics"
	case code == http.StatusOK:
		return doctor.Pass, "the credential scrapes https://" + n.Hostname + "/metrics/node", ""
	case code == http.StatusUnauthorized:
		return doctor.Fail, "the node refuses this context's credential", "eru metrics rotate --confirm " + s.ctx.Server + "/metrics"
	case code == http.StatusNotFound:
		return doctor.Skip, "/metrics is off on the node", ""
	}
	return doctor.Warn, fmt.Sprintf("/metrics/node answered %d", code), ""
}

func scrapeMetrics(ctx context.Context, dialer transport.Dialer, wgAddr, hostname, tlsMode string, creds *contexts.MetricsCreds) (int, error) {
	if dialer == nil {
		return 0, fmt.Errorf("no tunnel")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: hostname, InsecureSkipVerify: tlsMode == "self-signed", MinVersion: tls.VersionTLS12}, //nolint:gosec // G402: self-signed mode has nothing to verify against.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(wgAddr, "443"))
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+hostname+"/metrics/node", nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(creds.Username, creds.Password)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// applyReplyData is apply's result with the doctor gate (Phase 4 step 18).
type applyReplyData struct {
	engine.ApplyData
	Doctor *doctorReply `json:"doctor,omitempty"`
}

// healthLine is the one-line doctor summary apply and server init print.
func healthLine(s map[doctor.Status]int) string {
	line := fmt.Sprintf("health: %d pass, %d warn, %d fail", s[doctor.Pass], s[doctor.Warn], s[doctor.Fail])
	if s[doctor.Warn]+s[doctor.Fail] > 0 {
		line += " (eru doctor)"
	}
	return line
}
