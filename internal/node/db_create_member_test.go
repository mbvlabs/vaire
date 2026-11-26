package node

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

func TestMemberCreateMissingSeedFailsBeforeEffects(t *testing.T) {
	old := memberCreateCredentialCheck
	memberCreateCredentialCheck = func() error { return errors.New("missing") }

	t.Cleanup(func() { memberCreateCredentialCheck = old })

	for _, dry := range []bool{false, true} {
		env := &Env{DeviceIdentity: &rpc.WhoamiReply{Role: config.AccessRoleMember}}
		env.Req = request("db.create", map[string]any{"name": "newdb"})
		env.Req.DryRun = dry
		env.Req.Secrets = copyRoleSecrets("newdb")

		if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "cluster_seed_unavailable" {
			t.Errorf("dry=%v: %v", dry, err)
		}
	}
}

func TestMemberCreateRejectsUnsafeInputs(t *testing.T) {
	for _, dry := range []bool{false, true} {
		for _, key := range append(append([]string{}, steps.StepSecrets...), "db.other.app", "unknown") {
			env := &Env{DeviceIdentity: &rpc.WhoamiReply{Role: config.AccessRoleMember}}
			env.Req = request("db.create", map[string]any{"name": "newdb"})
			env.Req.DryRun = dry
			env.Req.Secrets = map[string]string{key: "injected"}

			if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "device_access_denied" {
				t.Errorf("dry=%v secret %s: %v", dry, key, err)
			}
		}

		env := &Env{DeviceIdentity: &rpc.WhoamiReply{Role: config.AccessRoleMember}}
		env.Req = request("db.create", map[string]any{"name": "newdb", "allow_overcommit": true})
		env.Req.DryRun = dry

		if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "device_access_denied" {
			t.Errorf("dry=%v overcommit: %v", dry, err)
		}
	}
}

func TestMemberCreateProvisionsWithOnlyDatabaseSecrets(t *testing.T) {
	t.Run("empty database", func(t *testing.T) { testMemberCreateProvisions(t, false) })
	t.Run("first database via copy", func(t *testing.T) { testMemberCreateProvisions(t, true) })
}

func testMemberCreateProvisions(t *testing.T, copyDB bool) {
	t.Helper()

	oldSeed := memberCreateCredentialCheck
	memberCreateCredentialCheck = func() error { return nil }

	t.Cleanup(func() { memberCreateCredentialCheck = oldSeed })
	cfg := followerLifecycleCfg(t)
	parent := t.TempDir()
	cfg.Clusters = nil
	cfg.Server.DataRoot = filepath.Join(parent, "postgresql", "18")
	env, store := resizeFixture(t, cfg, "", false)
	env.DeviceIdentity = &rpc.WhoamiReply{Role: config.AccessRoleMember, OperatorID: "op-member"}

	params := map[string]any{"name": "newdb", "size": "small"}
	if copyDB {
		params["copy_source_bytes"] = int64(1 << 30)
	}

	env.Req = request("db.create", params)
	env.Req.Secrets = copyRoleSecrets("newdb")

	fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)

	oldExists, oldStarts := createClusterExists, addDatabaseCheckStarts
	oldKernel, oldNew, oldPoolers := createApplyKernel, createApplyNewCluster, createReloadPoolers
	oldMetrics, oldOthers, oldBackup := createApplyMetrics, createApplyOtherConfigs, createFirstBackup
	oldFree := workstationCopyFreeBytes

	t.Cleanup(func() {
		createClusterExists, addDatabaseCheckStarts = oldExists, oldStarts
		createApplyKernel, createApplyNewCluster, createReloadPoolers = oldKernel, oldNew, oldPoolers
		createApplyMetrics, createApplyOtherConfigs, createFirstBackup = oldMetrics, oldOthers, oldBackup
		workstationCopyFreeBytes = oldFree
	})

	checked := false
	workstationCopyFreeBytes = func(path string) (uint64, bool) {
		checked = true

		if path != parent {
			t.Errorf("checked %q instead of existing parent %q", path, parent)
		}

		return 69 << 30, true
	}

	createClusterExists = func(context.Context, string) bool { return false }
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error { return nil }
	createApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		return engine.ApplyData{}, nil, nil
	}
	called := false
	createApplyNewCluster = func(_ context.Context, _ config.Cluster, rt engine.Runtime, name, _ string, _ engine.Reporter) error {
		called = true

		if name != "newdb" || len(rt.SecretValues) != len(steps.DatabaseRoles) {
			t.Errorf("unexpected provisioning inputs: %s (%d secrets)", name, len(rt.SecretValues))
		}

		for _, role := range steps.DatabaseRoles {
			if rt.SecretValues[steps.SecretDatabaseRole(name, role)] == "" {
				t.Errorf("missing %s role", role)
			}
		}

		return nil
	}
	createReloadPoolers = func(context.Context, *Env, config.Cluster) error { return nil }
	createApplyMetrics = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string, string) *output.Warning {
		return nil
	}
	createApplyOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, []string, engine.Options) []output.Warning {
		return nil
	}
	createFirstBackup = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string) *output.Warning {
		return nil
	}

	result, err := handleDBCreate(context.Background(), env)
	if err != nil || !called || checked != copyDB {
		t.Fatalf("create: provisioned=%v spaceChecked=%v err=%v", called, checked, err)
	}

	db := memberCreatedDB(t, result)
	saved, _, err := store.Load()

	if _, ok := saved.ClusterByName("newdb"); err != nil || !ok || db.Name != "newdb" {
		t.Fatalf("created=%+v saved=%v err=%v", db, ok, err)
	}

	if !saved.DatabaseCreatedByOperator("newdb", "op-member") || saved.DatabaseCreatedByOperator("newdb", "op-other") {
		t.Fatal("database creator must be the authenticated operator")
	}
}

func TestMemberCreateDryRun(t *testing.T) {
	oldSeed := memberCreateCredentialCheck
	memberCreateCredentialCheck = func() error { return nil }

	t.Cleanup(func() { memberCreateCredentialCheck = oldSeed })
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", true)
	env.DeviceIdentity = &rpc.WhoamiReply{Role: config.AccessRoleMember}
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small"})
	env.Req.DryRun = true

	fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)

	oldExists, oldStarts := createClusterExists, addDatabaseCheckStarts

	t.Cleanup(func() { createClusterExists, addDatabaseCheckStarts = oldExists, oldStarts })

	createClusterExists = func(context.Context, string) bool { return false }
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error { return nil }

	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	result, err := handleDBCreate(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	db := memberCreatedDB(t, result)
	if db.Name != "newdb" || db.Port == 0 {
		t.Fatalf("preview: %+v", db)
	}

	_, after, err := store.Load()
	if err != nil || string(before) != string(after) {
		t.Fatalf("preview saved state: %v", err)
	}
}

func memberCreatedDB(t *testing.T, result Result) config.ClusterSpec {
	t.Helper()

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result data type %T", result.Data)
	}

	db, ok := data["database"].(config.ClusterSpec)
	if !ok {
		t.Fatalf("unexpected database type %T", data["database"])
	}

	return db
}
