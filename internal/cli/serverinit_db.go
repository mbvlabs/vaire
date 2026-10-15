package cli

import (
	"context"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/steps"
)

// callDatabasePhase runs cluster.bootstrap phase=database over WireGuard.
func (r *initRun) callDatabasePhase(ctx context.Context, caller remote.Caller) (node.BootstrapResult, error) {
	raw, err := r.nodeConfig().Encode()
	if err != nil {
		return node.BootstrapResult{}, err
	}
	// master-backup-v1 encrypts the master bundles to the recovery
	// recipient, with or without a bucket (Phase 5).
	if r.sec.Backup != nil && r.sec.Backup.Dumps.AgeRecipient != "" {
		if r.dbSecrets == nil {
			r.dbSecrets = map[string]string{}
		}
		r.dbSecrets[steps.SecretBackupAgeRecipient] = r.sec.Backup.Dumps.AgeRecipient
	}
	reply, err := r.p.call(ctx, caller, "cluster.bootstrap", map[string]any{
		"phase":        node.PhaseDatabase,
		"cluster_toml": string(raw),
		"node_name":    r.master().Name,
		"local":        r.local,
		"skip_doctor":  flagTrue(r.cmd, "skip-doctor"),
	}, remote.CallOptions{Secrets: r.dbSecrets, Confirm: r.name, OnEvent: r.p.event})
	if err != nil {
		return node.BootstrapResult{}, err
	}
	var res node.BootstrapResult
	if err := reply.Decode(&res); err != nil {
		return res, err
	}
	if res.Doctor != nil {
		r.health = healthLine(res.Doctor.Summary)
	}
	r.warnings = append(r.warnings, reply.Warnings...)
	return res, nil
}

// healthSuffix appends the doctor gate's summary to the init message.
func (r *initRun) healthSuffix() string {
	if r.health == "" {
		return ""
	}
	return "\n" + strings.ToUpper(r.health[:1]) + r.health[1:]
}

// dnsHelp is shown with the records: the record must point straight at the
// server.
const dnsHelp = "Set the records to DNS only (not proxied). Database clients can't connect through a proxy such as Cloudflare's orange cloud."

// dnsStep shows the DNS records, gives the operator a pause to create them,
// then checks the public resolvers once. A mismatch is a warning, never a
// stop: the certificate wait after the database phase shows whether the
// certificate was issued (D10).
func (r *initRun) dnsStep(ctx context.Context) error {
	ipv4, ipv6 := r.master().PublicAddress, r.prog.PublicIPv6
	records := dnsRecords(r.cfg.MasterHostname(), ipv4, ipv6)
	if len(records) == 0 {
		return nil
	}
	cmd := r.cmd
	// The records were shown and confirmed after first contact unless this
	// is a resume or host bootstrap reported another IPv6 address.
	if !r.dnsShown || r.dnsIPv6 != ipv6 {
		if err := r.askDNS(ipv4, ipv6, false); err != nil {
			return err
		}
	}
	if flagChanged(cmd, "skip-dns-check") {
		r.warnings = append(r.warnings, output.Warning{Code: "flag_deprecated", Message: "--skip-dns-check is no longer needed: a DNS mismatch is only a warning"})
	}

	r.p.phase("init.dns", "started", "ask "+strings.Join(resolverNames(), " and ")+" for "+r.cfg.MasterHostname())
	check := checkDNS(ctx, r.cfg.MasterHostname(), ipv4, ipv6)
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
			Message: r.cfg.MasterHostname() + " is proxied by Cloudflare: database clients can't connect through it. Set the record to DNS only (grey cloud)."})
	default:
		r.warnings = append(r.warnings, output.Warning{Code: "dns_mismatch",
			Message: r.cfg.MasterHostname() + " does not resolve to the server yet; the certificate is issued once it does"})
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
	r.p.phase("init.tls", "started", "wait for the certificate for "+r.cfg.MasterHostname())
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
			t.Detail("still the placeholder certificate; Let's Encrypt validates " + r.cfg.MasterHostname() + " over HTTP on port 80")
		}
		if time.Now().After(deadline) {
			check := checkDNS(ctx, r.cfg.MasterHostname(), r.master().PublicAddress, r.prog.PublicIPv6)
			msg := "the certificate is still pending; eru retries every hour, so it is issued within the hour after DNS points at the server"
			if check.Proxied {
				msg += ". " + r.cfg.MasterHostname() + " is proxied by Cloudflare: set it to DNS only (grey cloud)"
			} else if !check.Matches {
				msg += ". DNS answers: " + dnsSummary(check, r.master().PublicAddress, r.prog.PublicIPv6)
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
	reply, err := r.p.call(ctx, caller, "tls.status", nil, remote.CallOptions{OnEvent: r.p.event})
	if err != nil {
		return nil, err
	}
	var state node.TLSState
	if err := reply.Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

// confirmDNSEarly shows the DNS records as soon as first contact knows the
// server's addresses and asks the operator to confirm them, so the records
// propagate while the host phase runs (D10).
func (r *initRun) confirmDNSEarly() error {
	if r.cfg.TLS.Mode != "acme" || r.prog.Done(phaseDNS) || (r.restore != nil && !r.restore.addressChanged()) {
		return nil
	}
	return r.askDNS(r.master().PublicAddress, r.prog.PublicIPv6, true)
}

// askDNS shows the records in one panel and waits for 'confirmed', unless
// eru must not prompt or --dns-ready says they exist.
func (r *initRun) askDNS(ipv4, ipv6 string, early bool) error {
	records := dnsRecords(r.cfg.MasterHostname(), ipv4, ipv6)
	if len(records) == 0 {
		return nil
	}
	title := "Create this DNS record"
	if len(records) > 1 {
		title = "Create these DNS records"
	}
	body := recordLines(records)
	if ipv6 == "" {
		body = append(body, "", "No AAAA record: the server has no public IPv6 address.")
	}
	body = append(body, "", dnsHelp)
	if early {
		body = append(body, "eru checks that they resolve after it sets up the host, before the database.")
	}
	r.p.panel(title, body)
	r.dnsShown, r.dnsIPv6 = true, ipv6
	cmd := r.cmd
	if MustNotPrompt(cmd) || flagTrue(cmd, "dns-ready") {
		return nil
	}
	prompt := "Type 'confirmed' once the A record exists"
	if ipv6 != "" {
		prompt = "Type 'confirmed' once the A and AAAA records exist"
	}
	return typeWord(cmd, prompt, "", "confirmed")
}
