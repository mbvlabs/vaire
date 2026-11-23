package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

var machineOperatorNamePattern = regexp.MustCompile(`^machine-[0-9a-hjkmnp-tv-z]{12}$`)

func machineOperatorTestRun(t *testing.T, name string) *initRun {
	t.Helper()

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	cfg := config.DefaultCluster()
	cfg.ClusterName = name

	return &initRun{
		cmd: cmd, p: newProgress(cmd), name: name, cfg: cfg,
		prog: &workspaces.Progress{Phases: map[string]string{}},
	}
}

func TestFreshInitGeneratesPersistentMachineOperator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	first := machineOperatorTestRun(t, "prod")
	if err := first.savePending(); err != nil {
		t.Fatal(err)
	}

	if err := first.generateSecrets(); err != nil {
		t.Fatal(err)
	}

	name := first.sec.OperatorName
	if !machineOperatorNamePattern.MatchString(name) {
		t.Fatalf("fresh init operator name %q is not machine- plus 12 lowercase Crockford base32 characters", name)
	}

	saved, err := workspaces.LoadSecrets("prod")
	if err != nil || saved.OperatorName != name || saved.WGPrivateKey != first.sec.WGPrivateKey || saved.SSHPrivateKey != first.sec.SSHPrivateKey {
		t.Fatalf("the generated operator identity or keys were not persisted: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().String("name", "prod", "")

	resumed := &initRun{cmd: cmd}
	if err := resumed.loadPending(); err != nil {
		t.Fatal(err)
	}

	if resumed.sec.OperatorName != name || resumed.sec.SSHPrivateKey != first.sec.SSHPrivateKey {
		t.Fatalf("resume changed the operator identity: %q -> %q", name, resumed.sec.OperatorName)
	}

	second := machineOperatorTestRun(t, "staging")
	if err := second.generateSecrets(); err != nil {
		t.Fatal(err)
	}

	if second.sec.OperatorName == name {
		t.Fatalf("two fresh inits generated the same operator name %q", name)
	}
}

type operatorCaptureCaller struct {
	operator config.Operator
}

func (c *operatorCaptureCaller) Call(_ context.Context, method string, params any, _ remote.CallOptions) (remote.Reply, error) {
	if method != "cluster.bootstrap" {
		return remote.Reply{}, fmt.Errorf("unexpected RPC %s", method)
	}

	raw, err := json.Marshal(params)
	if err != nil {
		return remote.Reply{}, err
	}

	var p struct {
		Operator config.Operator `json:"operator"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return remote.Reply{}, err
	}

	c.operator = p.Operator

	return remote.Reply{Data: []byte(`{}`)}, nil
}

func TestMachineOperatorFlowsThroughBootstrapAndHandoff(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	r := machineOperatorTestRun(t, "prod")
	if err := r.generateSecrets(); err != nil {
		t.Fatal(err)
	}

	operator, err := r.operator()
	if err != nil {
		t.Fatal(err)
	}

	caller := &operatorCaptureCaller{}
	if _, err := r.bootstrapHost(context.Background(), caller); err != nil {
		t.Fatal(err)
	}

	if caller.operator.Name != r.sec.OperatorName ||
		caller.operator.WGPublicKey != operator.WGPublicKey ||
		caller.operator.SSHPublicKey != operator.SSHPublicKey {
		t.Fatalf("bootstrap did not carry the generated operator: sent %+v, generated %+v", caller.operator, operator)
	}

	r.prog.WGPublicKey = "node-wireguard-public-key"
	r.prog.SSHHostKeys = []string{"node-ssh-host-key"}
	r.sec.RecoveryCode = "recovery-code"

	doc, err := r.document()
	if err != nil {
		t.Fatal(err)
	}

	if doc.Operator.Name != r.sec.OperatorName {
		t.Fatalf("handoff document carries operator %q, want %q", doc.Operator.Name, r.sec.OperatorName)
	}

	found := false

	for _, group := range doc.Groups() {
		if group.Title == "Operator "+r.sec.OperatorName {
			found = true
		}
	}

	if !found {
		t.Fatalf("handoff has no group for operator %q", r.sec.OperatorName)
	}
}

func TestRestoreKeepsWorkspaceMachineOperator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	name, err := newMachineOperatorName()
	if err != nil {
		t.Fatal(err)
	}

	wg, _, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	ssh, _, err := transport.GenerateSSHKey("vaire-prod-" + name)
	if err != nil {
		t.Fatal(err)
	}

	if err := workspaces.WriteFile("prod", workspaces.WGKeyFile, []byte(wg+"\n")); err != nil {
		t.Fatal(err)
	}

	if err := workspaces.WriteFile("prod", workspaces.SSHKeyFile, ssh); err != nil {
		t.Fatal(err)
	}

	if err := workspaces.SaveCredentials("prod", workspaces.Credentials{SudoPassword: "sudo-password"}); err != nil {
		t.Fatal(err)
	}

	r := &initRun{
		name: "prod", cfg: config.DefaultCluster(), prog: &workspaces.Progress{Phases: map[string]string{}},
		restore: &restoreRun{ctx: &workspaces.Workspace{Name: "prod", Operator: workspaces.Operator{Name: name}}},
	}
	if err := r.restoreSecrets(); err != nil {
		t.Fatal(err)
	}

	if r.sec.OperatorName != name {
		t.Fatalf("restore changed the operator identity: %q -> %q", name, r.sec.OperatorName)
	}

	saved, err := workspaces.LoadSecrets("prod")
	if err != nil || saved.OperatorName != name {
		t.Fatalf("restore did not persist the workspace operator: %v", err)
	}
}
