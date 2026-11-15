package node

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// dropClusterConfigTest adds leaf ownership to the config ancestry fixture.
// The real files stay owned by the test process, so the artifact identity seam
// models the postgres-owned config directory without chowning.
type dropClusterConfigTest struct {
	t          *testing.T
	f          *configAncestryFixture
	leafOwners map[uint64]uint32
}

func newDropClusterConfigTest(t *testing.T) *dropClusterConfigTest {
	t.Helper()
	f := newConfigAncestryFixture(t)
	d := &dropClusterConfigTest{t: t, f: f, leafOwners: map[uint64]uint32{}}
	oldID := materialisationArtifactID
	materialisationArtifactID = func(info os.FileInfo) (materialisationFileID, error) {
		id, err := oldID(info)
		if uid, ok := d.leafOwners[id.Inode]; ok {
			id.UID = uid
		}
		return id, err
	}
	t.Cleanup(func() { materialisationArtifactID = oldID })
	return d
}

func (d *dropClusterConfigTest) path(name string) string {
	return filepath.Join(d.f.configRoot, name)
}

// register creates what Debian considers a registered cluster: postgresql.conf
// plus the vaire drop-ins under conf.d.
func (d *dropClusterConfigTest) register(name string) string {
	d.t.Helper()
	dir := d.path(name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte("# registered\n"), 0o644); err != nil {
		d.t.Fatal(err)
	}
	confd := filepath.Join(dir, "conf.d")
	if err := os.Mkdir(confd, 0o755); err != nil {
		d.t.Fatal(err)
	}
	for _, file := range []string{"10-vaire.conf", "20-vaire.conf", "30-vaire.conf", "40-vaire.conf", "50-vaire.conf", "90-vaire.conf"} {
		if err := os.WriteFile(filepath.Join(confd, file), []byte("managed\n"), 0o644); err != nil {
			d.t.Fatal(err)
		}
	}
	d.own(dir, d.f.postgresUID)
	return dir
}

func (d *dropClusterConfigTest) own(path string, uid uint32) {
	d.t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		d.t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		d.t.Fatalf("cannot identify %s", path)
	}
	d.leafOwners[st.Ino] = uid
}

func (d *dropClusterConfigTest) inode(path string) uint64 {
	d.t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		d.t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		d.t.Fatalf("cannot identify %s", path)
	}
	return st.Ino
}

// deregister models the part of pg_dropcluster that removes the standard
// registration while leaving conf.d behind.
func deregister(dir string) error {
	return os.Remove(filepath.Join(dir, "postgresql.conf"))
}

func (d *dropClusterConfigTest) requireRemains(dir string) {
	d.t.Helper()
	for _, path := range []string{dir, filepath.Join(dir, "conf.d", "10-vaire.conf")} {
		if _, err := os.Lstat(path); err != nil {
			d.t.Fatalf("%s was removed: %v", path, err)
		}
	}
}

// interleave installs deterministic hooks around the atomic quarantine move.
func (d *dropClusterConfigTest) interleave(before, after func()) {
	d.t.Helper()
	oldBefore, oldAfter := dropConfigBeforeQuarantine, dropConfigAfterQuarantine
	if before == nil {
		before = func() {}
	}
	if after == nil {
		after = func() {}
	}
	dropConfigBeforeQuarantine, dropConfigAfterQuarantine = before, after
	d.t.Cleanup(func() {
		dropConfigBeforeQuarantine, dropConfigAfterQuarantine = oldBefore, oldAfter
	})
}

func (d *dropClusterConfigTest) quarantines() []string {
	d.t.Helper()
	return d.quarantinesAt(d.f.configRoot)
}

func (d *dropClusterConfigTest) quarantinesAt(root string) []string {
	d.t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		d.t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), dropConfigQuarantinePrefix) {
			paths = append(paths, filepath.Join(root, entry.Name()))
		}
	}
	return paths
}

func (d *dropClusterConfigTest) requireNoQuarantine() {
	d.t.Helper()
	if paths := d.quarantines(); len(paths) != 0 {
		d.t.Fatalf("quarantine directories remain: %v", paths)
	}
}

func TestDropClusterConfigRemovesDeregisteredRemnant(t *testing.T) {
	for _, test := range []struct {
		name   string
		effect func(string) error
	}{
		{"leaves conf.d", deregister},
		{"removes directory", func(dir string) error { return os.RemoveAll(dir) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := newDropClusterConfigTest(t)
			name := "app_read"
			dir := d.register(name)
			called := false
			err := dropRegisteredCluster(name, func() error {
				called = true
				return test.effect(dir)
			})
			if err != nil {
				t.Fatalf("drop: %v", err)
			}
			if !called {
				t.Fatal("drop effect was not run")
			}
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("configuration directory remains: %v", err)
			}
			d.requireNoQuarantine()
			// The live failure blocked this: follow refused a same-name cluster
			// because the old configuration directory still existed.
			reused := d.register(name)
			if _, err := os.Stat(filepath.Join(reused, "postgresql.conf")); err != nil {
				t.Fatalf("same-name reuse: %v", err)
			}
		})
	}
}

func TestDropClusterConfigEffectFailurePreservesRemnant(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_failed"
	dir := d.register(name)
	dropErr := errors.New("pg_dropcluster failed")
	err := dropRegisteredCluster(name, func() error {
		if err := deregister(dir); err != nil {
			return err
		}
		return dropErr
	})
	if !errors.Is(err, dropErr) {
		t.Fatalf("drop error: %v", err)
	}
	d.requireRemains(dir)
}

func TestDropClusterConfigWithoutDirectoryRunsEffect(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_absent"
	called := false
	if err := dropRegisteredCluster(name, func() error { called = true; return nil }); err != nil {
		t.Fatalf("drop without a config directory: %v", err)
	}
	if !called {
		t.Fatal("drop effect was not run")
	}
	if _, err := os.Lstat(d.path(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected configuration directory: %v", err)
	}
}

func TestDropClusterConfigRefusesUnregisteredRemnant(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_legacy"
	dir := d.path(name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf.d", "10-vaire.conf"), []byte("managed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := dropRegisteredCluster(name, func() error { called = true; return nil }); err == nil {
		t.Fatal("unregistered remnant accepted")
	}
	if called {
		t.Fatal("drop effect ran for an unregistered remnant")
	}
	d.requireRemains(dir)
}

func TestDropClusterConfigRefusesRegistrationLeftAfterDrop(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_registered"
	dir := d.register(name)
	if err := dropRegisteredCluster(name, func() error { return nil }); err == nil {
		t.Fatal("drop left a registration and cleanup succeeded")
	}
	d.requireRemains(dir)
	d.requireNoQuarantine()
}

func TestDropClusterConfigRefusesReplacedRemnant(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_replaced"
	dir := d.register(name)
	moved := dir + "-old"
	err := dropRegisteredCluster(name, func() error {
		if err := deregister(dir); err != nil {
			return err
		}
		if err := os.Rename(dir, moved); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte("# new\n"), 0o644); err != nil {
			return err
		}
		d.own(dir, d.f.postgresUID)
		return nil
	})
	if err == nil {
		t.Fatal("replacement was removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "postgresql.conf")); err != nil {
		t.Fatalf("replacement was touched: %v", err)
	}
	d.requireRemains(moved)
	d.requireNoQuarantine()
}

func TestDropClusterConfigRefusesLeafOwnerChange(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_owned"
	dir := d.register(name)
	inode := d.inode(dir)
	err := dropRegisteredCluster(name, func() error {
		if err := deregister(dir); err != nil {
			return err
		}
		d.leafOwners[inode] = d.f.postgresUID + 1
		return nil
	})
	if err == nil {
		t.Fatal("owner change was removed")
	}
	d.requireRemains(dir)
	d.requireNoQuarantine()
}

func TestDropClusterConfigRefusesSymlinkReplacement(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_linked"
	dir := d.register(name)
	moved := dir + "-old"
	err := dropRegisteredCluster(name, func() error {
		if err := deregister(dir); err != nil {
			return err
		}
		if err := os.Rename(dir, moved); err != nil {
			return err
		}
		return os.Symlink(moved, dir)
	})
	if err == nil {
		t.Fatal("symlink replacement was accepted")
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replacement changed: %v %v", info, err)
	}
	d.requireRemains(moved)
	d.requireNoQuarantine()
}

func TestDropClusterConfigRefusesSymlinkRemnant(t *testing.T) {
	d := newDropClusterConfigTest(t)
	outside := filepath.Join(d.f.base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, d.path("app_linkleaf")); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := dropRegisteredCluster("app_linkleaf", func() error { called = true; return nil }); err == nil {
		t.Fatal("symlink remnant accepted")
	}
	if called {
		t.Fatal("drop effect ran for a symlink remnant")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("symlink target was touched: %v", err)
	}
}

func TestDropClusterConfigDoesNotFollowRemnantSymlinks(t *testing.T) {
	d := newDropClusterConfigTest(t)
	dir := d.register("app_innerlink")
	outside := filepath.Join(d.f.base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "conf.d", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := dropRegisteredCluster("app_innerlink", func() error { return deregister(dir) }); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cleanup followed a symlink outside the config directory: %v", err)
	}
}

func TestDropClusterConfigKeepsPinnedLeafOpen(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_pinned"
	dir := d.register(name)
	c, err := openRegisteredDropConfig(name)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	err = c.drop(func() error {
		if _, err := unix.FcntlInt(c.dir.Fd(), unix.F_GETFD, 0); err != nil {
			return errors.New("pinned configuration directory closed before the drop effect: " + err.Error())
		}
		return deregister(dir)
	})
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration directory remains: %v", err)
	}
}

func requireFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s: %q %v", path, data, err)
	}
}

func replaceWithUnknown(d *dropClusterConfigTest, dir, original, unknown string, hookErr *error) func() {
	return func() {
		if *hookErr = os.Rename(dir, original); *hookErr != nil {
			return
		}
		if *hookErr = os.Mkdir(dir, 0o755); *hookErr != nil {
			return
		}
		if *hookErr = os.WriteFile(filepath.Join(dir, unknown), []byte("unknown\n"), 0o644); *hookErr == nil {
			d.own(dir, d.f.postgresUID)
		}
	}
}

func occupyPublicName(d *dropClusterConfigTest, dir, file string, hookErr *error) func() {
	return func() {
		if *hookErr != nil {
			return
		}
		if *hookErr = os.Mkdir(dir, 0o755); *hookErr != nil {
			return
		}
		if *hookErr = os.WriteFile(file, []byte("occupant\n"), 0o644); *hookErr == nil {
			d.own(dir, d.f.postgresUID)
		}
	}
}

func TestDropClusterConfigReplacementBeforeQuarantineIsPreserved(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_interleave"
	dir := d.register(name)
	original := dir + "-original"
	var hookErr error
	d.interleave(replaceWithUnknown(d, dir, original, "unknown.conf", &hookErr), nil)
	err := dropRegisteredCluster(name, func() error { return deregister(dir) })
	if hookErr != nil {
		t.Fatalf("interleave: %v", hookErr)
	}
	if err == nil {
		t.Fatal("replacement before quarantine was removed")
	}
	requireFile(t, filepath.Join(dir, "unknown.conf"), "unknown\n")
	d.requireRemains(original)
	d.requireNoQuarantine()
}

func TestDropClusterConfigRegistrationRecreatedBeforeQuarantineIsPreserved(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_reregistered"
	dir := d.register(name)
	var hookErr error
	d.interleave(func() {
		hookErr = os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte("# recreated\n"), 0o644)
	}, nil)
	err := dropRegisteredCluster(name, func() error { return deregister(dir) })
	if hookErr != nil {
		t.Fatalf("interleave: %v", hookErr)
	}
	if err == nil {
		t.Fatal("recreated registration was removed")
	}
	d.requireRemains(dir)
	requireFile(t, filepath.Join(dir, "postgresql.conf"), "# recreated\n")
	d.requireNoQuarantine()
}

func TestDropClusterConfigRenamedWithoutReplacementIsPreserved(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_renamed"
	dir := d.register(name)
	unknown := filepath.Join(dir, "unknown.conf")
	if err := os.WriteFile(unknown, []byte("unknown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	renamed := dir + "-renamed"
	err := dropRegisteredCluster(name, func() error { return os.Rename(dir, renamed) })
	if err == nil {
		t.Fatal("renamed original was accepted as removed")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected configuration directory at the public name: %v", err)
	}
	d.requireRemains(renamed)
	requireFile(t, filepath.Join(renamed, "unknown.conf"), "unknown\n")
	d.requireNoQuarantine()
}

func TestDropClusterConfigUnknownCandidateIsRetainedNotDeleted(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_retained"
	dir := d.register(name)
	original := dir + "-original"
	occupant := filepath.Join(dir, "occupant.conf")
	var hookErr error
	d.interleave(
		replaceWithUnknown(d, dir, original, "unknown.conf", &hookErr),
		occupyPublicName(d, dir, occupant, &hookErr),
	)
	err := dropRegisteredCluster(name, func() error { return deregister(dir) })
	if hookErr != nil {
		t.Fatalf("interleave: %v", hookErr)
	}
	if err == nil {
		t.Fatal("unknown replacement was deleted")
	}
	requireFile(t, occupant, "occupant\n")
	d.requireRemains(original)
	paths := d.quarantines()
	if len(paths) != 1 {
		t.Fatalf("quarantine directories: %v", paths)
	}
	candidate := filepath.Join(paths[0], dropConfigQuarantineEntry)
	requireFile(t, filepath.Join(candidate, "unknown.conf"), "unknown\n")
	if !strings.Contains(err.Error(), candidate) {
		t.Fatalf("error does not report the retained candidate %s: %v", candidate, err)
	}
	// A later drop must not adopt either the occupant or the retained jail.
	called := false
	if err := dropRegisteredCluster(name, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("retained state was adopted by a later drop")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("retained candidate was removed: %v", err)
	}
}

func TestDropClusterConfigPublicReplacementAfterQuarantineRefusesSuccess(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_postjail"
	dir := d.register(name)
	replacement := filepath.Join(dir, "replacement.conf")
	var hookErr error
	d.interleave(nil, occupyPublicName(d, dir, replacement, &hookErr))
	err := dropRegisteredCluster(name, func() error { return deregister(dir) })
	if hookErr != nil {
		t.Fatalf("interleave: %v", hookErr)
	}
	if err == nil {
		t.Fatal("public replacement after quarantine was accepted")
	}
	if !strings.Contains(err.Error(), "recreated") {
		t.Fatalf("unexpected error: %v", err)
	}
	requireFile(t, replacement, "occupant\n")
	if _, err := os.Lstat(filepath.Join(dir, "conf.d")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old quarantined candidate was not removed: %v", err)
	}
	d.requireNoQuarantine()
}

func TestDropClusterConfigAbsentNameChecksAncestry(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_absent_ancestry"
	dir := d.register(name)
	err := dropRegisteredCluster(name, func() error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		d.f.withOwner(d.f.configRoot, false)
		return nil
	})
	if err == nil {
		t.Fatal("absent name skipped ancestry validation")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration directory remains: %v", err)
	}
	d.requireNoQuarantine()
}

func TestDropClusterConfigAncestorChangeDuringQuarantinePreserves(t *testing.T) {
	d := newDropClusterConfigTest(t)
	name := "app_midancestor"
	dir := d.register(name)
	oldRoot := d.f.configRoot + "-old"
	var hookErr error
	d.interleave(func() {
		if hookErr = os.Rename(d.f.configRoot, oldRoot); hookErr != nil {
			return
		}
		hookErr = os.Mkdir(d.f.configRoot, 0o755)
	}, nil)
	err := dropRegisteredCluster(name, func() error { return deregister(dir) })
	if hookErr != nil {
		t.Fatalf("interleave: %v", hookErr)
	}
	if err == nil {
		t.Fatal("ancestor change during quarantine was ignored")
	}
	d.requireRemains(filepath.Join(oldRoot, name))
	if paths := d.quarantinesAt(oldRoot); len(paths) != 0 {
		t.Fatalf("quarantine directories remain: %v", paths)
	}
}

func TestDropClusterConfigRefusesAncestorChange(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*dropClusterConfigTest) error
	}{
		{"replaced", func(d *dropClusterConfigTest) error {
			return os.Rename(d.f.configRoot, d.f.configRoot+"-old")
		}},
		{"symlinked", func(d *dropClusterConfigTest) error {
			if err := os.Rename(d.f.configRoot, d.f.configRoot+"-old"); err != nil {
				return err
			}
			return os.Symlink(d.f.configRoot+"-old", d.f.configRoot)
		}},
		{"owner changed", func(d *dropClusterConfigTest) error {
			d.f.withOwner(d.f.configRoot, false)
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := newDropClusterConfigTest(t)
			name := "app_ancestor"
			dir := d.register(name)
			moved := d.f.configRoot + "-old"
			err := dropRegisteredCluster(name, func() error {
				if err := deregister(dir); err != nil {
					return err
				}
				return test.change(d)
			})
			if err == nil {
				t.Fatal("ancestor change was removed")
			}
			if test.name == "owner changed" {
				d.requireRemains(dir)
				return
			}
			d.requireRemains(filepath.Join(moved, name))
		})
	}
}
