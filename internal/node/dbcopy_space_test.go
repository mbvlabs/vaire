package node

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

func TestWorkstationCopySpace(t *testing.T) {
	old := workstationCopyFreeBytes

	t.Cleanup(func() { workstationCopyFreeBytes = old })

	const (
		size = int64(5 << 30)
		need = uint64(13 << 30)
	)
	for _, tc := range []struct {
		free   uint64
		ok     bool
		reason string
	}{{need, true, ""}, {need - 1, true, "insufficient_copy_space"}, {0, false, "copy_space_unavailable"}} {
		workstationCopyFreeBytes = func(path string) (uint64, bool) {
			if path != "/data" {
				t.Errorf("wrong filesystem: %s", path)
			}

			return tc.free, tc.ok
		}

		err := checkWorkstationCopySpace("/data", size)
		if tc.reason == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || reason(t, err) != tc.reason {
			t.Fatalf("got %v, want %s", err, tc.reason)
		} else if strings.Contains(err.Error(), "13958643711") {
			t.Fatalf("free bytes disclosed: %v", err)
		}
	}

	workstationCopyFreeBytes = hostinfo.FreeBytes

	if err := checkWorkstationCopySpace(t.TempDir(), math.MaxInt64/2-(3<<30)); err == nil {
		t.Fatal("real filesystem unexpectedly admitted an exabyte estimate")
	}
}

func TestCopyAdmissionBeforeTopologyWrite(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", false)
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small", "copy_source_bytes": int64(1 << 30)})
	env.Req.Secrets = copyRoleSecrets("newdb")

	fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}, hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)

	oldExists, oldStarts, oldFree := createClusterExists, addDatabaseCheckStarts, workstationCopyFreeBytes

	t.Cleanup(func() {
		createClusterExists, addDatabaseCheckStarts, workstationCopyFreeBytes = oldExists, oldStarts, oldFree
	})

	createClusterExists = func(context.Context, string) bool { return false }
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error { return nil }
	workstationCopyFreeBytes = func(string) (uint64, bool) { return 0, true }

	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	_, err = handleDBCreate(context.Background(), env)
	if err == nil || reason(t, err) != "insufficient_copy_space" {
		t.Fatalf("got %v", err)
	}

	raw, _ := json.Marshal(err)
	if strings.Contains(string(raw), "free_bytes") {
		t.Fatalf("space leaked: %s", raw)
	}

	_, after, err := store.Load()
	if err != nil || string(before) != string(after) {
		t.Fatalf("topology changed: %v", err)
	}
}

func TestCopySourceSizeValidation(t *testing.T) {
	for _, size := range []int64{-1, 0, math.MaxInt64} {
		env := &Env{}

		env.Req = request("db.create", map[string]any{"name": "newdb", "copy_source_bytes": size})
		if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "invalid_copy_size" {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}
