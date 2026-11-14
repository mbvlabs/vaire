package node

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

// bootstrapDatabase runs cluster.bootstrap phase=database: steps 7-13 (§11).
var bootstrapLoadNode = config.LoadNode

func bootstrapDatabase(ctx context.Context, env *Env, p bootstrapParams) (Result, error) {
	cfg, err := config.ParseCluster([]byte(p.ClusterTOML))
	if err != nil {
		return Result{}, output.NewError(output.CodeConfig, "cluster_toml does not parse: "+err.Error(), output.ExitConfig, "")
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, config.ValidationError(err)
	}
	nodeCfg, err := bootstrapLoadNode(config.NodePath)
	if err != nil {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"this server is not an vaire node yet",
			output.ExitPrecondition,
			"Run the host phase first with vaire server init.",
		).WithDetails(map[string]any{"reason": "not_a_node"})
	}
	if nodeCfg.Server != cfg.Name() {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"this node belongs to server "+nodeCfg.Server,
			output.ExitPrecondition,
			"Use that server's own secrets document.",
		).WithDetails(map[string]any{"reason": "node_belongs_to_other_server", "server": nodeCfg.Server})
	}
	if nodeCfg.NodeName != p.NodeName {
		return Result{}, output.NewError(output.CodePrecondition, "bootstrap node identity does not match", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "bootstrap_ownership_unavailable"})
	}
	if err := bootstrapDir(false); err != nil {
		return Result{}, err
	}
	owner, err := bootstrapReadOwnership()
	owned := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, output.NewError(output.CodePrecondition, "bootstrap ownership is unavailable", output.ExitPrecondition, "Use an exact database access confirmation through normal apply.").
			WithDetails(map[string]any{"reason": "bootstrap_ownership_unavailable"})
	}
	if owned {
		if err := bootstrapValidateOwnership(owner, cfg, p.NodeName, env.Req.OperationID); err != nil {
			return Result{}, output.NewError(output.CodePrecondition, "bootstrap ownership does not match this setup", output.ExitPrecondition, "Use an exact database access confirmation through normal apply.").
				WithDetails(map[string]any{"reason": "bootstrap_ownership_unavailable"})
		}
	} else {
		visible := cfg
		if engine.ClusterView != nil {
			visible = engine.ClusterView(cfg, "")
		}
		for _, spec := range visible.Clusters {
			if !spec.IsFollower() {
				return Result{}, output.NewError(output.CodePrecondition, "bootstrap has no authority over existing database "+spec.Name, output.ExitPrecondition, "Use an exact database access confirmation through normal apply.").
					WithDetails(map[string]any{"reason": "bootstrap_ownership_unavailable", "database": spec.Name})
			}
		}
	}
	if err := bootstrapExistingConfigMatches(cfg, p.NodeName); err != nil {
		return Result{}, output.NewError(output.CodePrecondition, "bootstrap configuration changed since ownership was established", output.ExitPrecondition, "Use an exact database access confirmation through normal apply.").
			WithDetails(map[string]any{"reason": "bootstrap_ownership_unavailable"})
	}
	if !hostPhaseComplete() {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"the host phase has not finished on this node",
			output.ExitPrecondition,
			"Re-run vaire server init --resume.",
		).WithDetails(map[string]any{"reason": "host_phase_incomplete"})
	}

	raw, err := cfg.Encode()
	if err != nil {
		return Result{}, err
	}
	if err := (state.FileStore{}).Save(raw); err != nil {
		return Result{}, err
	}
	if err := syncNodeHostname(cfg); err != nil {
		return Result{}, err
	}
	env.Cluster = cfg

	var ownerPtr *bootstrapOwnership
	var complete func(string) error
	if owned {
		ownerPtr = &owner
		complete = func(target string) error {
			return bootstrapCompleteTarget(&owner, cfg, p.NodeName, env.Req.OperationID, target)
		}
	}
	data, warnings, err := bootstrapDatabaseSteps(ctx, cfg, runtimeFor(env, false), env.Report,
		steps.Catalog(), engine.Apply, steps.Restarter(env.Cluster), initFailureHint(cfg.ClusterName, p.Local), ownerPtr, complete)
	if err != nil {
		return Result{}, err
	}
	res := BootstrapResult{
		Phase:            PhaseDatabase,
		Node:             nodeCfg.NodeName,
		Steps:            data,
		TLS:              inspectTLS(cfg),
		RestartPerformed: data.Restarted(),
	}
	// Step 18: doctor gates init (Phase 4 D10).
	if !p.SkipDoctor {
		gate, _, err := doctorGate(ctx, cfg, runtimeFor(env, true), true)
		if err != nil {
			return Result{}, err
		}
		res.Doctor = gate
	}
	return Result{Data: res, Warnings: warnings}, nil
}

// handleTLSStatus reports the served certificate's state, so init can wait for
// the real certificate without re-running the database phase.
func handleTLSStatus(_ context.Context, env *Env) (Result, error) {
	return Result{Data: inspectTLS(env.Cluster)}, nil
}

// hostPhaseComplete reports whether every host step has a completed journal
// entry.
func hostPhaseComplete() bool {
	j, err := engine.LoadJournal(engine.JournalPath)
	if err != nil {
		return false
	}
	for _, id := range steps.HostStepIDs() {
		if entry, ok := j.Steps[id]; !ok || entry.Status != engine.StatusCompleted {
			return false
		}
	}
	return true
}

// inspectTLS reports the certificate state after the database phase (D10).
func inspectTLS(cfg config.Cluster) *TLSState {
	if cfg.TLS.Mode == "self-signed" {
		return &TLSState{State: "self-signed", NotAfter: certNotAfter(steps.TLSCertPath)}
	}
	if isSelfSigned(steps.TLSCertPath) {
		return &TLSState{State: "pending"}
	}
	return &TLSState{State: "acme", NotAfter: certNotAfter(steps.TLSCertPath)}
}

// isSelfSigned reports whether the certificate at path is self-signed, which
// in acme mode means the placeholder is still served.
func isSelfSigned(path string) bool {
	cert := readCert(path)
	if cert == nil {
		return true
	}
	return isSelfSignedCert(cert)
}

func isSelfSignedCert(cert *x509.Certificate) bool {
	return cert.Issuer.String() == cert.Subject.String()
}

func certNotAfter(path string) string {
	cert := readCert(path)
	if cert == nil {
		return ""
	}
	return cert.NotAfter.UTC().Format(time.RFC3339)
}

func readCert(path string) *x509.Certificate {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed certificate path.
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}
