package node

import (
	"context"
	"os"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/totp"
)

func configDeclarationCfg() config.Cluster {
	cfg := standaloneExtensionCfg()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s", Protected: true},
		{Name: "keep", Node: "db1", Port: 5434, Stanza: "keep-abc123", Size: "small", StatementTimeout: "60s"},
	}

	return cfg
}

func TestConfigSetRefusesDatabaseDeclarationRemoval(t *testing.T) {
	cfg := configDeclarationCfg()

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	store, path := useNodeConfigStore(t, raw)

	revision, err := store.Revision()
	if err != nil {
		t.Fatal(err)
	}

	newExtHarness(t, cfg)

	guard, clock := newTestGuard(t)
	oldGuard := defaultProtectGuard
	defaultProtectGuard = guard

	t.Cleanup(func() { defaultProtectGuard = oldGuard })

	secret := setUp(t, guard, clock)

	before, err := guard.load()
	if err != nil {
		t.Fatal(err)
	}

	clock.t = clock.t.Add(totp.Period)

	updated := cfg.Clone()
	updated.Clusters = updated.Clusters[1:]

	updatedRaw, err := updated.Encode()
	if err != nil {
		t.Fatal(err)
	}

	req := request("config.set", map[string]any{"toml": string(updatedRaw), "base_revision": revision})
	req.Secrets = map[string]string{ProtectCodeSecret: codeFor(t, secret, clock.t, 0)}

	_, refusalErr := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg})
	if refusalErr == nil || extReason(t, refusalErr) != "database_drop_required" {
		t.Fatalf("database declaration removal = %v", refusalErr)
	}

	if details := extDetails(t, refusalErr); details["database"] != "app" {
		t.Fatalf("database declaration removal details = %+v", details)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("refused declaration removal changed the file: %v", err)
	}

	after, err := guard.load()
	if err != nil {
		t.Fatal(err)
	}

	if after.LastStep != before.LastStep || after.Failures != before.Failures {
		t.Fatalf("refused declaration removal consumed protection state: before=%+v after=%+v", before, after)
	}

	if recs, err := backup.LoadRecords(); err != nil || len(recs.ExtensionRequirements) != 0 {
		t.Fatalf("refused declaration removal wrote history: %+v, %v", recs.ExtensionRequirements, err)
	}
}

func TestConfigSetRefusesFollowerDeclarationRemoval(t *testing.T) {
	cfg := extFollowerCfg(t)

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	store, path := useNodeConfigStore(t, raw)

	revision, err := store.Revision()
	if err != nil {
		t.Fatal(err)
	}

	newExtHarness(t, cfg)

	updated := cfg.Clone()
	updated.Clusters = []config.ClusterSpec{cfg.Clusters[0], cfg.Clusters[1]}

	updatedRaw, err := updated.Encode()
	if err != nil {
		t.Fatal(err)
	}

	req := request("config.set", map[string]any{"toml": string(updatedRaw), "base_revision": revision})

	_, refusalErr := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg})
	if refusalErr == nil || extReason(t, refusalErr) != "database_drop_required" {
		t.Fatalf("follower declaration removal = %v", refusalErr)
	}

	if details := extDetails(t, refusalErr); details["database"] != "app_follower_1" {
		t.Fatalf("follower declaration removal details = %+v", details)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("refused follower declaration removal changed the file: %v", err)
	}
}

func TestConfigSetOmissionCannotBypassExtensionRemoval(t *testing.T) {
	cfg := standaloneExtensionCfg()
	cfg.Clusters[0].Extensions = []string{"pg_stat_statements", "pg_trgm"}

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	store, path := useNodeConfigStore(t, raw)

	revision, err := store.Revision()
	if err != nil {
		t.Fatal(err)
	}

	newExtHarness(t, cfg)

	omitted := cfg.Clone()
	omitted.Clusters = nil

	omittedRaw, err := omitted.Encode()
	if err != nil {
		t.Fatal(err)
	}

	req := request("config.set", map[string]any{"toml": string(omittedRaw), "base_revision": revision})
	if _, err := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || extReason(t, err) != "database_drop_required" {
		t.Fatalf("first save omitting the declaration = %v", err)
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) {
		t.Fatalf("first save changed the file: %v", err)
	}

	readded := cfg.Clone()
	readded.Clusters[0].Extensions = nil

	readdedRaw, err := readded.Encode()
	if err != nil {
		t.Fatal(err)
	}

	req = request("config.set", map[string]any{"toml": string(readdedRaw), "base_revision": revision})
	if _, err := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || extReason(t, err) != "extension_remove_unsupported" {
		t.Fatalf("re-added declaration without extensions = %v", err)
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) {
		t.Fatalf("re-added declaration changed the file: %v", err)
	}
}

func TestConfigDeclarationRemovalPolicyAllowsNonRemoval(t *testing.T) {
	cfg := configDeclarationCfg()

	settings := cfg.Clone()
	settings.Clusters[0].Settings = map[string]string{"work_mem": "8MB"}

	if err := refuseClusterDeclarationRemoval(cfg, settings); err != nil {
		t.Fatalf("settings change = %v", err)
	}

	added := cfg.Clone()
	added.Clusters = append(added.Clusters, config.ClusterSpec{
		Name: "newdb", Node: "db1", Port: 5435, Stanza: "newdb-abc123", Size: "small", StatementTimeout: "60s",
	})

	if err := refuseClusterDeclarationRemoval(cfg, added); err != nil {
		t.Fatalf("new declaration = %v", err)
	}

	extensions := cfg.Clone()
	extensions.Clusters[0].Extensions = []string{"pg_stat_statements", "pg_trgm"}

	if err := refuseExtensionSelectionReduction(cfg, extensions); err != nil {
		t.Fatalf("extension addition = %v", err)
	}
}
