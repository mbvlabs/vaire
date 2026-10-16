package masterbackup

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func sampleTree(t *testing.T) string {
	return writeTree(t, map[string]string{
		ClusterPath:                         "name = \"prod\"\n",
		NodePath:                            "server = \"prod\"\nnode_name = \"db1\"\n",
		OperatorsPath:                       "[[operators]]\n",
		WGKeyPath:                           "wgkey\n",
		"/etc/ssh/ssh_host_ed25519_key":     "private\n",
		"/etc/ssh/ssh_host_ed25519_key.pub": "ssh-ed25519 AAAA\n",
		"/etc/ssh/ssh_config":               "not bundled\n",
		"/etc/eru/acme/account.key":         "acme\n",
		"/etc/eru/runtime/backup.env":       "never bundled\n",
		TLSCertPath:                         "cert\n",
		TLSKeyPath:                          "key\n",
	})
}

func meta() Meta {
	return Meta{Server: "prod", Node: "db1", Epoch: 2, CreatedAt: time.Date(2026, 10, 15, 10, 15, 2, 0, time.UTC), EruVersion: "dev"}
}

func TestSnapshotPicksTheBundleSetOnly(t *testing.T) {
	files, err := Snapshot(sampleTree(t))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{ClusterPath, NodePath, OperatorsPath, WGKeyPath, "/etc/ssh/ssh_host_ed25519_key", "/etc/ssh/ssh_host_ed25519_key.pub", TLSCertPath, TLSKeyPath, "/etc/eru/acme/account.key"}
	if len(paths) != len(want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths %v, want %v", paths, want)
		}
	}
}

func TestSnapshotNeedsClusterToml(t *testing.T) {
	if _, err := Snapshot(writeTree(t, map[string]string{NodePath: "x"})); err == nil {
		t.Fatal("a snapshot without cluster.toml succeeded")
	}
}

func TestBuildIsDeterministicAndRoundTrips(t *testing.T) {
	files, err := Snapshot(sampleTree(t))
	if err != nil {
		t.Fatal(err)
	}
	a, m, err := Build(meta(), files)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Build(meta(), files)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two builds of the same snapshot differ")
	}
	if m.Revision != Revision(files) || m.Revision == "" {
		t.Fatalf("revision %q", m.Revision)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(a, id.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(sealed, id.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Files) != len(files) || opened.Manifest.Server != "prod" || opened.Manifest.Epoch != 2 {
		t.Fatalf("opened %+v", opened.Manifest)
	}
	f, ok := opened.File(WGKeyPath)
	if !ok || string(f.Data) != "wgkey\n" {
		t.Fatalf("wg key %q", f.Data)
	}

	sealed[len(sealed)-5] ^= 0xff
	if _, err := Open(sealed, id.String()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a flipped byte opened: %v", err)
	}
}

func tarOf(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e[0], Mode: 0o600, Size: int64(len(e[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadTarRefusesStrangeContent(t *testing.T) {
	files, _ := Snapshot(sampleTree(t))
	good, _, err := Build(meta(), files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTar(good); err != nil {
		t.Fatalf("good tar: %v", err)
	}
	manifest := `{"schema":"eru-master-bundle/v1","files":[]}`
	cases := map[string][]byte{
		"no manifest first": tarOf(t, [2]string{"etc/eru/cluster.toml", "x"}),
		"escaping path":     tarOf(t, [2]string{ManifestName, manifest}, [2]string{"../etc/passwd", "x"}),
		"extra file":        tarOf(t, [2]string{ManifestName, manifest}, [2]string{"etc/eru/cluster.toml", "x"}),
		"wrong schema":      tarOf(t, [2]string{ManifestName, `{"schema":"other"}`}),
		"empty":             tarOf(t),
	}
	for name, raw := range cases {
		if _, err := ReadTar(raw); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrCorrupt", name, err)
		}
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ManifestName, Mode: 0o600, Size: int64(len(manifest))})
	_, _ = tw.Write([]byte(manifest))
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "etc/eru/cluster.toml", Linkname: "/etc/shadow"})
	_ = tw.Close()
	if _, err := ReadTar(buf.Bytes()); !errors.Is(err, ErrCorrupt) {
		t.Errorf("symlink: %v, want ErrCorrupt", err)
	}
}

func TestAllowed(t *testing.T) {
	for p, want := range map[string]bool{
		ClusterPath:                        true,
		"/etc/ssh/ssh_host_rsa_key":        true,
		"/etc/ssh/ssh_host_rsa_key.pub":    true,
		"/etc/eru/acme/account.key":        true,
		"/etc/eru/runtime/backup.env":      false,
		"/etc/eru/../shadow":               false,
		"etc/eru/cluster.toml":             false,
		"/etc/ssh/sshd_config":             false,
		"/var/lib/eru/master-backup/dirty": false,
		"/etc/eru/acme/sub/dir":            false,
	} {
		if got := Allowed(p); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestNames(t *testing.T) {
	at := time.Date(2026, 10, 15, 10, 15, 2, 0, time.UTC)
	n := Name(at, 2, "3f9a1c0b77d2aaaaaaaa")
	if n != "20261015T101502Z-e2-r3f9a1c0b77d2" {
		t.Fatalf("name %s", n)
	}
	p, ok := ParseName(n)
	if !ok || !p.CreatedAt.Equal(at) || p.Epoch != 2 || p.Revision != "3f9a1c0b77d2" {
		t.Fatalf("parsed %+v %v", p, ok)
	}
	if name, isBundle, ok := SplitKey("prod/master/" + n + BundleSuffix); !ok || !isBundle || name != n {
		t.Fatalf("split bundle %s %v %v", name, isBundle, ok)
	}
	if name, isBundle, ok := SplitKey("prod/master/" + n + SidecarSuffix); !ok || isBundle || name != n {
		t.Fatalf("split sidecar %s %v %v", name, isBundle, ok)
	}
	if _, _, ok := SplitKey("prod/master/epoch.json"); ok {
		t.Fatal("epoch.json parsed as a bundle")
	}
}

// A bundle read from a file named otherwise is named by its manifest, the
// same name the node gave it when it built it.
func TestManifestNamesTheBundle(t *testing.T) {
	files, err := Snapshot(sampleTree(t))
	if err != nil {
		t.Fatal(err)
	}
	m := meta()
	built := m.CreatedAt.Add(400 * time.Millisecond)
	m.CreatedAt = built
	_, manifest, err := Build(m, files)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := manifest.Name(), Name(built, m.Epoch, manifest.Revision); got != want {
		t.Fatalf("manifest names the bundle %s, the node named it %s", got, want)
	}
}

func TestKeep(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	name := func(daysAgo int, hour int) string {
		return Name(now.Add(-time.Duration(daysAgo)*24*time.Hour).Add(time.Duration(hour)*time.Hour), 1, "abc")
	}
	var names []string
	for d := 0; d < 400; d++ {
		names = append(names, name(d, 0), name(d, -1))
	}
	names = append(names, "not-a-bundle")
	keep := Keep(names, now, 30, 52, 3)
	if !keep["not-a-bundle"] {
		t.Fatal("an unparseable name was pruned")
	}
	for d := 0; d < 30; d++ {
		if !keep[name(d, 0)] || !keep[name(d, -1)] {
			t.Fatalf("day %d pruned inside the 30 days", d)
		}
	}
	weekly := 0
	for d := 30; d < 400; d++ {
		for _, h := range []int{0, -1} {
			if keep[name(d, h)] {
				if d > 52*7 {
					t.Fatalf("day %d kept past 52 weeks", d)
				}
				weekly++
			}
		}
	}
	if weekly < 45 || weekly > 50 {
		t.Fatalf("%d weekly bundles kept", weekly)
	}

	old := []string{Name(now.Add(-400*24*time.Hour), 1, "abc")}
	if !Keep(old, now, 30, 52, 3)[old[0]] {
		t.Fatal("the only bundle, a year old, was pruned")
	}
}

func TestPick(t *testing.T) {
	at := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	e1new := Name(at.Add(2*time.Hour), 1, "aaa") // an old master that kept writing
	e2old := Name(at, 2, "bbb")
	e2new := Name(at.Add(time.Hour), 2, "ccc")
	incomplete := Name(at.Add(3*time.Hour), 2, "ddd")
	complete := map[string]bool{e1new: true, e2old: true, e2new: true, incomplete: false}
	if p, ok := Pick(complete, "", time.Time{}); !ok || p.Name != e2new {
		t.Fatalf("picked %s", p.Name)
	}
	if p, ok := Pick(complete, "", at.Add(30*time.Minute)); !ok || p.Name != e2old {
		t.Fatalf("picked %s before a time", p.Name)
	}
	if _, ok := Pick(complete, incomplete, time.Time{}); ok {
		t.Fatal("picked an incomplete bundle by name")
	}
	if p, ok := Pick(complete, e1new, time.Time{}); !ok || p.Name != e1new {
		t.Fatal("named pick failed")
	}
}

func TestDecide(t *testing.T) {
	ep := func(server string, n int64, id string) *Epoch {
		return &Epoch{Schema: EpochSchema, Server: server, Epoch: n, InstanceID: id}
	}
	me := Local{Server: "prod", Instance: "aaa", Epoch: 2}
	for name, c := range map[string]struct {
		bucket *Epoch
		err    error
		local  Local
		want   FenceState
	}{
		"ours":                      {ep("prod", 2, "aaa"), nil, me, FenceOK},
		"newer epoch, other":        {ep("prod", 3, "bbb"), nil, me, FenceFenced},
		"no local epoch yet, other": {ep("prod", 1, "bbb"), nil, Local{Server: "prod", Instance: "aaa"}, FenceFenced},
		"same epoch, other":         {ep("prod", 2, "bbb"), nil, me, FenceUnknown},
		"older epoch, other":        {ep("prod", 1, "bbb"), nil, me, FenceUnknown},
		"another server's epoch":    {ep("staging", 9, "bbb"), nil, me, FenceUnknown},
		"no schema":                 {&Epoch{Server: "prod", Epoch: 3, InstanceID: "bbb"}, nil, me, FenceUnknown},
		"no instance in the object": {ep("prod", 3, ""), nil, me, FenceUnknown},
		"zero epoch in the object":  {ep("prod", 0, "bbb"), nil, Local{Server: "prod", Instance: "aaa"}, FenceUnknown},
		"absent":                    {nil, nil, me, FenceAbsent},
		"read error":                {nil, errors.New("timeout"), me, FenceUnknown},
		"no local instance":         {ep("prod", 3, "bbb"), nil, Local{Server: "prod"}, FenceUnknown},
	} {
		if got := Decide(c.bucket, c.err, c.local); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

func TestDirtyFlag(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testRoot = root
	t.Cleanup(func() { testRoot = "" })
	if d, _ := Dirty(root); d {
		t.Fatal("dirty before any change")
	}
	MarkDirty("config.set")
	d, at := Dirty(root)
	if !d {
		t.Fatal("MarkDirty didn't set the flag")
	}
	if Settled(at, at.Add(time.Second), 5*time.Second) || !Settled(at, at.Add(5*time.Second), 5*time.Second) {
		t.Fatal("debounce decision wrong")
	}
	if err := ClearDirty(root); err != nil {
		t.Fatal(err)
	}
	if d, _ := Dirty(root); d {
		t.Fatal("flag still set after ClearDirty")
	}
	testRoot = root + "/missing"
	MarkDirty("ignored") // no directory: no error, no file
}
