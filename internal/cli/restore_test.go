package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

// The combined restore --promote decodes the job's followers_diverged
// warnings as structured warnings with their own code, while the job's dump
// warnings keep the restore_dump_errors label (D12, R3-3).
func TestRestoreWarningsKeepFollowersDivergedStructured(t *testing.T) {
	raw := `{
		"phase": "promoted",
		"warnings": ["dump public.events had 2 errors"],
		"follower_warnings": [{"code": "followers_diverged",
			"message": "app is on a new timeline, so its followers (app_follower) stopped following it and serve the old data read-only."}],
		"promoted": {"stanza": "app-3f9a1c"}
	}`
	var res restoreJobResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	warnings := restoreWarnings(nil, nil, res)
	if len(warnings) != 2 {
		t.Fatalf("warnings %+v, want the dump warning and the divergence warning", warnings)
	}
	if warnings[0].Code != "restore_dump_errors" || warnings[0].Message != "dump public.events had 2 errors" {
		t.Errorf("the dump warning: %+v", warnings[0])
	}
	if warnings[1].Code != "followers_diverged" || !strings.Contains(warnings[1].Message, "new timeline") {
		t.Errorf("the divergence warning: %+v", warnings[1])
	}
	for _, w := range warnings {
		if w.Code == "restore_dump_errors" && strings.Contains(w.Message, "new timeline") {
			t.Errorf("the divergence message is labelled a dump error: %+v", w)
		}
	}
}

// The combined restore --promote uses the same node-order preflight: a
// follower is refused with is_follower before protection is checked, so a
// protected follower does not answer database_protected and no token, dump
// identity or dry run follows (Phase 6, B-04).
func TestCombinedRestorePreflightRefusesAFollowerFirst(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-3f9a1c", Size: "large", StatementTimeout: "60s"},
		{Name: "app_follower", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app"},
	}
	for _, protected := range []bool{false, true} {
		cfg.Clusters[1].Protected = protected
		err := combinedRestorePreflight(cfg, "app_follower")
		var ce *output.CLIError
		if !errors.As(err, &ce) {
			t.Fatalf("combined restore on a follower (protected %v): %v, want is_follower", protected, err)
		}
		d, _ := ce.Details.(map[string]any)
		if ce.Code != output.CodePrecondition || d["reason"] != "is_follower" || d["source"] != "app" {
			t.Errorf("combined restore on a follower (protected %v): %s %v, want is_follower from app", protected, ce.Code, d)
		}
		if err := combinedRestorePreflight(cfg, "app"); err != nil {
			t.Errorf("combined restore on the source (protected %v): %v", protected, err)
		}
	}
}

// restore promote and undo refuse a follower with the node's is_follower
// error before protection is checked or the token asked for, so a protected
// follower does not answer database_protected (R3-2). A source still passes.
func TestRestorePreflightRefusesAFollowerFirst(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-3f9a1c", Size: "large", StatementTimeout: "60s"},
		{Name: "app_follower", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app"},
	}
	for _, protected := range []bool{false, true} {
		cfg.Clusters[1].Protected = protected
		for _, c := range []struct{ method, action string }{
			{"restore.promote", "restore promote"},
			{"restore.undo", "restore undo"},
		} {
			err := restorePreflight(cfg, "app_follower", c.method, c.action)
			var ce *output.CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("%s on a follower (protected %v): %v, want is_follower", c.method, protected, err)
			}
			d, _ := ce.Details.(map[string]any)
			if d["reason"] != "is_follower" || d["source"] != "app" {
				t.Errorf("%s on a follower (protected %v): details %v, want is_follower from app", c.method, protected, d)
			}
			if err := restorePreflight(cfg, "app", c.method, c.action); err != nil {
				t.Errorf("%s on the source: %v", c.method, err)
			}
		}
	}
}
