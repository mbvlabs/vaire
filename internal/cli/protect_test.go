package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/spf13/cobra"
)

func TestGroupSecret(t *testing.T) {
	if got := groupSecret("JBSWY3DPEHPK3PXPJB"); got != "JBSW Y3DP EHPK 3PXP JB" {
		t.Fatalf("got %q", got)
	}

	if got := groupSecret("ABCD"); got != "ABCD" {
		t.Fatalf("got %q", got)
	}
}

func cliErrorOf(t *testing.T, err error) *output.CLIError {
	t.Helper()

	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("got %v, want a CLIError", err)
	}

	return cliErr
}

func reasonOf(cliErr *output.CLIError) string {
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)

	return reason
}

func TestProtectTargets(t *testing.T) {
	dbs := []string{"app", "billing", "other"}

	got, err := protectTargets([]string{"other", "app", "other"}, false, dbs)
	if err != nil || !slices.Equal(got, []string{"other", "app"}) {
		t.Fatalf("names: %v %v", got, err)
	}

	got, err = protectTargets(nil, true, dbs)
	if err != nil || !slices.Equal(got, dbs) {
		t.Fatalf("--all: %v %v", got, err)
	}

	if got, err = protectTargets(nil, true, nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("--all without databases: %v %v, want an empty, non-nil list", got, err)
	}

	if e := cliErrorOf(t, func() error { _, err := protectTargets([]string{"app"}, true, dbs); return err }()); e.Code != output.CodeUsage {
		t.Fatalf("names and --all: %s", e.Code)
	}

	if e := cliErrorOf(t, func() error { _, err := protectTargets(nil, false, dbs); return err }()); e.Code != output.CodeUsage {
		t.Fatalf("nothing named: %s", e.Code)
	}

	_, err = protectTargets([]string{"app", "nope", "gone"}, false, dbs)

	e := cliErrorOf(t, err)
	if e.Code != output.CodePrecondition || reasonOf(e) != "database_not_found" || !strings.Contains(e.Message, "nope, gone") {
		t.Fatalf("unknown names: %s %q %q", e.Code, reasonOf(e), e.Message)
	}
}

func TestSplitProtectedAndSet(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Protected: true}, {Name: "billing"}, {Name: "other"}}}
	if got := databaseNames(cfg); !slices.Equal(got, []string{"app", "billing", "other"}) {
		t.Fatalf("databaseNames = %v", got)
	}

	protected, rest := splitProtected(cfg, []string{"other", "app", "billing"})
	if !slices.Equal(protected, []string{"app"}) || !slices.Equal(rest, []string{"other", "billing"}) {
		t.Fatalf("splitProtected = %v, %v", protected, rest)
	}

	set := protectSet([]string{"billing", "other"}, true)
	if len(set) != 2 || set["clusters.billing.protected"] != "true" || set["clusters.other.protected"] != "true" {
		t.Fatalf("protectSet = %v", set)
	}
	// The keys are the ones vaire config set takes.
	for key, value := range protectSet([]string{"app"}, false) {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}

	if spec, _ := cfg.ClusterByName("app"); spec.Protected {
		t.Fatal("clusters.app.protected false did not lift it")
	}
}

func TestProtectStatusLines(t *testing.T) {
	lines := protectStatusLines([]node.ProtectedDatabase{{Name: "app", Protected: true}, {Name: "billing_archive"}})
	if len(lines) != 3 || !strings.Contains(lines[0], "PROTECTED") {
		t.Fatalf("lines = %q", lines)
	}

	if f := strings.Fields(lines[1]); !slices.Equal(f, []string{"app", "yes"}) {
		t.Errorf("app row = %q", lines[1])
	}

	if f := strings.Fields(lines[2]); !slices.Equal(f, []string{"billing_archive", "no"}) {
		t.Errorf("billing_archive row = %q", lines[2])
	}

	if lines := protectStatusLines(nil); len(lines) != 1 || !strings.Contains(lines[0], "no databases") {
		t.Errorf("no databases: %q", lines)
	}
}

func TestDBListLinesProtected(t *testing.T) {
	lines := dbListLines([]databaseInfo{
		{Name: "app", InConfig: true, Protected: true, State: "running"},
		{Name: "other", InConfig: true, State: "running"},
		{Name: "stray", State: "orphaned"},
	})
	if !strings.Contains(lines[0], "PROTECTED") {
		t.Fatalf("header = %q", lines[0])
	}

	col := strings.Index(lines[0], "PROTECTED")
	for i, want := range []string{"yes", "no", "-"} {
		if got := strings.Fields(lines[i+1][col:])[0]; got != want {
			t.Errorf("row %d PROTECTED = %q, want %q", i+1, got, want)
		}
	}

	if len(dbListLines(nil)) != 0 {
		t.Error("no databases: want no header")
	}
}

// commandAt returns the command at path in a fresh tree, with its flags
// parsed from args.
func commandAt(t *testing.T, path string, args ...string) *cobra.Command {
	t.Helper()

	root := NewRoot()

	cmd, _, err := root.Find(strings.Fields(path))
	if err != nil || cmd.CommandPath() != "vaire "+path {
		t.Fatalf("find %s: %v", path, err)
	}

	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}

	return cmd
}

func TestProtectCommandsInTree(t *testing.T) {
	if report := checkCommandMetadata(NewRoot()); !report.OK {
		t.Fatalf("metadata: %v", report.Failures)
	}

	cmd, _, err := NewRoot().Find([]string{"protect", "init"})
	if err == nil && cmd.CommandPath() == "vaire protect init" {
		t.Fatal("protect init must not be registered")
	}

	for path, flags := range map[string][]string{
		"db protect":   {"all", "dry-run"},
		"db unprotect": {"dry-run"},
	} {
		cmd := commandAt(t, path)
		for _, f := range flags {
			if cmd.Flags().Lookup(f) == nil {
				t.Errorf("%s lacks --%s", path, f)
			}
		}
	}

	if cmd := commandAt(t, "db unprotect"); cmd.Args(cmd, nil) == nil {
		t.Error("db unprotect without a name should be refused")
	}
}

func TestDBProtectUsage(t *testing.T) {
	// Both are refused before any connection is made.
	if e := cliErrorOf(t, runDBProtect(commandAt(t, "db protect"), nil)); e.Code != output.CodeUsage {
		t.Fatalf("no names: %s", e.Code)
	}

	if e := cliErrorOf(t, runDBProtect(commandAt(t, "db protect", "--all"), []string{"app"})); e.Code != output.CodeUsage {
		t.Fatalf("names and --all: %s", e.Code)
	}
}

func TestUnprotectRefusedWithoutHuman(t *testing.T) {
	// unprotectCode refuses before it touches the session or the progress.
	for _, args := range [][]string{{"--yes"}, {"--output", "json"}, {"--no-input"}} {
		_, err := unprotectCode(commandAt(t, "db unprotect", args...), nil, nil, []string{"app", "billing"})

		e := cliErrorOf(t, err)
		if e.Code != output.CodeDatabaseProtected || reasonOf(e) != "unprotect_requires_human" || !strings.Contains(e.Message, "app, billing") {
			t.Errorf("%v: %s %q %q", args, e.Code, reasonOf(e), e.Message)
		}
	}
	// A dry run lifts nothing, so it asks nothing.
	if code, err := unprotectCode(commandAt(t, "db unprotect", "--yes", "--dry-run"), nil, nil, []string{"app"}); err != nil || code != nil {
		t.Fatalf("dry run: %v %v", code, err)
	}
}
