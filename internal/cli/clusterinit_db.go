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

// dnsStep guides the operator through creating the DNS records and verifies
// them against public resolvers (D10).
func (r *initRun) dnsStep(ctx context.Context) error {
	records := dnsRecords(r.cfg.Domain, r.primary().PublicAddress, r.prog.PublicIPv6)
	if len(records) == 0 {
		return nil
	}
	r.p.phase("init.dns", "started", "confirm the DNS records for "+r.cfg.Domain)
	r.p.note("Create these DNS records:\n" + formatRecords(records))

	cmd := r.cmd
	if MustNotPrompt(cmd) {
		if !flagTrue(cmd, "dns-ready") {
			return output.NewError(
				output.CodeInputRequired,
				"the DNS records must exist before the database is set up",
				output.ExitInput,
				"Create the records, then re-run with --dns-ready.",
			).WithDetails(map[string]any{"flag": "--dns-ready", "dns_records": records})
		}
	} else {
		for {
			line, err := promptLine(cmd, "Type added once the records exist", "")
			if err != nil {
				return err
			}
			if line == "added" {
				break
			}
		}
	}

	for {
		ok, got := verifyDNS(ctx, r.cfg.Domain, r.primary().PublicAddress, r.prog.PublicIPv6)
		if ok {
			r.p.phase("init.dns", "completed", "DNS records resolve to the node")
			return nil
		}
		if flagTrue(cmd, "skip-dns-check") {
			r.warnings = append(r.warnings, output.Warning{Code: "tls_unverified", Message: "--skip-dns-check: continuing without a verified DNS record"})
			return nil
		}
		if MustNotPrompt(cmd) {
			return output.NewError(
				output.CodePrecondition,
				"the DNS records do not resolve to the node",
				output.ExitPrecondition,
				"Fix the records, or re-run with --skip-dns-check.",
			).WithDetails(map[string]any{"reason": "dns_mismatch", "dns_records": records, "resolved": got})
		}
		line, err := promptLine(cmd, "DNS does not match yet: type wait, continue, or stop", "")
		if err != nil {
			return err
		}
		switch strings.ToLower(line) {
		case "continue":
			r.warnings = append(r.warnings, output.Warning{Code: "tls_unverified", Message: "continuing before DNS resolves; the certificate will be issued once it does"})
			return nil
		case "stop":
			return output.NewError(
				output.CodePrecondition,
				"stopped at the DNS step",
				output.ExitPrecondition,
				"Create the records, then run eru cluster init --resume.",
			).WithDetails(map[string]any{"dns_records": records, "resolved": got})
		default:
			// wait, or an empty answer: verifyDNS polls for another 5 minutes.
		}
	}
}

// waitForTLS polls tls.status until the served certificate is no longer the
// placeholder, or until the 120 s window passes (D10).
func (r *initRun) waitForTLS(ctx context.Context, caller remote.Caller) error {
	if r.cfg.TLS.Mode == "self-signed" {
		return nil
	}
	r.p.phase("init.tls", "started", "wait for the certificate for "+r.cfg.Domain)
	deadline := time.Now().Add(tlsWaitTimeout)
	for {
		state, err := r.tlsStatus(ctx, caller)
		if err != nil {
			return err
		}
		r.tlsState = state
		if state.State != "pending" {
			r.p.phase("init.tls", "completed", "certificate issued")
			return nil
		}
		if time.Now().After(deadline) {
			r.warnings = append(r.warnings, output.Warning{
				Code:    "tls_pending",
				Message: "the certificate is still pending; Caddy will issue it once DNS is correct",
			})
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
	r.p.note("Create these DNS records now:\n" + formatRecords(records) +
		"Setup continues meanwhile; you'll be asked to confirm before the database is set up.")
}
