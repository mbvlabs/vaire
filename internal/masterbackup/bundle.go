package masterbackup

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
)

// Schema is the manifest's schema.
const Schema = "vaire-master-bundle/v1"

// ManifestName is the tar entry that comes first.
const ManifestName = "manifest.json"

// maxBundleSize bounds a bundle read from the bucket, a file or a request.
const maxBundleSize = 32 << 20

// Meta describes a bundle.
type Meta struct {
	Server       string
	Node         string
	Epoch        int64
	CreatedAt    time.Time
	VaireVersion string
}

// Manifest is manifest.json inside the tar.
type Manifest struct {
	Schema       string `json:"schema"`
	VaireVersion string `json:"vaire_version"`
	Server       string `json:"server"`
	Node         string `json:"node"`
	Epoch        int64  `json:"epoch"`
	Revision     string `json:"revision"`
	CreatedAt    string `json:"created_at"`
	Content      string `json:"content_hash"`
	Files        []File `json:"files"`
}

// Bundle is an opened bundle: its manifest and its files with their data.
type Bundle struct {
	Manifest Manifest
	Files    []File
}

// File returns the bundled file at p.
func (b Bundle) File(p string) (File, bool) {
	for _, f := range b.Files {
		if f.Path == p {
			return f, true
		}
	}
	return File{}, false
}

// Build writes the tar: manifest.json first, then each file in order, with
// fixed mtimes and ids, so the same snapshot gives the same bytes.
func Build(meta Meta, files []File) ([]byte, Manifest, error) {
	created := meta.CreatedAt.UTC().Truncate(time.Second)
	m := Manifest{
		Schema:       Schema,
		VaireVersion: meta.VaireVersion,
		Server:       meta.Server,
		Node:         meta.Node,
		Epoch:        meta.Epoch,
		Revision:     Revision(files),
		CreatedAt:    created.Format(time.RFC3339),
		Content:      ContentHash(files),
		Files:        files,
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, m, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, mode int64, data []byte) error {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     mode,
			Size:     int64(len(data)),
			ModTime:  created,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := write(ManifestName, 0o600, raw); err != nil {
		return nil, m, err
	}
	for _, f := range files {
		if !Allowed(f.Path) {
			return nil, m, fmt.Errorf("%s is not part of the bundle", f.Path)
		}
		if err := write(strings.TrimPrefix(f.Path, "/"), int64(f.Mode), f.Data); err != nil {
			return nil, m, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, m, err
	}
	return buf.Bytes(), m, nil
}

// Seal encrypts a tar to an age X25519 recipient.
func Seal(tarball []byte, recipient string) ([]byte, error) {
	rcpt, err := age.ParseX25519Recipient(strings.TrimSpace(recipient))
	if err != nil {
		return nil, fmt.Errorf("the recovery recipient is invalid: %w", err)
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, rcpt)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(tarball); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ErrCorrupt marks a bundle that fails decryption or a check.
var ErrCorrupt = errors.New("the bundle is corrupt")

// Open decrypts a bundle with the recovery identity and validates it.
func Open(ciphertext []byte, identity string) (Bundle, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(identity))
	if err != nil {
		return Bundle{}, fmt.Errorf("the recovery identity is invalid: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		return Bundle{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBundleSize+1))
	if err != nil {
		return Bundle{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if len(raw) > maxBundleSize {
		return Bundle{}, fmt.Errorf("%w: larger than %d bytes", ErrCorrupt, maxBundleSize)
	}
	return ReadTar(raw)
}

// ReadTar reads and validates a plaintext bundle: manifest first, only
// regular files the bundle may hold, each once and in the manifest, with the
// manifest's size and sha256, and nothing missing.
func ReadTar(raw []byte) (Bundle, error) {
	tr := tar.NewReader(bytes.NewReader(raw))
	var b Bundle
	first := true
	byPath := map[string]File{}
	var got []File
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return Bundle{}, fmt.Errorf("%w: %s is not a regular file", ErrCorrupt, hdr.Name)
		}
		if hdr.Size > MaxFileSize && hdr.Name != ManifestName || hdr.Size > maxBundleSize {
			return Bundle{}, fmt.Errorf("%w: %s is too large", ErrCorrupt, hdr.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		if first {
			if hdr.Name != ManifestName {
				return Bundle{}, fmt.Errorf("%w: the first entry is %s, not %s", ErrCorrupt, hdr.Name, ManifestName)
			}
			if err := json.Unmarshal(data, &b.Manifest); err != nil {
				return Bundle{}, fmt.Errorf("%w: manifest.json: %w", ErrCorrupt, err)
			}
			if b.Manifest.Schema != Schema {
				return Bundle{}, fmt.Errorf("%w: schema %q, want %q", ErrCorrupt, b.Manifest.Schema, Schema)
			}
			for _, f := range b.Manifest.Files {
				if !Allowed(f.Path) {
					return Bundle{}, fmt.Errorf("%w: the manifest names %s, which a bundle never holds", ErrCorrupt, f.Path)
				}
				if _, dup := byPath[f.Path]; dup {
					return Bundle{}, fmt.Errorf("%w: the manifest names %s twice", ErrCorrupt, f.Path)
				}
				byPath[f.Path] = f
			}
			first = false
			continue
		}
		p := "/" + hdr.Name
		want, ok := byPath[p]
		if !ok || !Allowed(p) {
			return Bundle{}, fmt.Errorf("%w: %s is not in the manifest", ErrCorrupt, hdr.Name)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != want.Size || hex.EncodeToString(sum[:]) != want.SHA256 {
			return Bundle{}, fmt.Errorf("%w: %s does not match its manifest entry", ErrCorrupt, p)
		}
		want.Data = data
		got = append(got, want)
		delete(byPath, p)
	}
	if first {
		return Bundle{}, fmt.Errorf("%w: the tar is empty", ErrCorrupt)
	}
	if len(byPath) > 0 {
		for p := range byPath {
			return Bundle{}, fmt.Errorf("%w: %s is in the manifest but not in the tar", ErrCorrupt, p)
		}
	}
	b.Files = got
	if ContentHash(got) != b.Manifest.Content {
		return Bundle{}, fmt.Errorf("%w: the content hash does not match", ErrCorrupt)
	}
	return b, nil
}

// SHA256 is the hex sha256 of b, the sidecar's ciphertext hash.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
