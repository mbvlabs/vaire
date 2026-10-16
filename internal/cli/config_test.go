package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/config"
)

func TestConfigValueOutputTable(t *testing.T) {
	m := config.Maintenance{Window: "02:00-05:00", RepackBloatThreshold: 0.4}
	data, message, err := configValueOutput("maintenance", m)
	if err != nil {
		t.Fatal(err)
	}
	want := "[maintenance]\nwindow = '02:00-05:00'\nauto_repack = false\nrepack_bloat_threshold = 0.4"
	if message != want {
		t.Fatalf("message = %q, want %q", message, want)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"auto_repack":false,"repack_bloat_threshold":0.4,"window":"02:00-05:00"}` {
		t.Fatalf("json = %s", got)
	}
}

func TestConfigValueOutputNestedAndArrays(t *testing.T) {
	_, message, err := configValueOutput("postgres.settings", map[string]string{"work_mem": "64MB"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(message, "[postgres.settings]\n") || !strings.Contains(message, "work_mem = '64MB'") {
		t.Fatalf("message = %q", message)
	}
	data, message, err := configValueOutput("metrics.allowlist", []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if message != "metrics.allowlist = ['10.0.0.0/8']" {
		t.Fatalf("message = %q", message)
	}
	if raw, _ := json.Marshal(data); string(raw) != `["10.0.0.0/8"]` {
		t.Fatalf("json = %s", raw)
	}
	_, message, err = configValueOutput("clusters", []config.ClusterSpec{{Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(message, "[[clusters]]\n") || !strings.Contains(message, "name = 'app'") {
		t.Fatalf("message = %q", message)
	}
}

func TestConfigValueOutputScalar(t *testing.T) {
	data, message, err := configValueOutput("maintenance.window", "02:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	if data != "02:00-05:00" || message != "maintenance.window = 02:00-05:00" {
		t.Fatalf("got %v, %q", data, message)
	}
}

// TestConfigValueOutputDefaults renders every top-level table of the default
// config: none comes out as a Go struct.
func TestConfigValueOutputDefaults(t *testing.T) {
	cfg := config.DefaultCluster()
	for _, key := range []string{"tls", "transport", "server", "postgres", "network", "backup", "backup.database", "master_backup", "maintenance", "metrics"} {
		value, err := cfg.Get(key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		data, message, err := configValueOutput(key, value)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if _, ok := data.(map[string]any); !ok {
			t.Fatalf("%s: data is %T, want a JSON object", key, data)
		}
		if !strings.HasPrefix(message, "["+key+"]") || strings.Contains(message, "{") {
			t.Fatalf("%s: message = %q", key, message)
		}
	}
}
