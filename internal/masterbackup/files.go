// Package masterbackup is the master bundle (Phase 5): the age-encrypted tar
// of what is born on the master and can't be regenerated, its manifest and
// sidecar, the names and retention of bundles, the dirty flag that triggers a
// new one, and the epoch decision that fences an old master. It has no
// dependency on the rest of eru, so the operator (which opens bundles) and
// the node (which builds them) share it, and every writer of a bundled file
// can mark the state dirty without an import cycle.
package masterbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
)

// Node paths of the master backup's own state. None of them is in the bundle.
const (
	// Dir holds the local bundles, the dirty flag, last.json and the local
	// epoch. root 0700.
	Dir = "/var/lib/eru/master-backup"
	// DirtyPath is touched by every writer of a bundled file; the path unit
	// eru-master-backup.path starts the job while it exists.
	DirtyPath = Dir + "/dirty"
	// LastPath records the last bundle: name, content hash, upload state.
	LastPath = Dir + "/last.json"
	// EpochPath is the node's copy of the bucket's epoch.json.
	EpochPath = Dir + "/epoch.json"
	// InstancePath is this install's random id. A restore places a new one.
	InstancePath = "/var/lib/eru/instance"
	// FencedPath exists once the node has seen a newer epoch for another
	// instance in the bucket. It lives in /etc/eru (0755), not /var/lib/eru
	// (root 0700), because the archive wrapper runs as postgres and must be
	// able to see it.
	FencedPath = "/etc/eru/fenced"
	// RecipientPath is the recovery kit's age recipient on the node, written
	// with or without a bucket so bundles can always be encrypted.
	RecipientPath = "/etc/eru/runtime/recovery.recipient"
)

// Bundled paths (D1). They are absolute node paths; a test passes a root.
const (
	ClusterPath    = "/etc/eru/cluster.toml"
	NodePath       = "/etc/eru/node.toml"
	OperatorsPath  = "/etc/eru/operators.toml"
	RecordsPath    = "/var/lib/eru/records.json"
	ProtectPath    = "/etc/eru/protect.json"
	WGKeyPath      = "/etc/wireguard/eru.key"
	TLSCertPath    = "/etc/eru/tls/server.crt"
	TLSKeyPath     = "/etc/eru/tls/server.key"
	HostKeyGlob    = "/etc/ssh/ssh_host_*_key"
	HostKeyPubGlob = "/etc/ssh/ssh_host_*_key.pub"
	ACMEGlob       = "/etc/eru/acme/*"
)

// Spec is one path, or glob, of the bundle.
type Spec struct {
	Path     string
	Glob     bool
	Required bool
}

// Specs is the bundle's file set, in bundle order.
func Specs() []Spec {
	return []Spec{
		{Path: ClusterPath, Required: true},
		{Path: NodePath, Required: true},
		{Path: OperatorsPath},
		{Path: RecordsPath},
		{Path: ProtectPath},
		{Path: WGKeyPath},
		{Path: HostKeyGlob, Glob: true},
		{Path: HostKeyPubGlob, Glob: true},
		{Path: TLSCertPath},
		{Path: TLSKeyPath},
		{Path: ACMEGlob, Glob: true},
	}
}

// Allowed reports whether p is a path the bundle may hold.
func Allowed(p string) bool {
	if p != path.Clean(p) || !path.IsAbs(p) {
		return false
	}
	for _, s := range Specs() {
		if !s.Glob && s.Path == p {
			return true
		}
		if s.Glob {
			if ok, _ := path.Match(s.Path, p); ok {
				return true
			}
		}
	}
	return false
}

// File is one bundled file. Data travels in the tar, never in the manifest.
type File struct {
	Path   string `json:"path"`
	Owner  string `json:"owner"`
	Group  string `json:"group"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"-"`
}

// MaxFileSize bounds one bundled file; everything in the set is kilobytes.
const MaxFileSize = 4 << 20

// Snapshot reads the bundle's files under root ("" or "/" for the node).
// Paths in the result are node paths. A missing required file is an error;
// a missing optional one is left out.
func Snapshot(root string) ([]File, error) {
	var out []File
	seen := map[string]bool{}
	for _, s := range Specs() {
		paths := []string{s.Path}
		if s.Glob {
			matches, err := filepath.Glob(filepath.Join(root, s.Path))
			if err != nil {
				return nil, err
			}
			paths = paths[:0]
			for _, m := range matches {
				if info, err := os.Lstat(m); err != nil || !info.Mode().IsRegular() {
					// A directory or link beside the keys isn't bundled.
					continue
				}
				rel, err := filepath.Rel(rootDir(root), m)
				if err != nil {
					return nil, err
				}
				paths = append(paths, "/"+filepath.ToSlash(rel))
			}
			sort.Strings(paths)
		}
		for _, p := range paths {
			if seen[p] {
				continue
			}
			f, err := readFile(root, p)
			if errors.Is(err, fs.ErrNotExist) {
				if s.Required {
					return nil, fmt.Errorf("%s is missing", p)
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			seen[p] = true
			out = append(out, f)
		}
	}
	return out, nil
}

func rootDir(root string) string {
	if root == "" {
		return "/"
	}
	return root
}

func readFile(root, p string) (File, error) {
	full := filepath.Join(rootDir(root), filepath.FromSlash(p))
	info, err := os.Lstat(full)
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("%s is not a regular file", p)
	}
	if info.Size() > MaxFileSize {
		return File{}, fmt.Errorf("%s is larger than %d bytes", p, MaxFileSize)
	}
	data, err := os.ReadFile(full) //nolint:gosec // G304: a fixed bundle path under root.
	if err != nil {
		return File{}, err
	}
	owner, group := "root", "root"
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		owner, group = userName(st.Uid), groupName(st.Gid)
	}
	return NewFile(p, owner, group, uint32(info.Mode().Perm()), data), nil
}

// NewFile builds a File and its hash.
func NewFile(p, owner, group string, mode uint32, data []byte) File {
	sum := sha256.Sum256(data)
	return File{Path: p, Owner: owner, Group: group, Mode: mode & 0o777, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: data}
}

func userName(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(id); err == nil {
		return u.Username
	}
	return id
}

func groupName(gid uint32) string {
	id := strconv.FormatUint(uint64(gid), 10)
	if g, err := user.LookupGroupId(id); err == nil {
		return g.Name
	}
	return id
}

// ContentHash is the sha256 over the canonical file set: paths, owners,
// modes and contents. Two snapshots with the same hash bundle the same.
func ContentHash(files []File) string {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%o\x00%s\n", f.Path, f.Owner, f.Group, f.Mode, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Revision is the sha256 of cluster.toml in files, the desired-state
// revision state.FileStore reports.
func Revision(files []File) string {
	for _, f := range files {
		if f.Path == ClusterPath {
			return f.SHA256
		}
	}
	return ""
}
