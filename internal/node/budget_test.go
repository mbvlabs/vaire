package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/tune"
)

// db2Host is the test node db2.mbvlabs.com: 7.6 GiB of RAM, so a 1.9 GiB
// reserve and a 5.7 GiB cluster budget.
var db2Host = hostinfo.Info{RAMMiB: 7782, VCPU: 2}

func budgetCluster(specs ...config.ClusterSpec) config.Cluster {
	return config.Cluster{Postgres: config.Postgres{DefaultSize: config.DefaultSize}, Clusters: specs}
}

func overcommitDetails(t *testing.T, err error) (*output.CLIError, map[string]any) {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("got %v, want a CLIError", err)
	}
	if cliErr.Code != output.CodePrecondition || cliErr.ExitCode != output.ExitPrecondition {
		t.Fatalf("code %s exit %d, want precondition_failed", cliErr.Code, cliErr.ExitCode)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["reason"] != tune.ReasonMemoryOvercommitted {
		t.Fatalf("reason %v, want memory_overcommitted", details["reason"])
	}
	return cliErr, details
}

func createSuggest(name string) func(string) string {
	return createCommand(dbCreateParams{Name: name, StatementTimeout: "60s", Extensions: []string{"pg_stat_statements"}})
}

func TestCheckBudgetFits(t *testing.T) {
	// app2 at medium plus big at xlarge is 5 GiB on a 5.7 GiB budget.
	cfg := budgetCluster(config.ClusterSpec{Name: "app2", Size: "medium"}, config.ClusterSpec{Name: "big", Size: "xlarge"})
	if err := checkBudget(cfg, "big", 0, db2Host, false, createSuggest("big")); err != nil {
		t.Fatalf("fits: got %v", err)
	}
}

func TestCheckBudgetRefusedWithSuggestion(t *testing.T) {
	// big2 without --size gets the default, medium: 6 GiB on 5.7 GiB.
	cfg := budgetCluster(
		config.ClusterSpec{Name: "app2", Size: "medium"},
		config.ClusterSpec{Name: "big", Size: "xlarge"},
		config.ClusterSpec{Name: "big2", Size: config.DefaultSize},
	)
	err := checkBudget(cfg, "big2", 0, db2Host, false, createSuggest("big2"))
	cliErr, details := overcommitDetails(t, err)
	for _, want := range []string{"big2 at medium (1 GiB)", "6 GiB of database sizes", "5.7 GiB budget", "7.6 GiB RAM", "1.9 GiB reserve"} {
		if !strings.Contains(cliErr.Message, want) {
			t.Errorf("message %q lacks %q", cliErr.Message, want)
		}
	}
	if details["largest_size"] != "small" {
		t.Errorf("largest_size = %v, want small", details["largest_size"])
	}
	if want := "vaire db create big2 --size small"; details["suggested_command"] != want || !strings.Contains(cliErr.Hint, want) {
		t.Errorf("suggested %v, hint %q, want %q", details["suggested_command"], cliErr.Hint, want)
	}
	if !strings.Contains(cliErr.Hint, "--allow-overcommit") {
		t.Errorf("hint %q doesn't mention --allow-overcommit", cliErr.Hint)
	}
}

func TestCheckBudgetNothingFits(t *testing.T) {
	// app2 medium, big xlarge and big2 small leave 205 MiB: not even nano.
	cfg := budgetCluster(
		config.ClusterSpec{Name: "app2", Size: "medium"},
		config.ClusterSpec{Name: "big", Size: "xlarge"},
		config.ClusterSpec{Name: "big2", Size: "small"},
		config.ClusterSpec{Name: "big3", Size: "nano"},
	)
	err := checkBudget(cfg, "big3", 0, db2Host, false, createSuggest("big3"))
	cliErr, details := overcommitDetails(t, err)
	if _, ok := details["largest_size"]; ok {
		t.Errorf("largest_size = %v, want none", details["largest_size"])
	}
	for _, want := range []string{"No size fits", "vaire db resize", "big at xlarge", "bigger server"} {
		if !strings.Contains(cliErr.Hint, want) {
			t.Errorf("hint %q lacks %q", cliErr.Hint, want)
		}
	}
}

func TestCheckBudgetAllowOvercommit(t *testing.T) {
	cfg := budgetCluster(
		config.ClusterSpec{Name: "app2", Size: "medium"},
		config.ClusterSpec{Name: "big", Size: "xlarge"},
		config.ClusterSpec{Name: "big2", Size: "medium"},
	)
	if err := checkBudget(cfg, "big2", 0, db2Host, true, createSuggest("big2")); err != nil {
		t.Fatalf("--allow-overcommit: got %v", err)
	}
	// It still warns.
	var found bool
	for _, w := range oversubscriptionWarnings(cfg, db2Host) {
		found = found || w.Code == tune.WarnMemoryOvercommitted
	}
	if !found {
		t.Error("no memory_overcommitted warning")
	}
}

func TestCheckBudgetResize(t *testing.T) {
	// app2 medium to large next to big at xlarge: 6 GiB. Only sizes above
	// medium count, and none fits.
	cfg := budgetCluster(config.ClusterSpec{Name: "app2", Size: "large"}, config.ClusterSpec{Name: "big", Size: "xlarge"})
	suggest := resizeCommand(dbResizeParams{Name: "app2", Size: "large"}, true)
	cliErr, details := overcommitDetails(t, checkBudget(cfg, "app2", 1024, db2Host, false, suggest))
	if _, ok := details["largest_size"]; ok {
		t.Errorf("largest_size = %v, want none above medium", details["largest_size"])
	}
	if !strings.Contains(cliErr.Hint, "No size larger than 1 GiB fits") {
		t.Errorf("hint %q", cliErr.Hint)
	}

	// From small to xlarge next to app2 at large: large still fits.
	cfg = budgetCluster(config.ClusterSpec{Name: "app2", Size: "large"}, config.ClusterSpec{Name: "other", Size: "xlarge"})
	suggest = resizeCommand(dbResizeParams{Name: "other", Size: "xlarge"}, true)
	_, details = overcommitDetails(t, checkBudget(cfg, "other", 512, db2Host, false, suggest))
	if want := "vaire db resize other --size large --confirm other/restart"; details["suggested_command"] != want {
		t.Errorf("suggested %v, want %q", details["suggested_command"], want)
	}

	// Shrinking on an overcommitted server is never refused.
	cfg = budgetCluster(config.ClusterSpec{Name: "app2", Size: "large"}, config.ClusterSpec{Name: "big", Size: "xlarge"})
	if err := checkBudget(cfg, "big", 8192, db2Host, false, suggest); err != nil {
		t.Errorf("shrink: got %v", err)
	}
}

func TestForkCommand(t *testing.T) {
	got := forkCommand(dbForkParams{Source: "app2", Name: "app2_copy", At: "2026-10-14T12:00:00Z"})("small")
	if want := "vaire db fork app2 --name app2_copy --at 2026-10-14T12:00:00Z --size small"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = createCommand(dbCreateParams{Name: "big2", Profile: "oltp", Protected: true, StatementTimeout: "60s"})("small")
	if want := "vaire db create big2 --size small --profile oltp --protected"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLargestThatFits(t *testing.T) {
	var candidates []tune.Demand
	for _, s := range config.Sizes {
		candidates = append(candidates, tune.Demand{Size: s.Name, MemoryMiB: s.MemoryMiB})
	}
	cases := []struct {
		free, above int
		want        string
	}{
		{717, 0, "small"},
		{5837, 0, "xlarge"},
		{200, 0, ""},
		{1741, 1024, ""},
		{2048, 1024, "large"},
	}
	for _, c := range cases {
		if got := tune.LargestThatFits(candidates, c.free, c.above); got != c.want {
			t.Errorf("LargestThatFits(%d, %d) = %q, want %q", c.free, c.above, got, c.want)
		}
	}
}
