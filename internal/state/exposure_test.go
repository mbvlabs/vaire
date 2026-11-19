package state

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestExposureApprovalCeiling(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "cluster.toml")}
	closed := config.DefaultCluster()
	closed.ClusterName = "fixture"
	closed.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "192.0.2.1", WGAddress: "10.42.0.1"}}
	opened := closed
	opened.Network.ExposeSessionPooler = true

	save := func(cfg config.Cluster, auth *exposureAuthorization) error {
		t.Helper()

		raw, err := cfg.Encode()
		if err != nil {
			t.Fatal(err)
		}

		return store.save(raw, config.ScopeHost, auth)
	}
	activate := func(cfg config.Cluster) error {
		t.Helper()

		return store.WithExposure(cfg, func() error { return nil })
	}

	// The default is safe even before a config has been written.
	if err := activate(closed); err != nil {
		t.Fatal(err)
	}

	if err := save(opened, nil); err == nil {
		t.Fatal("unguarded bootstrap persisted a widening")
	}

	if err := activate(opened); err == nil {
		t.Fatal("unguarded activation succeeded")
	}

	if err := save(closed, nil); err != nil {
		t.Fatal(err)
	}

	rev, err := store.Revision()
	if err != nil {
		t.Fatal(err)
	}

	refusal := errors.New("invalid factor")

	auth := &exposureAuthorization{base: rev, verify: func() error { return refusal }}
	if err := save(opened, auth); !errors.Is(err, refusal) {
		t.Fatalf("factor refusal: %v", err)
	}

	if err := activate(opened); err == nil {
		t.Fatal("wrong factor authorized activation")
	}

	auth.verify = func() error { return nil }
	if err := save(opened, auth); err != nil {
		t.Fatal(err)
	}

	if err := activate(opened); err != nil {
		t.Fatalf("deferred activation: %v", err)
	}

	if err := save(opened, auth); err == nil {
		t.Fatal("stale revision was accepted")
	}

	if err := save(closed, nil); err != nil {
		t.Fatal(err)
	}

	if err := activate(opened); err == nil {
		t.Fatal("stale staged config activated after narrowing")
	}

	if err := save(opened, nil); err == nil {
		t.Fatal("previous approval silently reopened the pooler")
	}
}
