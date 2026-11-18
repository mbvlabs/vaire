package node

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/totp"
)

func TestExtRemoveUnsupportedHasNoEffects(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	h := newExtHarness(t, cfg)
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_remove_unsupported" {
		t.Fatalf("ext remove = %v", err)
	}

	if details := extDetails(t, err); details["database"] != "app" || details["extension"] != "pg_trgm" {
		t.Fatalf("ext remove details = %+v", details)
	}

	if len(h.saves) != 0 || len(h.saveAttempts) != 0 || h.prepareCalls != 0 ||
		len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 || h.installedCalls != 0 {
		t.Fatalf("refused ext remove had effects: saves=%d attempts=%d prepare=%d apply=%d exec=%d restart=%d installed=%d",
			len(h.saves), len(h.saveAttempts), h.prepareCalls, len(h.applyCalls), len(h.pg.execs), h.restartCalls, h.installedCalls)
	}

	if recs, err := backup.LoadRecords(); err != nil || len(recs.ExtensionRequirements) != 0 {
		t.Fatalf("refused ext remove wrote history: %+v, %v", recs.ExtensionRequirements, err)
	}
}

func TestConfigSelectionReductionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, cfg *config.Cluster) config.Cluster
		reason string
	}{
		{
			name: "uninstalled SQL extension",
			mutate: func(_ *testing.T, cfg *config.Cluster) config.Cluster {
				cfg.Clusters[0].Extensions = []string{"pg_stat_statements", "pg_trgm"}
				updated := cfg.Clone()
				updated.Clusters[0].Extensions = []string{"pg_stat_statements"}

				return updated
			},
			reason: "extension_remove_unsupported",
		},
		{
			name: "preload-only extension",
			mutate: func(_ *testing.T, cfg *config.Cluster) config.Cluster {
				cfg.Extensions = map[string]config.ExtensionDefinition{
					"custom_preload": {Source: catalog.SourcePreinstalled, Preload: "custom_lib"},
				}
				cfg.Clusters[0].Extensions = []string{"custom_preload"}
				updated := cfg.Clone()
				updated.Clusters[0].Extensions = nil

				return updated
			},
			reason: "extension_remove_unsupported",
		},
		{
			name: "alias-equivalent selection",
			mutate: func(_ *testing.T, cfg *config.Cluster) config.Cluster {
				cfg.Clusters[0].Extensions = []string{"pgvector"}
				updated := cfg.Clone()
				updated.Clusters[0].Extensions = []string{"vector"}

				return updated
			},
		},
		{
			name: "addition",
			mutate: func(_ *testing.T, cfg *config.Cluster) config.Cluster {
				cfg.Clusters[0].Extensions = []string{"pg_stat_statements"}
				updated := cfg.Clone()
				updated.Clusters[0].Extensions = []string{"pg_stat_statements", "pg_trgm"}

				return updated
			},
		},
		{
			name: "unused descriptor cleanup",
			mutate: func(_ *testing.T, cfg *config.Cluster) config.Cluster {
				cfg.Extensions = map[string]config.ExtensionDefinition{
					"unused": {Source: catalog.SourcePreinstalled, ExtName: "unused"},
				}
				cfg.Clusters[0].Extensions = []string{"pg_stat_statements"}
				updated := cfg.Clone()
				updated.Extensions = nil

				return updated
			},
		},
		{
			name: "recovery declaration only",
			mutate: func(_ *testing.T, cfg *config.Cluster) config.Cluster {
				recovery := []string{"pg_trgm"}
				cfg.Clusters[0].Extensions = []string{"pg_stat_statements"}
				cfg.Clusters[0].RecoveryExtensions = &recovery
				updated := cfg.Clone()
				updated.Clusters[0].RecoveryExtensions = nil

				return updated
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultCluster()
			cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Extensions: []string{"pg_stat_statements"}}}
			updated := tc.mutate(t, &cfg)

			oldRecords, oldInstalled := extensionRecords, extensionInstalled
			extensionRecords = func() (backup.Records, error) {
				t.Fatal("config extension policy read records")
				return backup.Records{}, nil
			}
			extensionInstalled = func(context.Context, config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
				t.Fatal("config extension policy read the installed SQL catalog")
				return nil, nil
			}

			t.Cleanup(func() { extensionRecords, extensionInstalled = oldRecords, oldInstalled })

			err := refuseExtensionSelectionReduction(cfg, updated)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("allowed change refused: %v", err)
				}

				return
			}

			if err == nil || extReason(t, err) != tc.reason {
				t.Fatalf("change = %v", err)
			}

			if hint := extDetails(t, err)["extension"]; hint == nil {
				t.Fatalf("refusal did not name the extension: %+v", extDetails(t, err))
			}
		})
	}
}

func TestConfigSetExtensionAdditionStillSaves(t *testing.T) {
	cfg := standaloneExtensionCfg()
	cfg.Clusters[0].Extensions = []string{"pg_stat_statements"}

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
	updated.Clusters[0].Extensions = []string{"pg_stat_statements", "pg_trgm"}

	updatedRaw, err := updated.Encode()
	if err != nil {
		t.Fatal(err)
	}

	req := request("config.set", map[string]any{"toml": string(updatedRaw), "base_revision": revision})

	if _, err := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatalf("extension addition = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(got), "pg_trgm") {
		t.Fatalf("extension addition was not saved: %v\n%s", err, got)
	}
}

func TestConfigSetRefusesExtensionSelectionReductionBeforeSave(t *testing.T) {
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

	updated := cfg.Clone()
	updated.Clusters[0].Extensions = []string{"pg_stat_statements"}

	updatedRaw, err := updated.Encode()
	if err != nil {
		t.Fatal(err)
	}

	req := request("config.set", map[string]any{"toml": string(updatedRaw), "base_revision": revision})

	_, refusalErr := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg})
	if refusalErr == nil || extReason(t, refusalErr) != "extension_remove_unsupported" {
		t.Fatalf("config selection reduction = %v", refusalErr)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("config selection reduction changed the file: %v", err)
	}

	if recs, err := backup.LoadRecords(); err != nil || len(recs.ExtensionRequirements) != 0 {
		t.Fatalf("config selection reduction wrote history: %+v, %v", recs.ExtensionRequirements, err)
	}

	var cliErr *output.CLIError
	if !errors.As(refusalErr, &cliErr) || strings.Contains(cliErr.Hint, "ext remove") {
		t.Fatalf("refusal still points at ext remove: %v", refusalErr)
	}
}

func TestConfigSetRefusesSelectionReductionBeforeProtectVerification(t *testing.T) {
	cfg := standaloneExtensionCfg()
	cfg.Clusters[0].Protected = true
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
	updated.Clusters[0].Protected = false
	updated.Clusters[0].Extensions = []string{"pg_stat_statements"}

	updatedRaw, err := updated.Encode()
	if err != nil {
		t.Fatal(err)
	}

	req := request("config.set", map[string]any{"toml": string(updatedRaw), "base_revision": revision})
	req.Secrets = map[string]string{ProtectCodeSecret: codeFor(t, secret, clock.t, 0)}

	_, refusalErr := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg})
	if refusalErr == nil || extReason(t, refusalErr) != "extension_remove_unsupported" {
		t.Fatalf("combined unprotect and removal = %v", refusalErr)
	}

	after, err := guard.load()
	if err != nil {
		t.Fatal(err)
	}

	if after.LastStep != before.LastStep || after.Failures != before.Failures {
		t.Fatalf("refused change consumed protection state: before=%+v after=%+v", before, after)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("refused combined change changed the file: %v", err)
	}
}
