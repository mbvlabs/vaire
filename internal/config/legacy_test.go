package config

import (
	"strings"
	"testing"
)

func TestParseClusterDropsLegacyVPS(t *testing.T) {
	cfg := DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Clusters = []ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-3f9a1c", Size: "small", Extensions: []string{"pg_trgm"}, PoolSize: 20, Settings: map[string]string{"work_mem": "8MB"}, Protected: true},
		{Name: "other", Node: "db1", Port: 5434, Stanza: "other-0a0b0c", Size: "small"},
	}
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	legacy := string(raw) + "\n[backup.vps]\nenabled = true\nschedule = \"Sun *-*-* 03:30:00\"\nkeep_weekly = 8\nextra_paths = []\n"
	got, err := ParseCluster([]byte(legacy))
	if err != nil {
		t.Fatalf("a cluster.toml from before Phase 5 doesn't parse: %v", err)
	}
	if got.Name() != "prod" {
		t.Fatalf("name %q", got.Name())
	}
	if !HasLegacyVPS([]byte(legacy)) || HasLegacyVPS(raw) {
		t.Fatal("HasLegacyVPS")
	}
	plain, err := ParseCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	gotRaw, _ := got.Encode()
	plainRaw, _ := plain.Encode()
	if string(gotRaw) != string(plainRaw) {
		t.Fatalf("dropping [backup.vps] changed the rest:\n%s\n---\n%s", gotRaw, plainRaw)
	}
	again, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), "vps") {
		t.Fatal("the next save still writes [backup.vps]")
	}
	if _, err := ParseCluster([]byte(string(raw) + "\n[backup.other]\nx = 1\n")); err == nil {
		t.Fatal("strict decoding is gone for other unknown tables")
	}
}
