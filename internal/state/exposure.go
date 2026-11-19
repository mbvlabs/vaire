package state

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// Keep the approval ceiling local to the node, outside the master bundle.
type exposurePolicy struct {
	Version int
	Network config.Network
	Metrics config.Metrics
	Subnet  string
}

func policyOf(cfg config.Cluster) exposurePolicy {
	return exposurePolicy{Version: 1, Network: cfg.Network, Metrics: cfg.Metrics, Subnet: cfg.Transport.Subnet}
}

func (p exposurePolicy) cluster() config.Cluster {
	return config.Cluster{Network: p.Network, Metrics: p.Metrics, Transport: config.Transport{Subnet: p.Subnet}}
}

func (s FileStore) exposureCeiling() (config.Cluster, error) {
	raw, err := os.ReadFile(s.path() + ".exposure")
	if os.IsNotExist(err) {
		return config.DefaultCluster(), nil
	}

	if err != nil {
		return config.Cluster{}, err
	}

	var p exposurePolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return config.Cluster{}, err
	}

	// Earlier edge approvals may cover only one pooler. Never interpret
	// one of those approvals as permission to open both.
	if p.Version != 1 {
		return config.DefaultCluster(), nil
	}

	return p.cluster(), nil
}

func (s FileStore) exposureCurrent() (config.Cluster, string, error) {
	raw, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		return config.DefaultCluster(), "", nil
	}

	if err != nil {
		return config.Cluster{}, "", err
	}

	cfg, err := config.ParseCluster(raw)

	return cfg, RevisionOf(raw), err
}

func unapprovedExposure(surfaces []string) error {
	return output.NewError(output.CodePrecondition, "network exposure is not approved on this node", output.ExitPrecondition,
		"Use an interactive vaire config set or config edit to review and approve the change. Start fresh nodes with public metrics off and no public database clients.").
		WithDetails(map[string]any{"reason": "exposure_unapproved", "exposure_increases": surfaces})
}

// WithExposure checks the desired config and node-local approval under the
// same lock used by writers, until the live change has finished.
func (s FileStore) WithExposure(cfg config.Cluster, apply func() error) error {
	return engine.WithFileLock(s.path()+".exposure.lock", func() error {
		current, _, err := s.exposureCurrent()
		if err != nil {
			return err
		}

		ceiling, err := s.exposureCeiling()
		if err != nil {
			return err
		}

		for _, baseline := range []config.Cluster{current, ceiling} {
			if increases := config.ExposureIncreases(baseline, cfg); len(increases) > 0 {
				return unapprovedExposure(increases)
			}
		}

		return apply()
	})
}

// SaveHost is used only before full bootstrap validation is possible.
func (s FileStore) SaveHost(raw []byte) error {
	return s.save(raw, config.ScopeHost, nil)
}

// SaveApproved verifies an exact proposal before persisting it. authorize
// must verify a human factor; it is called only if the proposal widens access.
func (s FileStore) SaveApproved(raw []byte, base string, authorize func() error) error {
	if authorize == nil {
		return errors.New("missing exposure authorizer")
	}

	return s.save(raw, config.ScopeFull, &exposureAuthorization{base: base, verify: authorize})
}

type exposureAuthorization struct {
	base   string
	verify func() error
}

func (s FileStore) saveExposure(cfg config.Cluster, auth *exposureAuthorization) error {
	current, revision, err := s.exposureCurrent()
	if err != nil {
		return err
	}

	ceiling, err := s.exposureCeiling()
	if err != nil {
		return err
	}

	if auth != nil && revision != auth.base {
		return output.NewError(output.CodePrecondition, "configuration changed before saving", output.ExitPrecondition,
			"Review the updated configuration and try again.").WithDetails(map[string]any{"reason": "config_changed"})
	}

	increases := config.ExposureIncreases(current, cfg)
	if len(increases) == 0 {
		increases = config.ExposureIncreases(ceiling, cfg)
	}

	if len(increases) == 0 {
		return nil
	}

	if auth == nil {
		return unapprovedExposure(increases)
	}

	if err := auth.verify(); err != nil {
		return err
	}

	raw, err := json.Marshal(policyOf(cfg))
	if err != nil {
		return err
	}

	// Write the ceiling first. If the config write fails, activation
	// still cannot exceed the intersection with the old desired state.
	return engine.WriteFileAtomic(s.path()+".exposure", raw, 0o600)
}
