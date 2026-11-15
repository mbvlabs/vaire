package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func bootstrapOwnershipFixture(t *testing.T) config.Cluster {
	t.Helper()
	oldDir, oldNode, oldCluster := bootstrapStateDir, bootstrapNodePath, bootstrapClusterPath
	oldUID, oldConf, oldInstance := bootstrapExpectedUID, bootstrapConfDir, bootstrapInstance
	root := t.TempDir()
	bootstrapStateDir = filepath.Join(root, "state")
	bootstrapNodePath = filepath.Join(root, "node.toml")
	bootstrapClusterPath = filepath.Join(root, "cluster.toml")
	bootstrapExpectedUID = uint32(os.Geteuid())
	bootstrapConfDir = func(name string) string { return filepath.Join(root, "postgres", name) }
	bootstrapInstance = func() (string, error) {
		raw, err := os.ReadFile(filepath.Join(bootstrapStateDir, "instance"))
		return strings.TrimSpace(string(raw)), err
	}
	t.Cleanup(func() {
		bootstrapStateDir, bootstrapNodePath, bootstrapClusterPath = oldDir, oldNode, oldCluster
		bootstrapExpectedUID, bootstrapConfDir, bootstrapInstance = oldUID, oldConf, oldInstance
	})
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Server.DataRoot = filepath.Join(root, "data")
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Size: "small"}}
	return cfg
}

func TestCompletedServerBootstrapRPCDoesNotAcquireAccessAuthority(t *testing.T) {
	_ = bootstrapOwnershipFixture(t)
	cfg := copyTestConfig(t)
	raw, err := cfg.Encode()
	reject(t, err != nil, err)
	if err := bootstrapDir(true); err != nil {
		t.Fatal(err)
	}
	oldLoadNode := bootstrapLoadNode
	bootstrapLoadNode = func(string) (config.Node, error) {
		return config.Node{Server: cfg.Name(), NodeName: "db1"}, nil
	}
	t.Cleanup(func() { bootstrapLoadNode = oldLoadNode })
	params, err := json.Marshal(bootstrapParams{Phase: PhaseDatabase, ClusterTOML: string(raw), NodeName: "db1"})
	reject(t, err != nil, err)
	env := &Env{Req: rpc.Request{Method: "cluster.bootstrap", Confirm: cfg.Name(), OperationID: "new-request", Params: params}}
	_, err = handleClusterBootstrap(context.Background(), env)
	var cli *output.CLIError
	rejectf(t, !errors.As(err, &cli) || cli.Code != output.CodePrecondition, "completed server acquired bootstrap access authority: %v", err)
	details, _ := cli.Details.(map[string]any)
	rejectf(t, details["reason"] != "bootstrap_ownership_unavailable", "wrong refusal: %+v", cli)
}

func TestBootstrapOwnershipFreshResumeAndCompletion(t *testing.T) {
	cfg := bootstrapOwnershipFixture(t)
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err != nil {
		t.Fatal(err)
	}
	reject(t, masterbackup.Allowed("/var/lib/vaire/"+bootstrapManifestName), "node-local bootstrap authority entered master backup allowlist")
	owner, err := bootstrapReadOwnership()
	rejectf(t, err != nil || owner.Targets["app"], "fresh manifest %+v: %v", owner, err)
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err != nil {
		t.Fatalf("original setup cannot resume: %v", err)
	}
	for _, pair := range [][2]string{{"db1", "other"}, {"foreign", "setup-1"}} {
		if err := bootstrapValidateOwnership(owner, cfg, pair[0], pair[1]); err == nil {
			t.Fatalf("foreign identity accepted: %v", pair)
		}
	}
	changed := cfg
	changed.Clusters = slices.Clone(cfg.Clusters)
	changed.Clusters[0].Size = "large"
	reject(t, bootstrapValidateOwnership(owner, changed, "db1", "setup-1") == nil, "changed target specification accepted")
	if err := bootstrapCompleteTarget(&owner, cfg, "db1", "setup-1", "app"); err != nil {
		t.Fatal(err)
	}
	reject(t, !owner.Targets["app"] || bootstrapCompleteTarget(&owner, cfg, "db1", "setup-1", "app") == nil, "completed target retained one-time ownership")
	if err := os.WriteFile(filepath.Join(bootstrapStateDir, "instance"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	reject(t, bootstrapValidateOwnership(owner, cfg, "db1", "setup-1") == nil, "foreign instance accepted")
}

func TestBootstrapOwnershipRefusesExistingAndCorrupt(t *testing.T) {
	cfg := bootstrapOwnershipFixture(t)
	if err := os.WriteFile(bootstrapNodePath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err == nil {
		t.Fatal("existing node was claimed")
	}
	if _, err := os.Stat(filepath.Join(bootstrapStateDir, bootstrapManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing node received manifest: %v", err)
	}
	if err := os.Remove(bootstrapNodePath); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bootstrapStateDir, bootstrapManifestName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err == nil {
		t.Fatal("corrupt ownership was reconstructed")
	}
}

func TestBootstrapOwnershipRejectsForeignTargetAndResumesOriginalInstance(t *testing.T) {
	cfg := bootstrapOwnershipFixture(t)
	target := cfg.Server.DataDirectory("app")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err == nil {
		t.Fatal("existing target was claimed")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err != nil {
		t.Fatal(err)
	}
	owner, err := bootstrapReadOwnership()
	reject(t, err != nil, err)
	if err := os.Remove(filepath.Join(bootstrapStateDir, "instance")); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "other-setup"); err == nil {
		t.Fatal("foreign setup recreated the instance")
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err != nil {
		t.Fatalf("original pre-exposure setup did not resume: %v", err)
	}
	instance, err := bootstrapInstance()
	rejectf(t, err != nil || instance != owner.Instance, "resume changed original instance: %q %v", instance, err)
	if err := bootstrapCompleteTarget(&owner, cfg, "db1", "setup-1", "app"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bootstrapStateDir, "instance")); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err == nil {
		t.Fatal("completed ownership recreated its instance")
	}
}

func TestBootstrapStateDirectoryIsNotRepaired(t *testing.T) {
	cfg := bootstrapOwnershipFixture(t)
	if err := os.Mkdir(bootstrapStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareBootstrapOwnership(cfg, "db1", "setup-1"); err == nil {
		t.Fatal("unsafe state directory accepted")
	}
	info, err := os.Stat(bootstrapStateDir)
	rejectf(t, err != nil || info.Mode().Perm() != 0o755, "existing state directory was modified: %v %v", info, err)
}

func TestCompletedBootstrapTargetCannotReauthorizeDrift(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}}}
	state := &provisionState{}
	catalog := []engine.Step{
		provisionStep{id: "postgres-roles-v1", state: state},
		provisionAccess{provisionStep{id: "postgres-access-v1", state: state}},
		bootstrapGlobal{id: "pgbouncer-v1"},
	}
	owner := &bootstrapOwnership{Targets: map[string]bool{"app": true}}
	oldView := engine.ClusterView
	engine.ClusterView = nil
	t.Cleanup(func() { engine.ClusterView = oldView })
	path := filepath.Join(t.TempDir(), "journal")
	runner := func(ctx context.Context, steps []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		rejectf(t, len(opts.InternalAccess) != 0, "completed target received internal access: %+v", opts.InternalAccess)
		opts.JournalPath = path
		return engine.Apply(ctx, steps, cfg, rt, opts)
	}
	_, _, err := bootstrapDatabaseSteps(context.Background(), cfg, engine.Runtime{}, nil, catalog, runner, nil, "", owner, func(string) error {
		t.Fatal("completed target was marked again")
		return nil
	})
	rejectf(t, err == nil || slices.Contains(state.events, "grant"), "completed server widened drift: events %v, err %v", state.events, err)
}

func TestBootstrapCompletionFailurePreventsSharedRouting(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}}}
	state := &provisionState{}
	catalog := []engine.Step{
		provisionStep{id: "postgres-roles-v1", state: state},
		provisionAccess{provisionStep{id: "postgres-access-v1", state: state}},
		bootstrapGlobal{id: "pgbouncer-v1"},
	}
	owner := &bootstrapOwnership{Targets: map[string]bool{"app": false}}
	oldView := engine.ClusterView
	engine.ClusterView = nil
	t.Cleanup(func() { engine.ClusterView = oldView })
	var selected []string
	runner := func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		selected = append(selected, opts.Only...)
		return engine.ApplyData{}, nil, nil
	}
	_, _, err := bootstrapDatabaseSteps(context.Background(), cfg, engine.Runtime{}, nil, catalog, runner, nil, "", owner,
		func(string) error { return errors.New("cannot persist completion") })
	rejectf(t, err == nil || slices.Contains(selected, "pgbouncer-v1"), "shared routing ran after failed completion: %v, %v", selected, err)
}
