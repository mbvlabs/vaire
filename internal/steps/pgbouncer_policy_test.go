package steps

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestPoolerOperatorsFailClosed(t *testing.T) {
	old := loadPoolerOperators

	t.Cleanup(func() { loadPoolerOperators = old })

	cfg := config.Cluster{Transport: config.Transport{Subnet: "10.42.0.0/16"}}

	for _, tc := range []struct {
		name string
		ops  config.Operators
		err  error
		want bool
	}{
		{"registered", config.Operators{Operators: []config.Operator{{WGAddress: "10.42.255.1"}}}, nil, true},
		{"outside", config.Operators{Operators: []config.Operator{{WGAddress: "192.0.2.1"}}}, nil, false},
		{"injection", config.Operators{Operators: []config.Operator{{WGAddress: "10.42.255.1/0"}}}, nil, false},
		{"unreadable", config.Operators{}, errors.New("unreadable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loadPoolerOperators = func() (config.Operators, error) { return tc.ops, tc.err }

			peers, err := poolerOperators(cfg)
			if (err == nil) != tc.want || (tc.want && (len(peers) != 1 || peers[0] != "10.42.255.1")) {
				t.Fatalf("peers %v, error %v", peers, err)
			}
		})
	}

	loadPoolerOperators = func() (config.Operators, error) {
		return config.Operators{Operators: []config.Operator{{WGAddress: "10.42.255.1"}}}, nil
	}

	cfg.Transport.Subnet = "invalid"
	if _, err := poolerOperators(cfg); err == nil {
		t.Fatal("invalid subnet accepted")
	}
}

func TestPoolerWritersRequireExposureGuard(t *testing.T) {
	old := poolerExposure

	t.Cleanup(func() { poolerExposure = old })

	refusal := errors.New("not approved")
	poolerExposure = func(config.Cluster, func() error) error { return refusal }
	ctx := context.Background()

	cfg := config.Cluster{}
	if _, err := (pgbouncer{}).Apply(ctx, cfg, engine.Runtime{}, nil); !errors.Is(err, refusal) {
		t.Fatalf("apply: %v", err)
	}

	if err := ApplyPoolerFence(ctx, cfg, engine.Runtime{}, nil); !errors.Is(err, refusal) {
		t.Fatalf("fence: %v", err)
	}

	if err := PublishFollowerRoutes(ctx, cfg, engine.Runtime{}, "app", nil); !errors.Is(err, refusal) {
		t.Fatalf("publish: %v", err)
	}
}
