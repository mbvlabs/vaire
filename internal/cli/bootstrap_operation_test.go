package cli

import (
	"context"
	"io"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/spf13/cobra"
)

type bootstrapOperationCaller struct{ ids []string }

func (c *bootstrapOperationCaller) Call(_ context.Context, method string, _ any, opts remote.CallOptions) (remote.Reply, error) {
	if method != "cluster.bootstrap" {
		panic("unexpected test RPC")
	}
	c.ids = append(c.ids, opts.OperationID)
	return remote.Reply{Data: []byte(`{}`)}, nil
}

func TestSetupOperationPersistsAndDiffersAcrossSetups(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	makeRun := func(name string) *initRun {
		cfg := config.DefaultCluster()
		cfg.ClusterName = name
		return &initRun{name: name, cfg: cfg, prog: &contexts.Progress{}}
	}
	first := makeRun("first")
	if err := first.savePending(); err != nil {
		t.Fatal(err)
	}
	id := first.prog.SetupOperationID
	saved, err := contexts.LoadProgress("first")
	if err != nil || id == "" || saved.SetupOperationID != id {
		t.Fatalf("operation id was not durable: %q %+v %v", id, saved, err)
	}
	first.prog = saved
	if err := first.savePending(); err != nil || first.prog.SetupOperationID != id {
		t.Fatalf("resume changed operation id: %q, %v", first.prog.SetupOperationID, err)
	}
	second := makeRun("second")
	if err := second.savePending(); err != nil {
		t.Fatal(err)
	}
	if second.prog.SetupOperationID == "" || second.prog.SetupOperationID == id {
		t.Fatal("distinct setups share an operation identity")
	}
}

func TestBootstrapPhasesCarrySavedOperation(t *testing.T) {
	wg, _, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}
	ssh, _, err := transport.GenerateSSHKey("test")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cfg := config.DefaultCluster()
	cfg.ClusterName = "test"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: "master"}}
	r := &initRun{
		cmd: cmd, p: newProgress(cmd), name: "test", cfg: cfg,
		prog: &contexts.Progress{SetupOperationID: "original-setup"},
		sec:  contexts.PendingSecrets{OperatorName: "test", WGPrivateKey: wg, SSHPrivateKey: string(ssh)},
	}
	caller := &bootstrapOperationCaller{}
	if _, err := r.bootstrapHost(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if _, err := r.callDatabasePhase(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if len(caller.ids) != 2 || caller.ids[0] != "original-setup" || caller.ids[1] != caller.ids[0] {
		t.Fatalf("bootstrap phases did not share operation ID: %v", caller.ids)
	}
}
