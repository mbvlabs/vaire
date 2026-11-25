package cli

import (
	"encoding/json"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestCopyCreatedDestination(t *testing.T) {
	ws := &workspaces.Workspace{Nodes: []workspaces.Node{
		{Name: "master", WGAddress: "10.0.0.1", Hostname: "master.example.com"},
		{Name: "other", WGAddress: "10.0.0.2", Hostname: "other.example.com"},
	}}

	raw, err := json.Marshal(map[string]any{"database": config.ClusterSpec{Name: "copy", Node: "other", Port: 5433}})
	if err != nil {
		t.Fatal(err)
	}

	db, endpoint, err := copyCreatedDestination(remote.Reply{Data: raw}, "copy", ws)
	if err != nil || db.Node != "other" || endpoint.WGAddress != "10.0.0.2" || endpoint.Hostname != "other.example.com" {
		t.Fatalf("got %#v %#v %v", db, endpoint, err)
	}

	for _, bad := range []string{
		``, `null`, `{}`, `{"database":null}`, `{"database":{"Name":"copy","Node":"other","Port":5433},"extra":1}`,
		`{"database":{"Name":"wrong","Node":"other","Port":5433}}`,
		`{"database":{"Name":"copy","Node":"missing","Port":5433}}`,
		`{"database":{"Name":"copy","Node":"other","Port":0}}`,
		`{"database":{"Name":"copy","Node":"other","Port":5433}} {}`,
	} {
		if _, _, err := copyCreatedDestination(remote.Reply{Data: []byte(bad)}, "copy", ws); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}

	ws.Nodes = append(ws.Nodes, ws.Nodes[1])
	if _, _, err := copyCreatedDestination(remote.Reply{Data: raw}, "copy", ws); err == nil {
		t.Fatal("accepted ambiguous node")
	}
}
