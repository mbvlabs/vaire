package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Size is one row of the size table (RFC 7.4): the memory that drives a
// cluster's tuning and MemoryHigh, its CPU weight and its max_connections.
type Size struct {
	Name           string
	MemoryMiB      int
	CPUWeight      int
	MaxConnections int
}

// SizeCustom means only a cluster's explicit keys count.
const SizeCustom = "custom"

// DefaultSize is [postgres] default_size until the operator changes it.
const DefaultSize = "medium"

// Cluster resource defaults that no size sets.
const (
	DefaultIOWeight        = 100
	DefaultCPUWeight       = 100
	DefaultSessionPoolSize = 10
	// ReservedConnections are the slots kept free beyond both pools:
	// reserved_connections (3) plus superuser_reserved_connections (0), as
	// tune sets them. Ordinary roles (the pools) get max_connections minus
	// both, so both pools full still leave the three to vaire_admin and
	// vaire_monitor (pg_use_reserved_connections; vaire_monitor capped at two,
	// one postgres_exporter probe) and to postgres.
	ReservedConnections = 3
)

// Sizes is the size table, smallest first.
var Sizes = []Size{
	{Name: "nano", MemoryMiB: 256, CPUWeight: 25, MaxConnections: 20},
	{Name: "small", MemoryMiB: 512, CPUWeight: 50, MaxConnections: 30},
	{Name: "medium", MemoryMiB: 1024, CPUWeight: 100, MaxConnections: 50},
	{Name: "large", MemoryMiB: 2048, CPUWeight: 200, MaxConnections: 100},
	{Name: "xlarge", MemoryMiB: 4096, CPUWeight: 400, MaxConnections: 200},
}

// LookupSize returns the named size.
func LookupSize(name string) (Size, bool) {
	for _, s := range Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return Size{}, false
}

// SizeNames is every size, including custom, for messages.
func SizeNames() []string {
	out := make([]string, 0, len(Sizes)+1)
	for _, s := range Sizes {
		out = append(out, s.Name)
	}
	return append(out, SizeCustom)
}

// Resources are a cluster's resolved limits: its size with the explicit
// keys applied. Zero MemoryMaxMiB and an empty CPUQuota mean unset.
type Resources struct {
	Size            string
	MemoryMiB       int
	MemoryMaxMiB    int
	CPUWeight       int
	CPUQuota        string
	IOWeight        int
	MaxConnections  int
	PoolSize        int
	SessionPoolSize int
}

// ResolveResources applies "explicit values win" (RFC 7.4): the cluster's
// size (or defaultSize when it names none) gives memory, CPU weight and
// max_connections, and every explicit key replaces its part. A custom size
// takes only explicit keys: memory is required, the CPU weight defaults to
// systemd's 100, and max_connections comes from the largest size whose
// memory fits in it. settings.max_connections overrides max_connections.
// pool_size 0 is what is left of max_connections after the session pool
// and the reserved slots.
func ResolveResources(spec ClusterSpec, defaultSize string) (Resources, error) {
	name := spec.Size
	if name == "" {
		name = defaultSize
	}
	r := Resources{Size: name, CPUQuota: spec.CPUQuota, IOWeight: DefaultIOWeight, SessionPoolSize: DefaultSessionPoolSize}
	if name == SizeCustom {
		if spec.Memory == "" {
			return Resources{}, errors.New("size custom needs memory")
		}
		r.CPUWeight = DefaultCPUWeight
	} else {
		size, ok := LookupSize(name)
		if !ok {
			return Resources{}, fmt.Errorf("size %q must be one of %s", name, strings.Join(SizeNames(), ", "))
		}
		r.MemoryMiB, r.CPUWeight, r.MaxConnections = size.MemoryMiB, size.CPUWeight, size.MaxConnections
	}
	if spec.Memory != "" {
		mib, err := ParseMemoryMiB(spec.Memory)
		if err != nil {
			return Resources{}, fmt.Errorf("memory %q %w", spec.Memory, err)
		}
		r.MemoryMiB = mib
	}
	if name == SizeCustom {
		r.MaxConnections = connectionsFor(r.MemoryMiB)
	}
	if spec.MemoryMax != "" {
		mib, err := ParseMemoryMiB(spec.MemoryMax)
		if err != nil {
			return Resources{}, fmt.Errorf("memory_max %q %w", spec.MemoryMax, err)
		}
		r.MemoryMaxMiB = mib
	}
	if spec.CPUWeight != 0 {
		r.CPUWeight = spec.CPUWeight
	}
	if spec.IOWeight != 0 {
		r.IOWeight = spec.IOWeight
	}
	if v, ok := spec.Settings["max_connections"]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			return Resources{}, fmt.Errorf("settings.max_connections %q is not a positive integer", v)
		}
		r.MaxConnections = n
	}
	if spec.SessionPoolSize != 0 {
		r.SessionPoolSize = spec.SessionPoolSize
	}
	r.PoolSize = spec.PoolSize
	if r.PoolSize == 0 {
		r.PoolSize = r.MaxConnections - r.SessionPoolSize - ReservedConnections
	}
	return r, nil
}

// connectionsFor is max_connections for a custom size: that of the largest
// size whose memory fits in mib, and nano's below it.
func connectionsFor(mib int) int {
	out := Sizes[0].MaxConnections
	for _, s := range Sizes {
		if s.MemoryMiB <= mib {
			out = s.MaxConnections
		}
	}
	return out
}

var memoryPattern = regexp.MustCompile(`^(\d+)\s*(KiB|MiB|GiB|TiB|kB|MB|GB|TB)$`)

// ParseMemoryMiB parses a memory amount such as "768MiB" or "2GiB" into MiB.
// The PostgreSQL units (kB, MB, GB, TB) are binary, as PostgreSQL reads them.
func ParseMemoryMiB(v string) (int, error) {
	m := memoryPattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, errors.New("must be an amount such as 768MiB or 2GiB")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, errors.New("is too large")
	}
	var mib int
	switch m[2] {
	case "KiB", "kB":
		mib = n / 1024
	case "MiB", "MB":
		mib = n
	case "GiB", "GB":
		mib = n * 1024
	case "TiB", "TB":
		mib = n * 1024 * 1024
	}
	if mib < 1 {
		return 0, errors.New("must be at least 1MiB")
	}
	return mib, nil
}
