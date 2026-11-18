package node

import (
	"os"
	"path/filepath"
	"testing"
)

// configAncestryFixture models Debian's /etc/postgresql/18 tree. The real
// directories stay owned by the test process; the ancestry seam substitutes
// the distinct root and postgres service identities without chowning.
type configAncestryFixture struct {
	t                      *testing.T
	base, etc, configBase  string
	configRoot             string
	rootUID, postgresUID   uint32
	owners                 map[string]uint32
	oldBase, oldConfigRoot string
	oldConfigBase          string
	oldRecordUID           uint32
	oldPostgresUID         func() (uint32, error)
	oldStat                func(string) (os.FileInfo, materialisationFileID, error)
}

func newConfigAncestryFixture(t *testing.T) *configAncestryFixture {
	t.Helper()
	f := &configAncestryFixture{t: t, base: t.TempDir()}
	f.etc = filepath.Join(f.base, "etc")
	f.configBase = filepath.Join(f.etc, "postgresql")

	f.configRoot = filepath.Join(f.configBase, "18")
	for _, dir := range []string{f.etc, f.configBase, f.configRoot} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	f.oldBase, f.oldConfigRoot, f.oldConfigBase = materialisationAncestorBase, materialisationConfigRoot, materialisationConfigBase
	f.oldRecordUID, f.oldPostgresUID, f.oldStat = materialisationRecordUID, materialisationPostgresUID, materialisationAncestryStat
	f.rootUID = uint32(os.Geteuid())
	f.postgresUID = f.rootUID + 1000
	materialisationAncestorBase, materialisationConfigRoot, materialisationConfigBase = f.base, f.configRoot, f.configBase
	materialisationRecordUID = f.rootUID
	materialisationPostgresUID = func() (uint32, error) { return f.postgresUID, nil }
	f.owners = map[string]uint32{
		f.base: f.rootUID, f.etc: f.rootUID, f.configBase: f.postgresUID, f.configRoot: f.postgresUID,
	}
	materialisationAncestryStat = func(path string) (os.FileInfo, materialisationFileID, error) {
		info, id, err := f.oldStat(path)
		if uid, ok := f.owners[path]; ok {
			id.UID = uid
		}

		return info, id, err
	}

	t.Cleanup(func() {
		materialisationAncestorBase, materialisationConfigRoot, materialisationConfigBase = f.oldBase, f.oldConfigRoot, f.oldConfigBase
		materialisationRecordUID, materialisationPostgresUID, materialisationAncestryStat = f.oldRecordUID, f.oldPostgresUID, f.oldStat
	})

	return f
}

func (f *configAncestryFixture) requireConfigAncestry() []materialisationAncestor {
	f.t.Helper()

	ancestors, err := materialisationConfigAncestry(f.configRoot, f.postgresUID)
	if err != nil {
		f.t.Fatalf("config ancestry: %v", err)
	}

	return ancestors
}

func (f *configAncestryFixture) withOwner(path string, postgres bool) {
	if postgres {
		f.owners[path] = f.postgresUID
		return
	}

	f.owners[path] = f.rootUID
}

func TestMaterialisationConfigProductionBoundary(t *testing.T) {
	if materialisationConfigRoot != "/etc/postgresql/18" || materialisationConfigBase != "/etc/postgresql" {
		t.Fatalf("config boundary: root=%q base=%q", materialisationConfigRoot, materialisationConfigBase)
	}

	if filepath.Dir(materialisationConfigRoot) != materialisationConfigBase {
		t.Fatal("config root is not the version directory under /etc/postgresql")
	}
}

func TestMaterialisationConfigAncestryAdmitsDebianOwnership(t *testing.T) {
	f := newConfigAncestryFixture(t)
	ancestors := f.requireConfigAncestry()

	want := []struct {
		path string
		uid  uint32
	}{
		{f.base, f.rootUID},
		{f.etc, f.rootUID},
		{f.configBase, f.postgresUID},
		{f.configRoot, f.postgresUID},
	}
	if len(ancestors) != len(want) {
		t.Fatalf("ancestors: %+v", ancestors)
	}

	for i, expected := range want {
		if ancestors[i].path != expected.path || ancestors[i].id.UID != expected.uid {
			t.Fatalf("ancestor %d: %+v, want %s uid %d", i, ancestors[i], expected.path, expected.uid)
		}
	}

	root, info, pinned, err := openMaterialisationConfigParent(f.postgresUID)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	if err := validateMaterialisationAncestry(pinned); err != nil {
		t.Fatal(err)
	}

	if err := validateMaterialisationParent(f.configRoot, info); err != nil {
		t.Fatal(err)
	}
}

func TestMaterialisationConfigAncestryAdmitsRootOwnedParent(t *testing.T) {
	for _, test := range []struct {
		name         string
		basePostgres bool
		rootPostgres bool
	}{
		{"both root", false, false},
		{"postgresql root version postgres", false, true},
		{"postgresql postgres version root", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConfigAncestryFixture(t)
			f.withOwner(f.configBase, test.basePostgres)
			f.withOwner(f.configRoot, test.rootPostgres)
			f.requireConfigAncestry()

			root, _, pinned, err := openMaterialisationConfigParent(f.postgresUID)
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = root.Close() }()

			if err := validateMaterialisationAncestry(pinned); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMaterialisationConfigAncestryRejectsPostgresSystemOwner(t *testing.T) {
	f := newConfigAncestryFixture(t)
	f.withOwner(f.base, true)

	if _, err := materialisationConfigAncestry(f.configRoot, f.postgresUID); err == nil {
		t.Fatal("postgres-owned traversal root accepted")
	}

	if _, err := materialisationAncestry(f.configRoot, f.postgresUID, true); err == nil {
		t.Fatal("generic policy admitted the traversal root")
	}

	f = newConfigAncestryFixture(t)
	f.withOwner(f.etc, true)

	if _, err := materialisationConfigAncestry(f.configRoot, f.postgresUID); err == nil {
		t.Fatal("postgres-owned /etc accepted")
	}
	// The generic data policy would accept the same system ancestor above the
	// config boundary. That is exactly why config has its own policy.
	if _, err := materialisationAncestry(f.configRoot, f.postgresUID, true); err != nil {
		t.Fatalf("generic policy precondition: %v", err)
	}
}

func TestMaterialisationConfigAncestryRejectsForeignOwner(t *testing.T) {
	for name, path := range map[string]func(*configAncestryFixture) string{
		"config base": func(f *configAncestryFixture) string { return f.configBase },
		"version":     func(f *configAncestryFixture) string { return f.configRoot },
	} {
		t.Run(name, func(t *testing.T) {
			f := newConfigAncestryFixture(t)

			f.owners[path(f)] = f.postgresUID + 1
			if _, err := materialisationConfigAncestry(f.configRoot, f.postgresUID); err == nil {
				t.Fatal("foreign owner accepted")
			}
		})
	}
}

func TestMaterialisationConfigAncestryRejectsWritableAncestor(t *testing.T) {
	for name, path := range map[string]func(*configAncestryFixture) string{
		"config base": func(f *configAncestryFixture) string { return f.configBase },
		"version":     func(f *configAncestryFixture) string { return f.configRoot },
	} {
		for _, mode := range []os.FileMode{0o770, 0o702} {
			t.Run(name+"/"+mode.String(), func(t *testing.T) {
				f := newConfigAncestryFixture(t)
				if err := os.Chmod(path(f), mode); err != nil {
					t.Fatal(err)
				}

				if _, err := materialisationConfigAncestry(f.configRoot, f.postgresUID); err == nil {
					t.Fatal("writable ancestor accepted")
				}
			})
		}
	}
}

func TestMaterialisationConfigAncestryRejectsSymlink(t *testing.T) {
	for name, replace := range map[string]func(*configAncestryFixture) string{
		"config base": func(f *configAncestryFixture) string { return f.configBase },
		"version":     func(f *configAncestryFixture) string { return f.configRoot },
	} {
		t.Run(name, func(t *testing.T) {
			f := newConfigAncestryFixture(t)
			target := replace(f)

			moved := target + "-old"
			if err := os.Rename(target, moved); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink(moved, target); err != nil {
				t.Fatal(err)
			}

			if _, err := materialisationConfigAncestry(f.configRoot, f.postgresUID); err == nil {
				t.Fatal("symlink ancestor accepted")
			}
		})
	}
}

func TestMaterialisationConfigAncestryRejectsParentReplacement(t *testing.T) {
	f := newConfigAncestryFixture(t)

	root, _, pinned, err := openMaterialisationConfigParent(f.postgresUID)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	if err := os.Rename(f.configRoot, f.configRoot+"-old"); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(f.configRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := validateMaterialisationAncestry(pinned); err == nil {
		t.Fatal("replaced pinned parent accepted")
	}
}

func TestMaterialisationConfigAncestryRejectsUIDChange(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*configAncestryFixture)
		reopen bool
	}{
		"config base": {func(f *configAncestryFixture) { f.withOwner(f.configBase, false) }, false},
		"version":     {func(f *configAncestryFixture) { f.withOwner(f.configRoot, false) }, false},
		"system":      {func(f *configAncestryFixture) { f.withOwner(f.etc, true) }, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newConfigAncestryFixture(t)

			root, _, pinned, err := openMaterialisationConfigParent(f.postgresUID)
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = root.Close() }()

			test.change(f)

			if err := validateMaterialisationAncestry(pinned); err == nil {
				t.Fatal("ownership change accepted")
			}

			_, err = materialisationConfigAncestry(f.configRoot, f.postgresUID)
			if test.reopen && err == nil {
				t.Fatal("reopened changed ownership")
			}

			if !test.reopen && err != nil {
				t.Fatalf("root owner should remain admissible: %v", err)
			}
		})
	}
}

func TestMaterialisationStateAncestryRemainsRootOnly(t *testing.T) {
	f := newConfigAncestryFixture(t)
	traversal := filepath.Join(f.etc, "traversalroot")

	state := filepath.Join(traversal, "state")
	if err := os.Mkdir(traversal, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}

	f.withOwner(traversal, true)

	if _, err := materialisationAncestry(state, f.postgresUID, false); err == nil {
		t.Fatal("postgres-owned state ancestor accepted")
	}

	if _, _, _, err := openMaterialisationParent(state, f.postgresUID, false); err == nil {
		t.Fatal("opened postgres-owned state ancestry")
	}

	if _, err := materialisationConfigAncestry(state, f.postgresUID); err == nil {
		t.Fatal("state path accepted by config policy")
	}

	f.withOwner(traversal, false)

	if _, err := materialisationAncestry(state, f.postgresUID, false); err != nil {
		t.Fatalf("root-owned state ancestry: %v", err)
	}
}

// TestMaterialisationConfigOpenKeepsUnboundArtifactRefusal proves the config
// opener does not adopt an existing unbound target.
func TestMaterialisationConfigOpenKeepsUnboundArtifactRefusal(t *testing.T) {
	f := newConfigAncestryFixture(t)

	root, _, _, err := openMaterialisationConfigParent(f.postgresUID)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	if _, err := root.Lstat("target"); !os.IsNotExist(err) {
		t.Fatalf("unexpected target: %v", err)
	}

	if err := validateMaterialisationDir(root, "target", materialisationFileID{}, nil, 0o755); err != nil {
		t.Fatalf("absent target: %v", err)
	}

	if _, err := openMaterialisationChild(root, "target"); err == nil {
		t.Fatal("opened absent config target")
	}

	if err := validateMaterialisationDir(root, "target", materialisationFileID{Inode: 1, UID: f.postgresUID}, nil, 0o755); err == nil {
		t.Fatal("accepted unbound target identity")
	}
}
