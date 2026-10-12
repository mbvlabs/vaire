package cli

import (
	"context"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
)

// callDatabasePhase runs cluster.bootstrap phase=database over WireGuard.
func (r *initRun) callDatabasePhase(ctx context.Context, caller remote.Caller) (node.BootstrapResult, error) {
	raw, err := r.cfg.Encode()
	if err != nil {
		return node.BootstrapResult{}, err
	}
	reply, err := caller.Call(ctx, "cluster.bootstrap", map[string]any{
		"phase":        node.PhaseDatabase,
		"cluster_toml": string(raw),
		"node_name":    r.primary().Name,
		"local":        r.local,
	}, remote.CallOptions{Secrets: r.dbSecrets, Confirm: r.name, OnEvent: r.p.event})
	if err != nil {
		return node.BootstrapResult{}, err
	}
	var res node.BootstrapResult
	if err := reply.Decode(&res); err != nil {
		return res, err
	}
	r.warnings = append(r.warnings, reply.Warnings...)
	return res, nil
}

// dnsHelp is shown with the records: the record must point straight at the
// server.
const dnsHelp = "Set the records to DNS only (not proxied). Database clients can't connect through a proxy such as Cloudflare's orange cloud."

// dnsStep shows the DNS records, gives the operator a pause to create them,
// then checks the public resolvers once. A mismatch is a warning, never a
// stop: the certificate wait after the database phase shows whether the
// certificate was issued (D10).
func (r *initRun) dnsStep(ctx context.Context) error {
	ipv4, ipv6 := r.primary().PublicAddress, r.prog.PublicIPv6
	records := dnsRecords(r.cfg.Domain, ipv4, ipv6)
	if len(records) == 0 {
		return nil
	}
	cmd := r.cmd
	prompt := "Type 'confirmed' once the records exist"
	switch {
	case !r.dnsNoted:
		body := recordLines(records)
		if ipv6 == "" {
			body = append(body, "", "No AAAA record: the server has no public IPv6 address.")
		}
		r.p.panel("Create these DNS records", append(body, "", dnsHelp))
	case ipv6 != "":
		// The A record and the help were shown before the host phase.
		r.p.panel("Add the AAAA record too", recordLines(dnsRecords(r.cfg.Domain, "", ipv6)))
		prompt = "Type 'confirmed' once the A and AAAA records exist"
	default:
		prompt = "Type 'confirmed' once the A record exists"
	}
	if !MustNotPrompt(cmd) && !flagTrue(cmd, "dns-ready") {
		if err := typeWord(cmd, prompt, "", "confirmed"); err != nil {
			return err
		}
	}
	if flagChanged(cmd, "skip-dns-check") {
		r.warnings = append(r.warnings, output.Warning{Code: "flag_deprecated", Message: "--skip-dns-check is no longer needed: a DNS mismatch is only a warning"})
	}

	r.p.phase("init.dns", "started", "ask "+strings.Join(resolverNames(), " and ")+" for "+r.cfg.Domain)
	check := checkDNS(ctx, r.cfg.Domain, ipv4, ipv6)
	lines := make([]string, 0, len(check.Resolvers))
	for _, a := range check.Resolvers {
		line, ok := a.describe(ipv4, ipv6)
		mark := "ok"
		if !ok {
			mark = "mismatch"
		}
		lines = append(lines, line+"  ["+mark+"]")
	}
	switch {
	case check.Matches:
		r.p.phase("init.dns", "completed", "the records resolve to the server")
		return nil
	case check.Proxied:
		r.warnings = append(r.warnings, output.Warning{Code: "dns_proxied",
			Message: r.cfg.Domain + " is proxied by Cloudflare: database clients can't connect through it. Set the record to DNS only (grey cloud)."})
	default:
		r.warnings = append(r.warnings, output.Warning{Code: "dns_mismatch",
			Message: r.cfg.Domain + " does not resolve to the server yet; the certificate is issued once it does"})
	}
	r.p.phase("init.dns", "completed", "not resolving to the server yet; continuing")
	r.p.panel("DNS answers", lines)
	if check.Proxied {
		r.p.warn("the record is proxied by Cloudflare; set it to DNS only (grey cloud)")
	}
	return nil
}

func resolverNames() []string {
	out := make([]string, 0, len(publicResolvers))
	for _, s := range publicResolvers {
		out = append(out, strings.TrimSuffix(s, ":53"))
	}
	return out
}

// waitForTLS polls tls.status until the served certificate is no longer the
// placeholder, or until the 120 s window passes (D10). Pending is a warning
// that carries the DNS answers, never a failure.
func (r *initRun) waitForTLS(ctx context.Context, caller remote.Caller) error {
	if r.cfg.TLS.Mode == "self-signed" {
		return nil
	}
	r.p.phase("init.tls", "started", "wait for the certificate for "+r.cfg.Domain)
	deadline := time.Now().Add(tlsWaitTimeout)
	if t := r.p.task("init.tls"); t != nil {
		t.Deadline(deadline)
	}
	for {
		state, err := r.tlsStatus(ctx, caller)
		if err != nil {
			return err
		}
		r.tlsState = state
		if state.State != "pending" {
			detail := "issued"
			if state.NotAfter != "" {
				detail += ", valid until " + state.NotAfter
			}
			r.p.phase("init.tls", "completed", detail)
			return nil
		}
		if t := r.p.task("init.tls"); t != nil {
			t.Detail("still the placeholder certificate; Let's Encrypt validates " + r.cfg.Domain + " over HTTP on port 80")
		}
		if time.Now().After(deadline) {
			check := checkDNS(ctx, r.cfg.Domain, r.primary().PublicAddress, r.prog.PublicIPv6)
			msg := "the certificate is still pending; eru retries every hour, so it is issued within the hour after DNS points at the server"
			if check.Proxied {
				msg += ". " + r.cfg.Domain + " is proxied by Cloudflare: set it to DNS only (grey cloud)"
			} else if !check.Matches {
				msg += ". DNS answers: " + dnsSummary(check, r.primary().PublicAddress, r.prog.PublicIPv6)
			}
			r.warnings = append(r.warnings, output.Warning{Code: "tls_pending", Message: msg})
			r.p.phase("init.tls", "completed", "still pending")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func dnsSummary(check dnsCheck, ipv4, ipv6 string) string {
	parts := make([]string, 0, len(check.Resolvers))
	for _, a := range check.Resolvers {
		line, _ := a.describe(ipv4, ipv6)
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}

// tlsStatus asks the node for the served certificate's state.
func (r *initRun) tlsStatus(ctx context.Context, caller remote.Caller) (*node.TLSState, error) {
	reply, err := caller.Call(ctx, "tls.status", nil, remote.CallOptions{OnEvent: r.p.event})
	if err != nil {
		return nil, err
	}
	var state node.TLSState
	if err := reply.Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

// noteDNSEarly shows the A record as soon as the answers are known, so the
// operator can create it while the host phase runs (D10). The AAAA record, if
// any, is known only after host bootstrap and is shown at the DNS step.
func (r *initRun) noteDNSEarly() {
	if r.cfg.TLS.Mode != "acme" || r.prog.Done(phaseDNS) || r.prog.Done(phaseHost) {
		return
	}
	records := dnsRecords(r.cfg.Domain, r.primary().PublicAddress, "")
	if len(records) == 0 {
		return
	}
	r.dnsNoted = true
	r.p.panel("Create this DNS record now", append(recordLines(records), "",
		"If the server has a public IPv6 address, eru shows its AAAA record once the host is up.",
		"", dnsHelp,
		"eru keeps working while you create the record, then pauses for you to confirm the records exist before it sets up the database."))
}
