package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func materialisationFixture(t *testing.T) (materialisationOwnership, string) {
	t.Helper()
	oldDir, oldInstance := materialisationStateDir, followHostInstance
	oldUID := materialisationRecordUID
	oldConfigRoot := materialisationConfigRoot
	oldBase := materialisationAncestorBase
	oldPostgresUID := materialisationPostgresUID
	base := t.TempDir()
	materialisationAncestorBase = base
	materialisationStateDir = filepath.Join(base, "state")
	materialisationConfigRoot = filepath.Join(base, "config")
	if err := os.Mkdir(materialisationStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(materialisationConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	materialisationRecordUID = uint32(os.Geteuid()) //nolint:gosec // test runs with its own effective user
	materialisationPostgresUID = func() (uint32, error) { return materialisationRecordUID, nil }
	followHostInstance = func() (string, error) { return "install", nil }
	t.Cleanup(func() {
		materialisationStateDir, followHostInstance, materialisationRecordUID, materialisationConfigRoot = oldDir, oldInstance, oldUID, oldConfigRoot
		materialisationAncestorBase = oldBase
		materialisationPostgresUID = oldPostgresUID
	})
	root := filepath.Join(base, "data")
	return materialisationOwnership{
		Operation: "operation", Job: "job", Instance: "install", Kind: materialisationFork,
		Source: "source", Target: "target", Stanza: "source-abcdef",
		IntendedPort: 5433,
		DataPath:     filepath.Join(root, "target"), RegisteredPath: filepath.Join(materialisationConfigRoot, "target"),
		Phase: materialisationPrepared,
	}, root
}

func reserveTestMaterialisation(t *testing.T, a *materialisationArtifacts) {
	t.Helper()
	err := a.bindMaterialisationReservation(config.TemporaryPortMin, func(_ materialisationOwnership, _ int, finish func() error) error {
		return finish()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMaterialisationOwnershipRecord(t *testing.T) {
	o, root := materialisationFixture(t)
	c, err := createMaterialisation(o, root)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := materialisationPath(o.Target)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", info, err)
	}
	if _, err := createMaterialisation(o, root); err == nil {
		t.Fatal("replaced existing record")
	}
	got, found, err := readMaterialisation(o.Target, root)
	if err != nil || !found || got != o {
		t.Fatalf("read: %+v %v %v", got, found, err)
	}
	list, err := listMaterialisations(root)
	if err != nil || len(list) != 1 || list[0] != o {
		t.Fatalf("list: %+v %v", list, err)
	}
	if c != nil || ownsMaterialisation(context.Background(), o) ||
		ownsMaterialisation(materialisationContext(context.Background(), &materialisationCapability{owner: o}), o) {
		t.Fatal("issued authority without a locked active job")
	}
	other := o
	other.Job = "another-job"
	if ownsMaterialisation(materialisationContext(context.Background(), c), other) {
		t.Fatal("another job obtained authority")
	}
	if err := advanceMaterialisation(&materialisationCapability{owner: o}, root, materialisationCopying, ""); err == nil {
		t.Fatal("accepted forged authority")
	}
}

func TestMaterialisationRecordRejectsUnsafeFile(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	p, _ := materialisationPath(o.Target)
	if err := os.Chmod(p, 0o644); err != nil { //nolint:gosec // intentionally unsafe mode
		t.Fatal(err)
	}
	if _, _, err := readMaterialisation(o.Target, root); err == nil {
		t.Fatal("accepted wrong mode")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	materialisationRecordUID++
	if _, _, err := readMaterialisation(o.Target, root); err == nil {
		t.Fatal("accepted wrong owner")
	}
	materialisationRecordUID--
	other := p + ".old"
	if err := os.Rename(p, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readMaterialisation(o.Target, root); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestMaterialisationLongInternalDrillName(t *testing.T) {
	o, root := materialisationFixture(t)
	o.Source = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabc"
	o.Target = o.Source + "__drill"
	o.Kind = materialisationDrill
	o.DataPath = filepath.Join(root, o.Target)
	o.RegisteredPath = filepath.Join(materialisationConfigRoot, o.Target)
	if err := validateMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
}

func TestMaterialisationArtifactBindingAndReplacement(t *testing.T) {
	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	reserveTestMaterialisation(t, a)
	if err := a.bindCreated(false); err != nil {
		t.Fatal(err)
	}
	if err := a.bindCreated(false); err == nil {
		t.Fatal("rebound data")
	}
	if err := a.bindCreated(true); err != nil {
		t.Fatal(err)
	}
	if err := a.withOwnedArtifacts(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	old := o.DataPath + "-old"
	if err := os.Rename(o.DataPath, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(o.DataPath, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := a.withOwnedArtifacts(func() error { called = true; return nil }); err == nil || called {
		t.Fatal("replacement accepted")
	}
	a.close()
	if _, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID); err == nil {
		t.Fatal("replacement adopted")
	}
}

func TestMaterialisationConfigUsesServiceOwnership(t *testing.T) {
	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	serviceUID := materialisationRecordUID + 1
	materialisationPostgresUID = func() (uint32, error) { return serviceUID, nil }
	oldID, oldConfigure := materialisationArtifactID, materialisationConfigureArtifact
	t.Cleanup(func() { materialisationArtifactID, materialisationConfigureArtifact = oldID, oldConfigure })
	owners := map[uint64]uint32{}
	materialisationArtifactID = func(info os.FileInfo) (materialisationFileID, error) {
		id, err := oldID(info)
		if uid, ok := owners[id.Inode]; ok {
			id.UID = uid
		}
		return id, err
	}
	materialisationConfigureArtifact = func(f *os.File, uid uint32, mode os.FileMode) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		id, err := oldID(info)
		if err != nil {
			return err
		}
		owners[id.Inode] = uid
		return f.Chmod(mode)
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	a, err := openMaterialisationArtifacts(o.Target, root, serviceUID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	reserveTestMaterialisation(t, a)
	if err := a.bindCreated(false); err != nil {
		t.Fatal(err)
	}
	if err := a.bindCreated(true); err != nil {
		t.Fatal(err)
	}
	if a.owner.DataIdentity.UID != serviceUID || a.owner.ConfigIdentity.UID != serviceUID ||
		a.owner.ConfigIdentity.UID == materialisationRecordUID {
		t.Fatal("artifact owner does not match distinct postgres identity")
	}
	data, err := os.Stat(o.DataPath)
	if err != nil || data.Mode().Perm() != 0o700 {
		t.Fatalf("data permissions: %v %v", data, err)
	}
	conf, err := os.Stat(o.RegisteredPath)
	if err != nil || conf.Mode().Perm() != 0o755 {
		t.Fatalf("config permissions: %v %v", conf, err)
	}
	if err := a.withOwnedArtifacts(func() error {
		// Registration leaves the root-created config directory owned by
		// postgres and world-readable, as the backup implementation requires.
		return os.Chmod(o.RegisteredPath, 0o755) //nolint:gosec // postgres-common config directory
	}); err != nil {
		t.Fatalf("legitimate registration invalidated binding: %v", err)
	}
	owners[a.owner.ConfigIdentity.Inode] = serviceUID + 1
	if err := a.withOwnedArtifacts(func() error { t.Fatal("effect on foreign config"); return nil }); err == nil {
		t.Fatal("unrelated config owner accepted")
	}
	owners[a.owner.ConfigIdentity.Inode] = serviceUID
	bad := a.owner
	bad.ConfigIdentity.UID = materialisationRecordUID
	if err := validateMaterialisation(bad, root); err == nil {
		t.Fatal("root-owned config record accepted for distinct postgres uid")
	}
}

func TestMaterialisationArtifactExistingAndParentReplacement(t *testing.T) {
	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(o.DataPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID); err == nil {
		t.Fatal("unbound artifact accepted")
	}
	if err := os.Remove(o.DataPath); err != nil {
		t.Fatal(err)
	}
	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	reserveTestMaterialisation(t, a)
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.bindCreated(false); err == nil {
		t.Fatal("replaced parent accepted")
	}
}

func TestMaterialisationArtifactConfigAndRecordReplacement(t *testing.T) {
	for _, replace := range []string{"config", "record"} {
		t.Run(replace, func(t *testing.T) {
			o, root := materialisationFixture(t)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := createMaterialisation(o, root); err != nil {
				t.Fatal(err)
			}
			a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
			if err != nil {
				t.Fatal(err)
			}
			defer a.close()
			reserveTestMaterialisation(t, a)
			if err := a.bindCreated(false); err != nil {
				t.Fatal(err)
			}
			if err := a.bindCreated(true); err != nil {
				t.Fatal(err)
			}
			if replace == "config" {
				if err := os.Rename(o.RegisteredPath, o.RegisteredPath+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(o.RegisteredPath, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				p, _ := materialisationPath(o.Target)
				raw, err := os.ReadFile(p) //nolint:gosec // test-owned temporary record
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(p, p+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, raw, 0o600); err != nil { //nolint:gosec // test-owned temporary record
					t.Fatal(err)
				}
			}
			if err := a.withOwnedArtifacts(func() error { return nil }); err == nil {
				t.Fatal("replacement accepted")
			}
		})
	}
}

func TestMaterialisationPartialRegistrationRetainsDataBinding(t *testing.T) {
	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	reserveTestMaterialisation(t, a)
	if err := a.bindCreated(false); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, o.RegisteredPath); err != nil {
		t.Fatal(err)
	}
	if err := a.bindCreated(true); err == nil {
		t.Fatal("existing registration accepted")
	}
	got, found, err := readMaterialisation(o.Target, root)
	if err != nil || !found || got.ArtifactStage != materialisationDataCreated || got.DataIdentity.Inode == 0 {
		t.Fatalf("data binding lost: %+v %v %v", got, found, err)
	}
}

func TestMaterialisationRetirement(t *testing.T) {
	for _, mode := range []string{"cleanup", "provisioned", "unverified-reservation", "cleanup-leaves-artifact"} {
		t.Run(mode, func(t *testing.T) {
			o, root := materialisationFixture(t)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := createMaterialisation(o, root); err != nil {
				t.Fatal(err)
			}
			a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
			if err != nil {
				t.Fatal(err)
			}
			defer a.close()
			reserveTestMaterialisation(t, a)
			if err := a.bindCreated(false); err != nil {
				t.Fatal(err)
			}
			if err := a.bindCreated(true); err != nil {
				t.Fatal(err)
			}
			guard := func(got materialisationOwnership) (func(), error) {
				if got != a.owner {
					t.Fatal("wrong reservation tuple")
				}
				return func() {}, nil
			}
			switch mode {
			case "provisioned":
				if err := a.markMaterialisationProvisioned(func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				err = a.retireProvisioned(guard)
			case "unverified-reservation":
				if err := a.markMaterialisationProvisioned(func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				err = a.retireProvisioned(func(materialisationOwnership) (func(), error) { return nil, nil })
			default:
				err = a.retireCleaned(func() error {
					if err := os.Remove(o.DataPath); err != nil {
						return err
					}
					if mode == "cleanup-leaves-artifact" {
						return nil
					}
					return os.Remove(o.RegisteredPath)
				}, guard)
			}
			wantError := mode == "unverified-reservation" || mode == "cleanup-leaves-artifact"
			if (err != nil) != wantError {
				t.Fatalf("retirement: %v", err)
			}
			_, found, readErr := readMaterialisation(o.Target, root)
			if readErr != nil || found != wantError {
				t.Fatalf("record found=%v err=%v", found, readErr)
			}
		})
	}
}

func TestMaterialisationReservationBinding(t *testing.T) {
	o, root := materialisationFixture(t)
	o.TemporaryPort = 0
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := a.bindCreated(false); err == nil {
		t.Fatal("created without reservation")
	}
	if err := a.bindMaterialisationReservation(config.TemporaryPortMin, func(materialisationOwnership, int, func() error) error { return nil }); err == nil {
		t.Fatal("accepted unverified reservation")
	}
	err = a.bindMaterialisationReservation(config.TemporaryPortMin, func(got materialisationOwnership, port int, finish func() error) error {
		if got != o || port != config.TemporaryPortMin {
			t.Fatal("wrong reservation tuple")
		}
		return finish()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.bindCreated(false); err != nil {
		t.Fatal(err)
	}
}

func TestMaterialisationRetirementCommitPoint(t *testing.T) {
	for _, mode := range []string{"prevalidation", "parentsync", "unlink", "success"} {
		t.Run(mode, func(t *testing.T) {
			o, root := materialisationFixture(t)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := createMaterialisation(o, root); err != nil {
				t.Fatal(err)
			}
			a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
			if err != nil {
				t.Fatal(err)
			}
			defer a.close()
			reserveTestMaterialisation(t, a)
			if err := a.bindCreated(false); err != nil {
				t.Fatal(err)
			}
			if err := a.bindCreated(true); err != nil {
				t.Fatal(err)
			}
			p, _ := materialisationPath(o.Target)
			raw, err := os.ReadFile(p) //nolint:gosec // test-owned record
			if err != nil {
				t.Fatal(err)
			}
			oldRemove, oldSync := materialisationRemoveRecord, materialisationSyncArtifactParent
			t.Cleanup(func() { materialisationRemoveRecord, materialisationSyncArtifactParent = oldRemove, oldSync })
			synced, removed, unlocked, cleanupCalled := 0, false, false, false
			materialisationRemoveRecord = func(path string) error {
				removed = true
				if mode == "unlink" {
					return errors.New("injected unlink failure")
				}
				return os.Remove(path)
			}
			materialisationSyncArtifactParent = func(parent *os.Root) error {
				if removed {
					t.Fatal("fallible parent sync after commit")
				}
				synced++
				if mode == "parentsync" {
					return errors.New("injected parent sync failure")
				}
				return oldSync(parent)
			}
			guard := func(materialisationOwnership) (func(), error) {
				if mode == "prevalidation" {
					return nil, errors.New("reservation mismatch")
				}
				return func() { unlocked = true }, nil
			}
			err = a.retireCleaned(func() error {
				cleanupCalled = true
				if err := os.Remove(o.DataPath); err != nil {
					return err
				}
				return os.Remove(o.RegisteredPath)
			}, guard)
			if mode == "success" {
				if err != nil || !removed || !unlocked || synced != 2 {
					t.Fatalf("success: %v removed=%v unlock=%v sync=%d", err, removed, unlocked, synced)
				}
				// A crash can restore the un-fsynced marker. Its original
				// artifacts are gone, so it remains visible but unusable.
				if err := os.WriteFile(p, raw, 0o600); err != nil { //nolint:gosec // test-owned record
					t.Fatal(err)
				}
				if _, found, err := readMaterialisation(o.Target, root); err != nil || !found {
					t.Fatalf("resurrected marker: %v %v", found, err)
				}
				if _, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID); err == nil {
					t.Fatal("resurrected marker adopted")
				}
			} else {
				if err == nil {
					t.Fatal("expected failure")
				}
				if mode == "prevalidation" && cleanupCalled {
					t.Fatal("cleanup before reservation validation")
				}
				if mode == "parentsync" && removed {
					t.Fatal("unlinked before artifact-parent sync")
				}
				if mode != "prevalidation" && !unlocked {
					t.Fatal("lock not released")
				}
				if _, found, err := readMaterialisation(o.Target, root); err != nil || !found {
					t.Fatalf("marker missing: %v %v", found, err)
				}
			}
		})
	}
}

func TestMaterialisationUnsafeAncestorAndReservationRefusal(t *testing.T) {
	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil { //nolint:gosec // intentionally unsafe parent
		t.Fatal(err)
	}
	if _, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID); err == nil {
		t.Fatal("accepted writable parent")
	}
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // restore private directory mode
		t.Fatal(err)
	}
	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	called := false
	err = a.retireCleaned(func() error {
		called = true
		return nil
	}, func(materialisationOwnership) (func(), error) { return nil, nil })
	if err == nil || called {
		t.Fatal("cleanup ran without a verified reservation")
	}
}

func TestMaterialisationDataAncestryServiceOwner(t *testing.T) {
	_, _ = materialisationFixture(t)
	base := materialisationAncestorBase
	mid := filepath.Join(base, "postgresql")
	leaf := filepath.Join(mid, "18")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	oldStat := materialisationAncestryStat
	t.Cleanup(func() { materialisationAncestryStat = oldStat })
	serviceUID := materialisationRecordUID + 1
	owners := map[string]uint32{mid: serviceUID, leaf: serviceUID}
	materialisationAncestryStat = func(path string) (os.FileInfo, materialisationFileID, error) {
		info, id, err := oldStat(path)
		if uid, ok := owners[path]; ok {
			id.UID = uid
		}
		return info, id, err
	}
	ancestors, err := materialisationAncestry(leaf, serviceUID, true)
	if err != nil {
		t.Fatalf("protected service data ancestry: %v", err)
	}
	if _, err := materialisationAncestry(leaf, serviceUID, false); err == nil {
		t.Fatal("service-owned registration/state ancestry accepted")
	}
	owners[mid] = serviceUID + 1
	if err := validateMaterialisationAncestry(ancestors); err == nil {
		t.Fatal("owner change accepted")
	}
	if _, err := materialisationAncestry(leaf, serviceUID, true); err == nil {
		t.Fatal("foreign owner accepted")
	}
	owners[mid] = serviceUID
	if err := os.Chmod(mid, 0o720); err != nil { //nolint:gosec // intentionally writable test directory
		t.Fatal(err)
	}
	if err := validateMaterialisationAncestry(ancestors); err == nil {
		t.Fatal("group-writable ancestor accepted")
	}
	if err := os.Chmod(mid, 0o700); err != nil { //nolint:gosec // private test directory
		t.Fatal(err)
	}
	if err := os.Rename(mid, mid+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mid+"-old", mid); err != nil {
		t.Fatal(err)
	}
	if _, err := materialisationAncestry(leaf, serviceUID, true); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if err := validateMaterialisationAncestry(ancestors); err == nil {
		t.Fatal("replaced ancestor accepted")
	}
	if err := os.Remove(mid); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateMaterialisationAncestry(ancestors); err == nil {
		t.Fatal("replacement directory accepted")
	}
}

func TestMaterialisationPersistImmutableIdentity(t *testing.T) {
	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	foreign := o
	foreign.Target = "other"
	foreign.DataPath = filepath.Join(root, foreign.Target)
	foreign.RegisteredPath = filepath.Join(materialisationConfigRoot, foreign.Target)
	if _, err := createMaterialisation(foreign, root); err != nil {
		t.Fatal(err)
	}
	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	reserveTestMaterialisation(t, a)
	if err := a.bindCreated(false); err != nil {
		t.Fatal(err)
	}
	if err := a.bindCreated(true); err != nil {
		t.Fatal(err)
	}
	foreignPath, _ := materialisationPath(foreign.Target)
	before, err := os.ReadFile(foreignPath) //nolint:gosec // test-owned record
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*materialisationOwnership){
		"operation":     func(n *materialisationOwnership) { n.Operation = "other" },
		"job":           func(n *materialisationOwnership) { n.Job = "other" },
		"instance":      func(n *materialisationOwnership) { n.Instance = "other" },
		"source":        func(n *materialisationOwnership) { n.Source = "other" },
		"stanza":        func(n *materialisationOwnership) { n.Stanza = "other-abcdef" },
		"intended port": func(n *materialisationOwnership) { n.IntendedPort++ },
		"reservation":   func(n *materialisationOwnership) { n.TemporaryPort++ },
		"data inode":    func(n *materialisationOwnership) { n.DataIdentity.Inode++ },
		"config inode":  func(n *materialisationOwnership) { n.ConfigIdentity.Inode++ },
		"foreign target": func(n *materialisationOwnership) {
			n.Target, n.DataPath, n.RegisteredPath = foreign.Target, foreign.DataPath, foreign.RegisteredPath
		},
	} {
		t.Run(name, func(t *testing.T) {
			next := a.owner
			mutate(&next)
			if err := a.persistMaterialisation(next); err == nil {
				t.Fatal("accepted identity change")
			}
			if err := a.validate(); err != nil {
				t.Fatalf("original changed: %v", err)
			}
		})
	}
	after, err := os.ReadFile(foreignPath) //nolint:gosec // test-owned record
	if err != nil || string(after) != string(before) {
		t.Fatal("foreign marker changed")
	}
}

func TestMaterialisationOwnershipRefusesUntrusted(t *testing.T) {
	o, root := materialisationFixture(t)
	for name, change := range map[string]func(*materialisationOwnership){
		"foreign install":        func(o *materialisationOwnership) { o.Instance = "other" },
		"arbitrary data":         func(o *materialisationOwnership) { o.DataPath = "/etc" },
		"arbitrary registration": func(o *materialisationOwnership) { o.RegisteredPath = "/etc" },
		"path traversal":         func(o *materialisationOwnership) { o.Target = "../target" },
		"wrong internal target":  func(o *materialisationOwnership) { o.Kind = materialisationRestore },
	} {
		t.Run(name, func(t *testing.T) {
			bad := o
			change(&bad)
			if _, err := createMaterialisation(bad, root); err == nil {
				t.Fatal("accepted invalid ownership")
			}
		})
	}
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	p, _ := materialisationPath(o.Target)
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readMaterialisation(o.Target, root); err == nil {
		t.Fatal("accepted corrupt record")
	}
}
