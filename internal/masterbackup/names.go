package masterbackup

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bundles live under <prefix>/master/: <name>.tar.age and its sidecar
// <name>.json, next to epoch.json.
const (
	BucketDir     = "master/"
	BundleSuffix  = ".tar.age"
	SidecarSuffix = ".json"
	EpochObject   = "epoch.json"
	nameTimeFmt   = "20060102T150405Z"
	revisionChars = 12
)

var nameRE = regexp.MustCompile(`^(\d{8}T\d{6}Z)-e(\d+)-r([0-9a-f]{1,64})$`)

// Name is <UTC ts>-e<epoch>-r<first 12 of the revision>.
func Name(created time.Time, epoch int64, revision string) string {
	rev := revision
	if len(rev) > revisionChars {
		rev = rev[:revisionChars]
	}
	if rev == "" {
		rev = "0"
	}
	return fmt.Sprintf("%s-e%d-r%s", created.UTC().Format(nameTimeFmt), epoch, rev)
}

// Name is the name the node gave the bundle m describes: it names a bundle
// by the second it built it, which the manifest records as created_at.
func (m Manifest) Name() string {
	created, _ := time.Parse(time.RFC3339, m.CreatedAt)
	return Name(created, m.Epoch, m.Revision)
}

// Parsed is a parsed bundle name.
type Parsed struct {
	Name      string
	CreatedAt time.Time
	Epoch     int64
	Revision  string
}

// ParseName parses a bundle name (without suffix).
func ParseName(name string) (Parsed, bool) {
	m := nameRE.FindStringSubmatch(name)
	if m == nil {
		return Parsed{}, false
	}
	t, err := time.Parse(nameTimeFmt, m[1])
	if err != nil {
		return Parsed{}, false
	}
	e, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return Parsed{}, false
	}
	return Parsed{Name: name, CreatedAt: t, Epoch: e, Revision: m[3]}, true
}

// SplitKey returns the bundle name and whether key is its bundle (true) or
// sidecar (false). ok is false for anything else.
func SplitKey(key string) (name string, isBundle, ok bool) {
	base := key[strings.LastIndex(key, "/")+1:]
	switch {
	case strings.HasSuffix(base, BundleSuffix):
		name, isBundle = strings.TrimSuffix(base, BundleSuffix), true
	case strings.HasSuffix(base, SidecarSuffix):
		name = strings.TrimSuffix(base, SidecarSuffix)
	default:
		return "", false, false
	}
	if _, ok := ParseName(name); !ok {
		return "", false, false
	}
	return name, isBundle, true
}

// Sidecar is <name>.json, written after the bundle. It holds no plaintext
// file hashes: those are only inside the encrypted manifest.
type Sidecar struct {
	Schema     string        `json:"schema"`
	Name       string        `json:"name"`
	Server     string        `json:"server"`
	Node       string        `json:"node"`
	Epoch      int64         `json:"epoch"`
	Revision   string        `json:"revision"`
	CreatedAt  string        `json:"created_at"`
	EruVersion string        `json:"eru_version"`
	Files      []SidecarFile `json:"files"`
	Size       int64         `json:"size"`
	SHA256     string        `json:"sha256"`
}

// SidecarFile is one file's path and size.
type SidecarFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// SidecarSchema is the sidecar's schema.
const SidecarSchema = "eru-master-sidecar/v1"

// NewSidecar describes a sealed bundle.
func NewSidecar(name string, m Manifest, ciphertext []byte) Sidecar {
	s := Sidecar{
		Schema: SidecarSchema, Name: name, Server: m.Server, Node: m.Node, Epoch: m.Epoch,
		Revision: m.Revision, CreatedAt: m.CreatedAt, EruVersion: m.EruVersion,
		Size: int64(len(ciphertext)), SHA256: SHA256(ciphertext),
	}
	for _, f := range m.Files {
		s.Files = append(s.Files, SidecarFile{Path: f.Path, Size: f.Size})
	}
	return s
}

// Keep returns the names to keep (D4): every bundle younger than days, the
// newest of each ISO week younger than weeks, and always the newest
// `newest`. Names that don't parse are kept: pruning never touches them.
func Keep(names []string, now time.Time, days, weeks, newest int) map[string]bool {
	keep := map[string]bool{}
	var parsed []Parsed
	for _, n := range names {
		p, ok := ParseName(n)
		if !ok {
			keep[n] = true
			continue
		}
		parsed = append(parsed, p)
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].Name > parsed[j].Name })
	for i, p := range parsed {
		if i < newest {
			keep[p.Name] = true
		}
	}
	dayCut := now.Add(-time.Duration(days) * 24 * time.Hour)
	weekCut := now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour)
	seenWeek := map[string]bool{}
	for _, p := range parsed {
		if !p.CreatedAt.Before(dayCut) {
			keep[p.Name] = true
			continue
		}
		if p.CreatedAt.Before(weekCut) {
			continue
		}
		y, w := p.CreatedAt.ISOWeek()
		wk := fmt.Sprintf("%d-%02d", y, w)
		if !seenWeek[wk] {
			seenWeek[wk] = true
			keep[p.Name] = true
		}
	}
	return keep
}

// Pick returns the bundle a restore uses: the newest complete bundle of the
// highest epoch at or before `before` (zero means no limit), or by exact
// name. complete maps a name to whether its sidecar exists.
func Pick(complete map[string]bool, name string, before time.Time) (Parsed, bool) {
	if name != "" {
		p, ok := ParseName(name)
		if !ok || !complete[name] {
			return Parsed{}, false
		}
		return p, true
	}
	var best Parsed
	found := false
	for n, ok := range complete {
		if !ok {
			continue
		}
		p, parsed := ParseName(n)
		if !parsed || (!before.IsZero() && p.CreatedAt.After(before)) {
			continue
		}
		if !found || p.Epoch > best.Epoch || (p.Epoch == best.Epoch && p.Name > best.Name) {
			best, found = p, true
		}
	}
	return best, found
}
