package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestDatabaseExposureDoesNotFollowNameReuse(t *testing.T) {
	old := config.Cluster{Clusters: []config.ClusterSpec{
		{Name: "app", Stanza: "app-123abc", ClientAllowlist: []string{"192.0.2.10/32"}},
	}}

	raw, err := json.Marshal(policyOf(old))
	if err != nil {
		t.Fatal(err)
	}

	var saved exposurePolicy
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}

	ceiling := saved.cluster()
	if increases := config.ExposureIncreases(ceiling, old); len(increases) != 0 {
		t.Fatalf("saved approval did not cover the original database: %v", increases)
	}

	old.Clusters[0].Stanza = "app-456def"
	if increases := config.ExposureIncreases(ceiling, old); len(increases) == 0 {
		t.Fatal("recreated database inherited approval")
	}
}

func TestGlobalApprovalDoesNotAuthorizeDatabaseExposure(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "cluster.toml")}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-123abc", ClientAllowlist: []string{"192.0.2.10/32"}}}

	prior := `{"Version":1,"Network":{"ClientAllowlist":["192.0.2.10/32"]}}`
	if err := os.WriteFile(store.Path+".exposure", []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(store.Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.WithExposure(cfg, func() error {
		t.Fatal("prior approval reached activation")

		return nil
	}); err == nil {
		t.Fatal("global approval was accepted for an individual database")
	}
}

func TestExposureApprovalCeiling(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "cluster.toml")}
	closed := config.DefaultCluster()
	closed.ClusterName = "fixture"
	closed.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "192.0.2.1", WGAddress: "10.42.0.1"}}
	opened := closed
	opened.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-123abc", ClientAllowlist: []string{"192.0.2.10/32"}}}

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
