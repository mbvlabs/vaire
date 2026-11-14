package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mbvlabs/vaire/internal/engine"
)

var errOwnershipTest = errors.New("ownership refused")

type fakeMaterialisationOwner struct {
	events     []string
	data, conf os.FileInfo
	denyCreate string
	denyAction MaterialisationAction
	inGuard    bool
}

func (o *fakeMaterialisationOwner) CreateData(_ context.Context, p MaterialisationPaths) error {
	o.events = append(o.events, "create-data")
	if o.inGuard || o.denyCreate == "data" {
		return errOwnershipTest
	}
	if err := os.Mkdir(p.DataDir, 0o700); err != nil {
		return err
	}
	var err error
	o.data, err = os.Stat(p.DataDir)
	return err
}

func (o *fakeMaterialisationOwner) CreateConfig(_ context.Context, p MaterialisationPaths) error {
	o.events = append(o.events, "create-config")
	if o.inGuard || o.denyCreate == "config" {
		return errOwnershipTest
	}
	if err := o.check(p); err != nil {
		return err
	}
	if err := os.Mkdir(p.ConfigDir, 0o755); err != nil { //nolint:gosec // G301: model postgresql-common configuration mode.
		return err
	}
	var err error
	o.conf, err = os.Stat(p.ConfigDir)
	return err
}

func (o *fakeMaterialisationOwner) check(p MaterialisationPaths) error {
	for _, item := range []struct {
		path string
		info os.FileInfo
	}{{p.DataDir, o.data}, {p.ConfigDir, o.conf}} {
		if item.info == nil {
			continue
		}
		got, err := os.Stat(item.path)
		if err != nil || !os.SameFile(item.info, got) {
			return errOwnershipTest
		}
	}
	return nil
}

func (o *fakeMaterialisationOwner) Guard(_ context.Context, p MaterialisationPaths, action MaterialisationAction, effect func() error) error {
	o.events = append(o.events, "pre:"+string(action))
	if o.inGuard || o.denyAction == action {
		return errOwnershipTest
	}
	if err := o.check(p); err != nil {
		return err
	}
	o.inGuard = true
	err := effect()
	o.inGuard = false
	o.events = append(o.events, "post:"+string(action))
	return errors.Join(err, o.check(p))
}

type ownershipDriver struct {
	Driver
	run func(RestoreOptions) error
}

func (d ownershipDriver) Materialise(_ context.Context, opts RestoreOptions) error {
	return d.run(opts)
}

func ownershipMaterialised(t *testing.T) (*Materialised, *fakeMaterialisationOwner) {
	t.Helper()
	oldConf, oldIDs, oldWrite := pgConfRoot, resolveClusterIDs, writeClusterFile
	oldProbe, oldStart, oldCommand, oldRunning := recoveryProbe, startMaterialisedRecovery, runMaterialisedCommand, materialisedDataRunning
	pgConfRoot = t.TempDir()
	resolveClusterIDs = func(_, _ string) (int, int, error) { return -1, -1, nil }
	writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
		return engine.WriteFileAtomicOwned(path, data, perm, "", "")
	}
	t.Cleanup(func() {
		pgConfRoot, resolveClusterIDs, writeClusterFile = oldConf, oldIDs, oldWrite
		recoveryProbe, startMaterialisedRecovery, runMaterialisedCommand, materialisedDataRunning = oldProbe, oldStart, oldCommand, oldRunning
	})
	src := filepath.Join(pgConfRoot, "source")
	if err := os.MkdirAll(filepath.Join(src, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"postgresql.conf": "port = 5432\n", "pg_ctl.conf": stockPgCtlConf} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o := &fakeMaterialisationOwner{}
	m := &Materialised{Name: "target", Source: "source", Data: filepath.Join(t.TempDir(), "target"), Port: 5555, SourcePort: 5432, Ownership: o}
	values := map[string]string{"max_connections": "30", "max_worker_processes": "8", "max_wal_senders": "10", "max_prepared_transactions": "0", "max_locks_per_transaction": "64"}
	recoveryProbe = func(_ context.Context, args ...string) (string, string, error) {
		if args[0] == pgBin+"/pg_controldata" {
			return controlExcerpt, "", nil
		}
		return values[args[len(args)-1]], "", nil
	}
	startMaterialisedRecovery = func(_ context.Context, _ *Materialised, _ string, _ string) error {
		o.events = append(o.events, "start-effect")
		return nil
	}
	materialisedDataRunning = func(context.Context, string) bool { return false }
	return m, o
}

func TestOwnedRestoreSequence(t *testing.T) {
	m, owner := ownershipMaterialised(t)
	driver := ownershipDriver{run: func(opts RestoreOptions) error {
		if opts.Path != m.Data || owner.data == nil || owner.conf != nil {
			t.Fatalf("restore before binding or after registration: %+v", opts)
		}
		owner.events = append(owner.events, "restore-effect")
		return nil
	}}
	if err := m.Restore(context.Background(), driver, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"create-data", "pre:restore", "restore-effect", "post:restore", "create-config",
		"pre:register", "post:register", "pre:prepare", "post:prepare", "pre:inspect", "post:inspect",
		"pre:start", "start-effect", "post:start"}
	if !reflect.DeepEqual(owner.events, want) {
		t.Fatalf("events=%v\nwant=%v", owner.events, want)
	}
	paths := m.OwnershipPaths()
	if paths.DataDir != m.Data || paths.ConfigDir != m.ConfDir() || paths.Port != 5555 || paths.SourcePort != 5432 || paths.Name != "target" || paths.Source != "source" {
		t.Fatalf("wrong ownership identity: %+v", paths)
	}
}

func TestOwnedCreateAndGuardFailuresDoNotRunEffects(t *testing.T) {
	for _, stage := range []string{"data", "config", "restore", "register", "inspect", "start"} {
		t.Run(stage, func(t *testing.T) {
			m, owner := ownershipMaterialised(t)
			if stage == "data" || stage == "config" {
				owner.denyCreate = stage
			} else {
				owner.denyAction = MaterialisationAction(stage)
			}
			restored := false
			err := m.Restore(context.Background(), ownershipDriver{run: func(RestoreOptions) error {
				restored = true
				return nil
			}}, RestoreOptions{})
			if !errors.Is(err, errOwnershipTest) {
				t.Fatalf("ownership error hidden: %v", err)
			}
			if (stage == "data" || stage == "restore") && restored {
				t.Fatal("restore ran before binding/guard")
			}
			for _, ev := range owner.events {
				if ev == "start-effect" {
					t.Fatalf("start ran after denied %s", stage)
				}
			}
		})
	}
}

func TestOwnedDataReplacementNeverRebound(t *testing.T) {
	m, owner := ownershipMaterialised(t)
	err := m.Restore(context.Background(), ownershipDriver{run: func(RestoreOptions) error {
		if err := os.Rename(m.Data, m.Data+".original"); err != nil {
			return err
		}
		return os.Mkdir(m.Data, 0o700)
	}}, RestoreOptions{})
	if !errors.Is(err, errOwnershipTest) || owner.conf != nil {
		t.Fatalf("replacement not refused before registration: %v", err)
	}
	want := []string{"create-data", "pre:restore", "post:restore"}
	if !reflect.DeepEqual(owner.events, want) {
		t.Fatalf("replacement events: %v", owner.events)
	}
}

func TestOwnedExclusiveCreationRefusesExisting(t *testing.T) {
	m, _ := ownershipMaterialised(t)
	if err := os.Mkdir(m.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	err := m.Restore(context.Background(), ownershipDriver{run: func(RestoreOptions) error { called = true; return nil }}, RestoreOptions{})
	if !errors.Is(err, os.ErrExist) || called {
		t.Fatalf("existing data: err=%v called=%v", err, called)
	}
}

func TestOwnedExistingConfigRefusedBeforeRegistration(t *testing.T) {
	m, owner := ownershipMaterialised(t)
	if err := os.Mkdir(m.ConfDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	err := m.Restore(context.Background(), ownershipDriver{run: func(RestoreOptions) error { return nil }}, RestoreOptions{})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing config was not refused: %v", err)
	}
	for _, event := range owner.events {
		if event == "pre:register" || event == "start-effect" {
			t.Fatalf("registration or start ran: %v", owner.events)
		}
	}
}

func TestOwnedRestoreEffectFailureStillChecksAndRetains(t *testing.T) {
	m, owner := ownershipMaterialised(t)
	failure := errors.New("tool failed")
	err := m.Restore(context.Background(), ownershipDriver{run: func(RestoreOptions) error { return failure }}, RestoreOptions{})
	if !errors.Is(err, failure) || !reflect.DeepEqual(owner.events, []string{"create-data", "pre:restore", "post:restore"}) {
		t.Fatalf("effect failure: err=%v events=%v", err, owner.events)
	}
	if _, err := os.Stat(m.Data); err != nil {
		t.Fatalf("failure silently removed owned data: %v", err)
	}
}

func TestOwnedBaseBackupChecksToolAndRegistration(t *testing.T) {
	m, owner := ownershipMaterialised(t)
	runMaterialisedCommand = func(_ context.Context, _ *Materialised, cmd engine.Command) error {
		if !owner.inGuard || cmd.Name != "runuser" || owner.data == nil {
			return fmt.Errorf("base backup outside guard")
		}
		owner.events = append(owner.events, "base-backup-effect")
		return nil
	}
	owner.denyAction = MaterialisationStart
	err := m.BaseBackup(context.Background())
	if !errors.Is(err, errOwnershipTest) {
		t.Fatalf("start denial: %v", err)
	}
	want := []string{"create-data", "pre:base_backup", "base-backup-effect", "post:base_backup",
		"create-config", "pre:register", "post:register", "pre:start"}
	if !reflect.DeepEqual(owner.events, want) {
		t.Fatalf("events=%v want=%v", owner.events, want)
	}
}

func TestOwnedBaseBackupDetectsReplacementAfterTool(t *testing.T) {
	m, owner := ownershipMaterialised(t)
	runMaterialisedCommand = func(_ context.Context, _ *Materialised, _ engine.Command) error {
		if err := os.Rename(m.Data, m.Data+".original"); err != nil {
			return err
		}
		return os.Mkdir(m.Data, 0o700)
	}
	if err := m.BaseBackup(context.Background()); !errors.Is(err, errOwnershipTest) {
		t.Fatalf("post-tool replacement: %v", err)
	}
	if owner.conf != nil {
		t.Fatal("configuration bound after data replacement")
	}
}

func TestOwnedPromotionRenameAdoptAndDropRefuseWithoutEffects(t *testing.T) {
	m, owner := ownershipMaterialised(t)
	if err := owner.CreateData(context.Background(), m.OwnershipPaths()); err != nil {
		t.Fatal(err)
	}
	if err := owner.CreateConfig(context.Background(), m.OwnershipPaths()); err != nil {
		t.Fatal(err)
	}
	owner.denyAction = MaterialisationPromote
	if _, err := m.Promote(context.Background()); !errors.Is(err, errOwnershipTest) {
		t.Fatalf("promote denial: %v", err)
	}
	owner.denyAction = MaterialisationRename
	renamed := false
	if err := m.GuardOwnership(context.Background(), MaterialisationRename, func() error { renamed = true; return nil }); !errors.Is(err, errOwnershipTest) || renamed {
		t.Fatalf("rename denial: %v renamed=%v", err, renamed)
	}
	owner.denyAction = MaterialisationAdopt
	if err := m.Adopt(context.Background(), 5556); !errors.Is(err, errOwnershipTest) || m.Port != 5555 {
		t.Fatalf("adoption denial: %v port=%d", err, m.Port)
	}
	owner.denyAction = MaterialisationDrop
	if err := m.Drop(); !errors.Is(err, errOwnershipTest) {
		t.Fatalf("drop denial: %v", err)
	}
	if _, err := os.Stat(m.Data); err != nil {
		t.Fatalf("denied drop removed data: %v", err)
	}
}

func TestNilOwnershipPreservesCallbackBehavior(t *testing.T) {
	m := &Materialised{}
	calls := 0
	failure := errors.New("effect failed")
	err := m.GuardOwnership(context.Background(), MaterialisationRename, func() error {
		calls++
		return failure
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("nil ownership: err=%v calls=%d", err, calls)
	}
}
