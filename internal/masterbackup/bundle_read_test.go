package masterbackup

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReadTarValidatesManifestEntries(t *testing.T) {
	file := NewFile(ClusterPath, "root", "root", 0o600, []byte("cluster"))
	disallowed := NewFile("/etc/shadow", "root", "root", 0o600, []byte("secret"))
	for _, tc := range []struct {
		name    string
		files   []File
		entries [][2]string
		content string
	}{
		{name: "duplicate manifest path", files: []File{file, file}},
		{name: "disallowed manifest path", files: []File{disallowed}},
		{name: "missing payload", files: []File{file}},
		{name: "wrong payload", files: []File{file}, entries: [][2]string{{strings.TrimPrefix(file.Path, "/"), "wrong"}}},
		{name: "duplicate payload", files: []File{file}, entries: [][2]string{
			{strings.TrimPrefix(file.Path, "/"), string(file.Data)},
			{strings.TrimPrefix(file.Path, "/"), string(file.Data)},
		}},
		{name: "wrong content hash", files: []File{file}, content: "wrong", entries: [][2]string{
			{strings.TrimPrefix(file.Path, "/"), string(file.Data)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := json.Marshal(Manifest{Schema: Schema, Files: tc.files, Content: tc.content})
			if err != nil {
				t.Fatal(err)
			}
			entries := make([][2]string, 0, 1+len(tc.entries))
			entries = append(entries, [2]string{ManifestName, string(manifest)})
			entries = append(entries, tc.entries...)
			if _, err := ReadTar(tarOf(t, entries...)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ReadTar error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestReadTarRejectsOversizedEntryBeforeReadingPayload(t *testing.T) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "etc/vaire/cluster.toml", Typeflag: tar.TypeReg, Size: MaxFileSize + 1}); err != nil {
		t.Fatal(err)
	}
	// No payload follows: the size check must run before trying to read it.
	if _, err := ReadTar(buf.Bytes()); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("ReadTar error = %v, want oversized-entry error", err)
	}
}
