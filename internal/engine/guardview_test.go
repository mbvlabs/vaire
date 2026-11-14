package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestGuardViewFailureBeforeExpansion(t *testing.T) {
	old := GuardView
	t.Cleanup(func() { GuardView = old })
	want := errors.New("guard refusal")
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, true)
	GuardView = func(got context.Context, cfg config.Cluster, opts Options) (config.Cluster, error) {
		if got.Value(key{}) != true || opts.DB != "target" {
			t.Fatal("context or selection lost")
		}
		return cfg, want
	}
	// Invalid step selection would otherwise fail during expansion.
	opts := Options{DB: "target", Only: []string{"missing"}}
	if _, err := Plan(ctx, nil, config.Cluster{}, Runtime{}, opts); !errors.Is(err, want) {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := Apply(ctx, nil, config.Cluster{}, Runtime{}, opts); !errors.Is(err, want) {
		t.Fatalf("apply: %v", err)
	}
}
