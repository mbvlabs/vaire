package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestWorkspaceCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(workspaces.EnvWorkspace, "")
	for _, workspace := range []*workspaces.Workspace{
		{Name: "prod", Server: "prod", Nodes: []workspaces.Node{
			{Name: "db1", Role: workspaces.RoleMaster},
			{Name: "db2", Role: "worker"},
		}, Databases: map[string]string{"billing": "db2"}},
		{Name: "lab", Server: "lab", Nodes: []workspaces.Node{{Name: "db1", Role: workspaces.RoleMaster}}},
	} {
		if err := workspaces.Save(workspace); err != nil {
			t.Fatal(err)
		}
	}
	if err := workspaces.SaveCredentials("prod", workspaces.Credentials{SudoPassword: "never-print-this-password"}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		root := NewRoot()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.Execute()
		return out.String(), err
	}
	if _, err := run("workspace", "use", "prod", "--json"); err != nil {
		t.Fatal(err)
	}
	raw, err := run("workspace", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []workspaceRow `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("list JSON: %s: %v", raw, err)
	}
	if len(list.Data) != 2 || list.Data[1].Name != "prod" || !list.Data[1].Current || list.Data[1].Nodes != 2 || strings.Join(list.Data[1].NodeNames, ",") != "db1,db2" {
		t.Fatalf("list = %+v", list.Data)
	}
	checkWorkspaceShow(t, run)
	t.Setenv(workspaces.EnvWorkspace, "lab")
	if raw, err := run("workspace", "show", "--json"); err != nil || !strings.Contains(raw, `"name": "lab"`) {
		t.Fatalf("environment selection: %s, %v", raw, err)
	}
	if raw, err := run("workspace", "show", "-w", "prod", "--json"); err != nil || !strings.Contains(raw, `"name": "prod"`) {
		t.Fatalf("flag selection: %s, %v", raw, err)
	}
	if raw, err := run("workspace", "set-address", "198.51.100.7", "-w", "prod", "--dry-run", "--json"); err != nil || !strings.Contains(raw, `"workspace": "prod"`) {
		t.Fatalf("set-address selection: %s, %v", raw, err)
	}
	if _, err := run("workspace", "remove", "lab", "--confirm", "lab", "--json"); err != nil {
		t.Fatal(err)
	}
	if workspaces.Exists("lab") || !workspaces.Exists("prod") {
		t.Fatal("removal changed the wrong workspace")
	}
	checkObsoleteWorkspaceCommands(t, run)
}

func checkWorkspaceShow(t *testing.T, run func(...string) (string, error)) {
	t.Helper()
	for _, args := range [][]string{
		{"workspace", "show", "--json"},
		{"workspace", "show", "-w", "prod", "--json"},
		{"workspace", "show", "--workspace", "prod", "--json"},
		{"workspace", "show", "prod", "--json"},
	} {
		raw, err := run(args...)
		if err != nil {
			t.Fatal(err)
		}
		var show struct {
			Data struct {
				Name      string            `json:"name"`
				Nodes     []workspaces.Node `json:"nodes"`
				Databases map[string]string `json:"databases"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(raw), &show); err != nil {
			t.Fatal(err)
		}
		if show.Data.Name != "prod" || len(show.Data.Nodes) != 2 || show.Data.Nodes[1].Name != "db2" || show.Data.Databases["billing"] != "db2" {
			t.Fatalf("show = %+v", show.Data)
		}
		if strings.Contains(raw, "never-print-this-password") || strings.Contains(raw, `"context"`) {
			t.Fatal("workspace show leaked credentials or obsolete output")
		}
	}
}

func checkObsoleteWorkspaceCommands(t *testing.T, run func(...string) (string, error)) {
	t.Helper()
	for _, args := range [][]string{{"context", "list"}, {"workspace", "list", "--context", "prod"}, {"workspace", "list", "-c", "prod"}} {
		if _, err := run(args...); err == nil {
			t.Fatalf("obsolete command accepted: %v", args)
		}
	}
}

func TestWorkspaceDiscovery(t *testing.T) {
	root := NewRoot()
	flag := root.PersistentFlags().Lookup("workspace")
	if flag == nil || flag.Shorthand != "w" || root.PersistentFlags().Lookup("context") != nil {
		t.Fatal("workspace selector is missing or obsolete selector remains")
	}
	for _, child := range root.Commands() {
		if child.Name() == "context" {
			t.Fatal("obsolete command remains discoverable")
		}
	}
	for _, name := range []string{"list", "show", "use", "remove", "set-address"} {
		commandAt(t, "workspace "+name)
	}
	if report := checkCommandMetadata(root); !report.OK {
		t.Fatalf("metadata: %v", report.Failures)
	}
}
