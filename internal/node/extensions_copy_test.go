package node

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/state"
)

func TestInvalidExtensionSettingCannotPersist(t *testing.T) {
	cfg := extensionViewCluster(t)
	validRaw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cluster.toml")
	store := state.FileStore{Path: path}
	if err := store.Save(validRaw); err != nil {
		t.Fatal(err)
	}
	invalid := cfg.Clone()
	invalid.Extensions["custom"] = config.ExtensionDefinition{
		Source: "preinstalled", Settings: map[string]string{"custom.mode": "ddl,role"},
	}
	invalidRaw, err := invalid.Encode()
	if err != nil {
		t.Fatal(err)
	}
	err = store.Save(invalidRaw)
	if err == nil || !strings.Contains(err.Error(), "not a safe PostgreSQL setting literal") {
		t.Fatalf("invalid setting persisted: %v", err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // G304: a test temp file.
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(validRaw) {
		t.Fatal("a rejected invalid setting changed cluster.toml")
	}
}

func TestExtensionIntentWarningsAreSemanticAndNotNoise(t *testing.T) {
	old := config.DefaultCluster()
	old.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	old.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: "preinstalled", Settings: map[string]string{"custom.mode": "on"}},
	}
	note := old.Clone()
	note.Extensions["custom"] = func() config.ExtensionDefinition {
		d := note.Extensions["custom"]
		d.Note = "human note"
		return d
	}()
	if warnings := extensionIntentWarnings(old, note); len(warnings) != 0 {
		t.Fatalf("a note-only change warned: %+v", warnings)
	}
	changed := old.Clone()
	changed.Extensions["custom"] = func() config.ExtensionDefinition {
		d := changed.Extensions["custom"]
		d.Settings = map[string]string{"custom.mode": "off"}
		return d
	}()
	warnings := extensionIntentWarnings(old, changed)
	if len(warnings) != 1 || warnings[0].Code != "extension_intent_changed" {
		t.Fatalf("semantic change warnings = %+v", warnings)
	}
}

func extensionViewCluster(t *testing.T) config.Cluster {
	t.Helper()
	recovery := []string{"pg_trgm"}
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Domain = "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s",
		Extensions: []string{"custom"}, RecoveryExtensions: &recovery,
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: "preinstalled", Settings: map[string]string{"custom.mode": "on"}},
	}
	return cfg
}

func TestDiagnosticsViewAndFollowerPreviewIsolateExtensionMetadata(t *testing.T) {
	cfg := extensionViewCluster(t)

	view := diagnosticsViewWith(cfg, backup.Records{})
	view.Extensions["custom"].Settings["custom.mode"] = "off"
	*view.Clusters[0].RecoveryExtensions = []string{"vector"}
	if cfg.Extensions["custom"].Settings["custom.mode"] != "on" {
		t.Fatal("diagnosticsViewWith shares extension settings")
	}
	if !slices.Equal(*cfg.Clusters[0].RecoveryExtensions, []string{"pg_trgm"}) {
		t.Fatal("diagnosticsViewWith shares recovery declarations")
	}

	preview, spec, err := followerPreviewEntry(cfg, cfg.Clusters[0], dbFollowParams{Name: "app_follower"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Follows != "app" {
		t.Fatalf("follower source = %q", spec.Follows)
	}
	preview.Extensions["custom"].Settings["custom.mode"] = "off"
	*preview.Clusters[0].RecoveryExtensions = []string{"vector"}
	if cfg.Extensions["custom"].Settings["custom.mode"] != "on" {
		t.Fatal("followerPreviewEntry shares extension settings")
	}
	if !slices.Equal(*cfg.Clusters[0].RecoveryExtensions, []string{"pg_trgm"}) {
		t.Fatal("followerPreviewEntry shares recovery declarations")
	}
}

func TestForkAndFollowerSpecCopyExtensionIntent(t *testing.T) {
	recovery := []string{"pg_trgm"}
	src := config.ClusterSpec{
		Name: "app", Size: "small",
		Extensions:         []string{"custom"},
		Settings:           map[string]string{"work_mem": "8MB"},
		RecoveryExtensions: &recovery,
	}
	fork := forkSpec(src, "copy")
	if fork.Name != "copy" || fork.Follows != "" || fork.Stanza != "" || fork.Protected {
		t.Fatalf("fork entry not reset: %+v", fork)
	}
	if !slices.Equal(fork.Extensions, []string{"custom"}) || fork.Settings["work_mem"] != "8MB" {
		t.Fatalf("fork lost extension intent: %+v", fork)
	}
	if fork.RecoveryExtensions == nil || !slices.Equal(*fork.RecoveryExtensions, []string{"pg_trgm"}) {
		t.Fatalf("fork lost recovery declarations: %v", fork.RecoveryExtensions)
	}
	fork.Extensions[0] = "changed"
	fork.Settings["work_mem"] = "changed"
	*fork.RecoveryExtensions = []string{"vector"}
	if src.Extensions[0] != "custom" || src.Settings["work_mem"] != "8MB" || !slices.Equal(*src.RecoveryExtensions, []string{"pg_trgm"}) {
		t.Fatal("forkSpec shares nested extension metadata with its source")
	}

	follower := followerSpec(src, "app_follower", "")
	if follower.Follows != "app" {
		t.Fatalf("follower source = %q", follower.Follows)
	}
	*follower.RecoveryExtensions = []string{"vector"}
	if !slices.Equal(*src.RecoveryExtensions, []string{"pg_trgm"}) {
		t.Fatal("followerSpec shares recovery declarations with its source")
	}
}
