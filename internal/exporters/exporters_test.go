package exporters

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/versions"
)

func TestPinsComplete(t *testing.T) {
	if err := versions.CheckExporterPins(); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactLayout(t *testing.T) {
	a, err := ArtifactFor(versions.NodeExporter, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if a.Tarball != "node_exporter-1.12.1.linux-amd64.tar.gz" {
		t.Errorf("tarball %s", a.Tarball)
	}
	if a.Member != "node_exporter-1.12.1.linux-amd64/node_exporter" {
		t.Errorf("member %s", a.Member)
	}
	if a.URL != "https://github.com/prometheus/node_exporter/releases/download/v1.12.1/node_exporter-1.12.1.linux-amd64.tar.gz" {
		t.Errorf("url %s", a.URL)
	}
	if a.BinaryPath() != "/usr/local/lib/eru/exporters/node_exporter-1.12.1/node_exporter" {
		t.Errorf("path %s", a.BinaryPath())
	}
	p, err := ArtifactFor(versions.PgbouncerExporter, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.URL, "https://github.com/prometheus-community/pgbouncer_exporter/releases/download/v0.12.1/") {
		t.Errorf("url %s", p.URL)
	}
	if _, err := ArtifactFor("pgbackrest_exporter", "amd64"); err == nil {
		t.Error("pgbackrest_exporter is not pinned")
	}
	if _, err := ArtifactFor(versions.NodeExporter, "riscv64"); err == nil {
		t.Error("riscv64 is not pinned")
	}
}

type entry struct {
	name string
	typ  byte
	body string
}

func tarball(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			hdr.Linkname = "/etc/passwd"
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractOnlyTheMember(t *testing.T) {
	dir := t.TempDir()
	raw := tarball(t, []entry{
		{"x-1.0.linux-amd64/LICENSE", tar.TypeReg, "license"},
		{"../../escape", tar.TypeReg, "evil"},
		{"x-1.0.linux-amd64/x", tar.TypeReg, "binary"},
	})
	dest := filepath.Join(dir, "x")
	if err := Extract(bytes.NewReader(raw), "x-1.0.linux-amd64/x", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest) //nolint:gosec // G304: a test temp file.
	if err != nil || string(got) != "binary" {
		t.Fatalf("got %q, %v", got, err)
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", info.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("extra files: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "escape")); err == nil {
		t.Error("an escaping path was written")
	}
}

func TestExtractRefusesSymlinkAndMissing(t *testing.T) {
	dir := t.TempDir()
	raw := tarball(t, []entry{{"x-1.0.linux-amd64/x", tar.TypeSymlink, ""}})
	if err := Extract(bytes.NewReader(raw), "x-1.0.linux-amd64/x", filepath.Join(dir, "x")); err == nil {
		t.Error("a symlink member was accepted")
	}
	raw = tarball(t, []entry{{"other", tar.TypeReg, "b"}})
	if err := Extract(bytes.NewReader(raw), "x-1.0.linux-amd64/x", filepath.Join(dir, "x")); err == nil {
		t.Error("a missing member was accepted")
	}
}

func TestVerify(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	if err := Verify(strings.NewReader("abc"), hex.EncodeToString(sum[:])); err != nil {
		t.Error(err)
	}
	if err := Verify(strings.NewReader("abd"), hex.EncodeToString(sum[:])); err == nil {
		t.Error("a wrong digest was accepted")
	}
}

func TestRemoveOldKeepsOtherNames(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"pgbouncer_exporter-0.11.0", "pgbouncer_exporter-0.12.1", "pgbouncer_exporter-session", "node_exporter-1.0.0"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeOldIn(root, "pgbouncer_exporter", filepath.Join(root, "pgbouncer_exporter-0.12.1")); err != nil {
		t.Fatal(err)
	}
	var left []string
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := "node_exporter-1.0.0 pgbouncer_exporter-0.12.1 pgbouncer_exporter-session"
	if strings.Join(left, " ") != want {
		t.Errorf("left %v, want %s", left, want)
	}
}
