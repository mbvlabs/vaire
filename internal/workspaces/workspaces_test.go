package workspaces

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

func TestWorkspaceHintOnNode(t *testing.T) {
	saved := nodeMarker
	t.Cleanup(func() { nodeMarker = saved })

	nodeMarker = filepath.Join(t.TempDir(), "cluster.toml")
	if got := workspaceHint("default"); got != "default" {
		t.Fatalf("off a node: got %q, want the default hint", got)
	}
	if err := os.WriteFile(nodeMarker, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := workspaceHint("default"); !strings.Contains(got, "vaire node") || !strings.Contains(got, "your own machine") {
		t.Fatalf("on a node: got %q, want the run-from-your-machine hint", got)
	}
}

func TestWorkspaceRoundTripAndSelection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(EnvWorkspace, "")
	prod := &Workspace{
		Name: "prod", Server: "prod",
		Nodes: []Node{
			{Name: "db1", Role: RoleMaster, Hostname: "db1.example.com"},
			{Name: "db2", Role: "worker", Hostname: "db2.example.com"},
		},
		Databases: map[string]string{"app": "db1", "billing": "db2"},
	}
	if err := Save(prod); err != nil {
		t.Fatal(err)
	}
	if err := Save(&Workspace{Name: "lab", Server: "lab"}); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrent("prod"); err != nil {
		t.Fatal(err)
	}
	got, err := Load("prod")
	if err != nil || !reflect.DeepEqual(got, prod) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if node, ok := got.DatabaseNode("billing"); !ok || node.Name != "db2" {
		t.Fatalf("placement = %+v, %v", node, ok)
	}
	if node, ok := got.Master(); !ok || node.Name != "db1" {
		t.Fatalf("master = %+v, %v", node, ok)
	}
	if node, ok := got.DatabaseNode("unknown"); ok || node.Name != "db1" {
		t.Fatalf("unknown placement = %+v, %v", node, ok)
	}
	if names, err := List(); err != nil || !reflect.DeepEqual(names, []string{"lab", "prod"}) {
		t.Fatalf("List = %v, %v", names, err)
	}
	checkWorkspaceSelection(t)
	checkWorkspaceStorage(t)
}

func checkWorkspaceSelection(t *testing.T) {
	t.Helper()
	check := func(flag, want string) {
		t.Helper()
		workspace, err := Resolve(flag)
		if err != nil || workspace.Name != want {
			t.Fatalf("Resolve(%q) = %+v, %v; want %s", flag, workspace, err, want)
		}
	}
	check("", "prod")
	t.Setenv(EnvWorkspace, " lab ")
	check("", "lab")
	check(" prod ", "prod")
	t.Setenv(EnvWorkspace, "missing")
	if _, err := Resolve(""); output.ExitCode(err) != output.ExitWorkspace {
		t.Fatalf("missing explicit environment target: %v", err)
	}
	check("prod", "prod")
}

func checkWorkspaceStorage(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(Root(), "config.toml"))
	if err != nil || !strings.Contains(string(raw), "current_workspace") || strings.Contains(string(raw), "current_context") {
		t.Fatalf("saved selection = %q, %v", raw, err)
	}
	if Path("prod", WorkspaceFile) != filepath.Join(Root(), "workspaces", "prod", "workspace.toml") {
		t.Fatal("unexpected workspace path")
	}
	for _, path := range []string{Root(), Dir("prod")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode: %s: %v", path, err)
		}
	}
	for _, path := range []string{Path("prod", WorkspaceFile), filepath.Join(Root(), "config.toml")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode: %s: %v", path, err)
		}
	}
	if err := Remove("prod"); err != nil {
		t.Fatal(err)
	}
	if current, err := Current(); err != nil || current != "" || Exists("prod") || !Exists("lab") {
		t.Fatalf("remove current workspace: current=%q, err=%v", current, err)
	}
}

func TestValidateName(t *testing.T) {
	for _, name := range []string{"", "select 1", "Prod", "../prod", "prod/other"} {
		var cliErr *output.CLIError
		if err := ValidateName(name); !errors.As(err, &cliErr) || cliErr.Code != output.CodeValidation || strings.Contains(cliErr.Hint, "psql") {
			t.Fatalf("ValidateName(%q) = %v, want the workspace naming hint", name, err)
		}
	}
	for _, name := range []string{"prod", "my-lab", "prod.eu", "lab_1"} {
		if err := ValidateName(name); err != nil {
			t.Fatalf("ValidateName(%q): %v", name, err)
		}
	}
}
