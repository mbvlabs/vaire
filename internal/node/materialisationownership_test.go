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
	oldConfigRoot, oldConfigBase := materialisationConfigRoot, materialisationConfigBase
	oldBase := materialisationAncestorBase
	oldPostgresUID := materialisationPostgresUID
	base := t.TempDir()
	materialisationAncestorBase = base
	materialisationStateDir = filepath.Join(base, "state")
	materialisationConfigRoot = filepath.Join(base, "config")
	materialisationConfigBase = base

	if err := os.Mkdir(materialisationStateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(materialisationConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	materialisationRecordUID = uint32(os.Geteuid())
	materialisationPostgresUID = func() (uint32, error) { return materialisationRecordUID, nil }
	followHostInstance = func() (string, error) { return "install", nil }

	t.Cleanup(func() {
		materialisationStateDir, followHostInstance, materialisationRecordUID, materialisationConfigRoot = oldDir, oldInstance, oldUID, oldConfigRoot
		materialisationConfigBase = oldConfigBase
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
	reject(t, err != nil, err)
}

func TestMaterialisationOwnershipRecord(t *testing.T) {
	o, root := materialisationFixture(t)
	c, err := createMaterialisation(o, root)
	reject(t, err != nil, err)

	path, _ := materialisationPath(o.Target)
	info, err := os.Stat(path)
	rejectf(t, err != nil || info.Mode().Perm() != 0o600, "record mode: %v %v", info, err)

	if _, err := createMaterialisation(o, root); err == nil {
		t.Fatal("replaced existing record")
	}

	got, found, err := readMaterialisation(o.Target, root)
	rejectf(t, err != nil || !found || got != o, "read: %+v %v %v", got, found, err)
	list, err := listMaterialisations(root)
	rejectf(t, err != nil || len(list) != 1 || list[0] != o, "list: %+v %v", list, err)

	if c != nil || ownsMaterialisation(context.Background(), o) ||
		ownsMaterialisation(materialisationContext(context.Background(), &materialisationCapability{owner: o}), o) {
		t.Fatal("issued authority without a locked active job")
	}

	other := o
	other.Job = "another-job"
	reject(t, ownsMaterialisation(materialisationContext(context.Background(), c), other), "another job obtained authority")

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
	if err := os.Chmod(p, 0o644); err != nil {
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
	reject(t, err != nil, err)

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
	reject(t, err != nil, err)

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
	rejectf(t, err != nil || data.Mode().Perm() != 0o700, "data permissions: %v %v", data, err)
	conf, err := os.Stat(o.RegisteredPath)
	rejectf(t, err != nil || conf.Mode().Perm() != 0o755, "config permissions: %v %v", conf, err)

	if err := a.withOwnedArtifacts(func() error {
		// Registration leaves the root-created config directory owned by
		// postgres and world-readable, as the backup implementation requires.
		return os.Chmod(o.RegisteredPath, 0o755)
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
	reject(t, err != nil, err)

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
			runArtifactReplacementCase(t, replace)
		})
	}
}

func runArtifactReplacementCase(t *testing.T, replace string) {
	t.Helper()
	o, root := materialisationFixture(t)
	mustNoErr(t, os.Mkdir(root, 0o700))
	_, createErr := createMaterialisation(o, root)
	reject(t, createErr != nil, createErr)

	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	reject(t, err != nil, err)

	defer a.close()

	reserveTestMaterialisation(t, a)
	mustNoErr(t, a.bindCreated(false))
	mustNoErr(t, a.bindCreated(true))

	if replace == "config" {
		mustNoErr(t, os.Rename(o.RegisteredPath, o.RegisteredPath+"-old"))
		mustNoErr(t, os.Mkdir(o.RegisteredPath, 0o700))
	} else {
		p, _ := materialisationPath(o.Target)
		raw, err := os.ReadFile(p)
		reject(t, err != nil, err)
		mustNoErr(t, os.Rename(p, p+"-old"))
		mustNoErr(t, os.WriteFile(p, raw, 0o600))
	}

	rejectf(t, a.withOwnedArtifacts(func() error { return nil }) == nil, "replacement accepted")
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
	reject(t, err != nil, err)

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
	rejectf(t, err != nil || !found || got.ArtifactStage != materialisationDataCreated || got.DataIdentity.Inode == 0, "data binding lost: %+v %v %v", got, found, err)
}

func TestMaterialisationRetirement(t *testing.T) {
	for _, mode := range []string{"cleanup", "provisioned", "unverified-reservation", "cleanup-leaves-artifact"} {
		t.Run(mode, func(t *testing.T) {
			runRetirementCase(t, mode)
		})
	}
}

func runRetirementCase(t *testing.T, mode string) {
	t.Helper()
	o, root := materialisationFixture(t)
	mustNoErr(t, os.Mkdir(root, 0o700))
	_, createErr := createMaterialisation(o, root)
	reject(t, createErr != nil, createErr)

	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	reject(t, err != nil, err)

	defer a.close()

	reserveTestMaterialisation(t, a)
	mustNoErr(t, a.bindCreated(false))
	mustNoErr(t, a.bindCreated(true))

	guard := func(got materialisationOwnership) (func(), error) {
		reject(t, got != a.owner, "wrong reservation tuple")
		return func() {}, nil
	}

	switch mode {
	case "provisioned":
		mustNoErr(t, a.markMaterialisationProvisioned(func() error { return nil }))
		err = a.retireProvisioned(guard)
	case "unverified-reservation":
		mustNoErr(t, a.markMaterialisationProvisioned(func() error { return nil }))
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
	rejectf(t, (err != nil) != wantError, "retirement: %v", err)

	_, found, readErr := readMaterialisation(o.Target, root)
	rejectf(t, readErr != nil || found != wantError, "record found=%v err=%v", found, readErr)
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
	reject(t, err != nil, err)

	defer a.close()

	if err := a.bindCreated(false); err == nil {
		t.Fatal("created without reservation")
	}

	if err := a.bindMaterialisationReservation(config.TemporaryPortMin, func(materialisationOwnership, int, func() error) error { return nil }); err == nil {
		t.Fatal("accepted unverified reservation")
	}

	err = a.bindMaterialisationReservation(config.TemporaryPortMin, func(got materialisationOwnership, port int, finish func() error) error {
		reject(t, got != o || port != config.TemporaryPortMin, "wrong reservation tuple")
		return finish()
	})
	reject(t, err != nil, err)

	if err := a.bindCreated(false); err != nil {
		t.Fatal(err)
	}
}

func TestMaterialisationRetirementCommitPoint(t *testing.T) {
	for _, mode := range []string{"prevalidation", "parentsync", "unlink", "success"} {
		t.Run(mode, func(t *testing.T) {
			runRetirementCommitCase(t, mode)
		})
	}
}

func runRetirementCommitCase(t *testing.T, mode string) {
	t.Helper()
	o, root := materialisationFixture(t)
	mustNoErr(t, os.Mkdir(root, 0o700))
	_, createErr := createMaterialisation(o, root)
	reject(t, createErr != nil, createErr)

	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	reject(t, err != nil, err)

	defer a.close()

	reserveTestMaterialisation(t, a)
	mustNoErr(t, a.bindCreated(false))
	mustNoErr(t, a.bindCreated(true))

	p, _ := materialisationPath(o.Target)
	raw, err := os.ReadFile(p)
	reject(t, err != nil, err)

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
		reject(t, removed, "fallible parent sync after commit")

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
		assertRetirementCommitSuccess(t, o, root, p, raw, err, removed, unlocked, synced)
		return
	}

	assertRetirementCommitFailure(t, o, root, mode, err, removed, unlocked, cleanupCalled)
}

func assertRetirementCommitSuccess(t *testing.T, o materialisationOwnership, root, p string, raw []byte, err error, removed, unlocked bool, synced int) {
	t.Helper()
	rejectf(t, err != nil || !removed || !unlocked || synced != 2, "success: %v removed=%v unlock=%v sync=%d", err, removed, unlocked, synced)
	// A crash can restore the un-fsynced marker. Its original artifacts are
	// gone, so it remains visible but unusable.
	mustNoErr(t, os.WriteFile(p, raw, 0o600))

	_, found, readErr := readMaterialisation(o.Target, root)
	rejectf(t, readErr != nil || !found, "resurrected marker: %v %v", found, readErr)

	_, openErr := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	reject(t, openErr == nil, "resurrected marker adopted")
}

func assertRetirementCommitFailure(t *testing.T, o materialisationOwnership, root, mode string, err error, removed, unlocked, cleanupCalled bool) {
	t.Helper()
	reject(t, err == nil, "expected failure")
	reject(t, mode == "prevalidation" && cleanupCalled, "cleanup before reservation validation")
	reject(t, mode == "parentsync" && removed, "unlinked before artifact-parent sync")
	reject(t, mode != "prevalidation" && !unlocked, "lock not released")

	_, found, readErr := readMaterialisation(o.Target, root)
	rejectf(t, readErr != nil || !found, "marker missing: %v %v", found, readErr)
}

func TestMaterialisationUnsafeAncestorAndReservationRefusal(t *testing.T) {
	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID); err == nil {
		t.Fatal("accepted writable parent")
	}

	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}

	a, err := openMaterialisationArtifacts(o.Target, root, materialisationRecordUID)
	reject(t, err != nil, err)

	defer a.close()

	called := false
	err = a.retireCleaned(func() error {
		called = true
		return nil
	}, func(materialisationOwnership) (func(), error) { return nil, nil })
	reject(t, err == nil || called, "cleanup ran without a verified reservation")
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
	rejectf(t, err != nil, "protected service data ancestry: %v", err)

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
	if err := os.Chmod(mid, 0o720); err != nil {
		t.Fatal(err)
	}

	if err := validateMaterialisationAncestry(ancestors); err == nil {
		t.Fatal("group-writable ancestor accepted")
	}

	if err := os.Chmod(mid, 0o700); err != nil {
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
	reject(t, err != nil, err)

	defer a.close()

	reserveTestMaterialisation(t, a)

	if err := a.bindCreated(false); err != nil {
		t.Fatal(err)
	}

	if err := a.bindCreated(true); err != nil {
		t.Fatal(err)
	}

	foreignPath, _ := materialisationPath(foreign.Target)
	before, err := os.ReadFile(foreignPath)
	reject(t, err != nil, err)

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

	after, err := os.ReadFile(foreignPath)
	reject(t, err != nil || string(after) != string(before), "foreign marker changed")
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
