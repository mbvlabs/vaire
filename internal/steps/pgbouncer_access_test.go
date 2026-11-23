package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestManagedAccessPoolerHBA(t *testing.T) {
	oldOps, oldAccess := loadPoolerOperators, loadPoolerAccess

	t.Cleanup(func() { loadPoolerOperators, loadPoolerAccess = oldOps, oldAccess })

	loadPoolerOperators = func() (config.Operators, error) {
		return config.Operators{Operators: []config.Operator{{WGAddress: "10.42.0.2"}}}, nil
	}
	loadPoolerAccess = func() (config.AccessRegistry, error) {
		return config.AccessRegistry{
			Operators: []config.AccessOperator{{ID: "ok"}, {ID: "revoked", Revoked: true}},
			Devices: []config.AccessDevice{
				{ID: "active", OperatorID: "ok", State: config.DeviceActive, UnixUID: "1000", Address: "10.42.0.3", TemporaryAddress: "10.42.0.4"},
				{ID: "pending", OperatorID: "ok", State: config.DevicePending, Address: "10.42.0.5"},
				{ID: "revoked", OperatorID: "ok", State: config.DeviceRevoked, UnixUID: "1001", Address: "10.42.0.6"},
				{ID: "unverified", OperatorID: "ok", State: config.DeviceActive, Address: "10.42.0.7"},
				{ID: "revoked-owner", OperatorID: "revoked", State: config.DeviceActive, UnixUID: "1002", Address: "10.42.0.8"},
			},
		}, nil
	}
	cfg := config.DefaultCluster()
	cfg.Transport.Subnet = "10.42.0.0/16"
	cfg.Nodes = []config.NodeSpec{{}}

	peers, err := poolerOperators(cfg)
	if err != nil {
		t.Fatal(err)
	}

	files := pgbouncerFilesFrom(cfg, engine.Runtime{}, backup.Records{}, peers)
	for _, file := range []pgbouncerFile{files[1], files[3]} {
		for _, want := range []string{"10.42.0.2/32", "10.42.0.3/32"} {
			if !strings.Contains(file.content, want) {
				t.Errorf("%s missing %s", file.path, want)
			}
		}

		for _, absent := range []string{"10.42.0.4/32", "10.42.0.5/32", "10.42.0.6/32", "10.42.0.7/32", "10.42.0.8/32"} {
			if strings.Contains(file.content, absent) {
				t.Errorf("%s includes %s", file.path, absent)
			}
		}
	}

	loadPoolerAccess = func() (config.AccessRegistry, error) {
		return config.AccessRegistry{
			Operators: []config.AccessOperator{{ID: "ok"}},
			Devices:   []config.AccessDevice{{ID: "outside", OperatorID: "ok", State: config.DeviceActive, UnixUID: "1000", Address: "192.0.2.1"}},
		}, nil
	}

	if _, err := poolerOperators(cfg); err == nil {
		t.Fatal("out-of-subnet device accepted")
	}

	loadPoolerAccess = func() (config.AccessRegistry, error) { return config.AccessRegistry{}, errors.New("bad registry") }

	if _, err := poolerOperators(cfg); err == nil {
		t.Fatal("unreadable access registry accepted")
	}
}

func TestRefreshAccessPoolerHBA(t *testing.T) {
	oldRecords, oldPaths := backup.RecordsPath, poolerFilePaths
	oldOps, oldAccess, oldExposure := loadPoolerOperators, loadPoolerAccess, poolerExposure
	oldState, oldReload, oldWrite := poolerServiceState, reloadAccessPooler, writePoolerFile

	t.Cleanup(func() {
		backup.RecordsPath, poolerFilePaths = oldRecords, oldPaths
		loadPoolerOperators, loadPoolerAccess, poolerExposure = oldOps, oldAccess, oldExposure
		poolerServiceState, reloadAccessPooler, writePoolerFile = oldState, oldReload, oldWrite
	})
	dir := t.TempDir()
	backup.RecordsPath = filepath.Join(dir, "records")
	poolerFilePaths.ini = filepath.Join(dir, "transaction.ini")
	poolerFilePaths.hba = filepath.Join(dir, "transaction.hba")
	poolerFilePaths.sessionIni = filepath.Join(dir, "session.ini")
	poolerFilePaths.sessionHBA = filepath.Join(dir, "session.hba")
	poolerFilePaths.sessionUnit = filepath.Join(dir, "unit")
	loadPoolerOperators = func() (config.Operators, error) { return config.Operators{}, nil }
	loadPoolerAccess = func() (config.AccessRegistry, error) { return config.AccessRegistry{}, nil }
	poolerExposure = func(_ config.Cluster, f func() error) error { return f() }
	cfg, rt := config.DefaultCluster(), engine.Runtime{}
	cfg.Clusters = []config.ClusterSpec{{Name: "sample", Port: 5433}}

	files := pgbouncerFilesFrom(cfg, rt, backup.Records{}, nil)
	for _, f := range []pgbouncerFile{files[0], files[2]} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var reloaded, written []string

	poolerServiceState = func(_ context.Context, unit string) (string, error) {
		if unit == PgbouncerService {
			return "active", nil
		}

		return "inactive", nil
	}
	reloadAccessPooler = func(_ context.Context, unit string) error { reloaded = append(reloaded, unit); return nil }
	writePoolerFile = func(path, content string, _ os.FileMode, _, _ string) error {
		written = append(written, path)
		return os.WriteFile(path, []byte(content), 0o600)
	}

	if err := RefreshAccessPoolerHBA(context.Background(), cfg, rt); err != nil {
		t.Fatal(err)
	}

	if len(written) != 2 || len(reloaded) != 1 || reloaded[0] != PgbouncerService {
		t.Fatalf("writes %v reloads %v", written, reloaded)
	}

	if err := os.WriteFile(poolerFilePaths.sessionIni, []byte("stale route"), 0o600); err != nil {
		t.Fatal(err)
	}

	written, reloaded = nil, nil
	if err := RefreshAccessPoolerHBA(context.Background(), cfg, rt); err == nil || len(written) != 0 || len(reloaded) != 0 {
		t.Fatalf("stale ini: error %v, writes %v reloads %v", err, written, reloaded)
	}

	empty := cfg
	empty.Clusters = nil

	if err := RefreshAccessPoolerHBA(context.Background(), empty, rt); err != nil || len(written) != 0 || len(reloaded) != 0 {
		t.Fatalf("empty node must leave stock poolers alone: error %v, writes %v reloads %v", err, written, reloaded)
	}

	if err := os.Remove(poolerFilePaths.ini); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(poolerFilePaths.sessionIni); err != nil {
		t.Fatal(err)
	}

	poolerServiceState = func(context.Context, string) (string, error) { return "inactive", nil }

	if err := RefreshAccessPoolerHBA(context.Background(), cfg, rt); err != nil || len(written) != 0 || len(reloaded) != 0 {
		t.Fatalf("uninstalled poolers: error %v, writes %v reloads %v", err, written, reloaded)
	}
}
