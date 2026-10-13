package hostinfo

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Memory is what /proc/meminfo says about what can still start (RFC 7.5):
// with vm.overcommit_memory = 2, an allocation past CommitLimit fails, and a
// cluster with huge_pages = on needs free pages in the pool.
type Memory struct {
	CommitLimitMiB  int `json:"commit_limit_mib"`
	CommittedMiB    int `json:"committed_mib"`
	AvailableMiB    int `json:"available_mib"`
	HugePagesTotal  int `json:"huge_pages_total"`
	HugePagesFree   int `json:"huge_pages_free"`
	HugePageSizeKiB int `json:"huge_page_size_kib"`
}

// ReadMemory reads /proc/meminfo. ok is false when it can't be read, as off
// Linux.
func ReadMemory() (Memory, bool) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Memory{}, false
	}
	return ParseMeminfo(raw), true
}

// ParseMeminfo picks the commit and huge-page fields out of /proc/meminfo.
func ParseMeminfo(raw []byte) Memory {
	var m Memory
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "CommitLimit:":
			m.CommitLimitMiB = n / 1024
		case "Committed_AS:":
			m.CommittedMiB = n / 1024
		case "MemAvailable:":
			m.AvailableMiB = n / 1024
		case "HugePages_Total:":
			m.HugePagesTotal = n
		case "HugePages_Free:":
			m.HugePagesFree = n
		case "Hugepagesize:":
			m.HugePageSizeKiB = n
		}
	}
	return m
}

// IOWeight reports whether IOWeight= has any effect on device (a whole disk
// such as vda): only the BFQ scheduler and the kernel's io.cost model honour
// it (RFC 7.2). reason says which, or why not.
func IOWeight(device string) (enforced bool, reason string) {
	if device == "" {
		return false, "the data disk's device is unknown"
	}
	name := filepath.Base(device)
	if raw, err := os.ReadFile(filepath.Join("/sys/block", name, "queue", "scheduler")); err == nil { //nolint:gosec // G304: a sysfs path under /sys/block.
		if strings.Contains(string(raw), "[bfq]") {
			return true, name + " uses the bfq scheduler"
		}
	}
	if devno, err := os.ReadFile(filepath.Join("/sys/block", name, "dev")); err == nil { //nolint:gosec // G304: a sysfs path under /sys/block.
		if qos, err := os.ReadFile("/sys/fs/cgroup/io.cost.qos"); err == nil {
			for _, line := range strings.Split(string(qos), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == strings.TrimSpace(string(devno)) && fields[1] == "enable=1" {
					return true, name + " has the io.cost model enabled"
				}
			}
		}
	}
	return false, "not enforced: " + name + " uses neither bfq nor io.cost"
}

// UsedGiB is the space in use on the filesystem holding path, or ok=false.
func UsedGiB(path string) (int, bool) {
	total, ok := statfsGiB(path)
	if !ok {
		return 0, false
	}
	free, ok := FreeBytes(path)
	if !ok {
		return 0, false
	}
	return maxZero(total - int(free/(1<<30))), true //nolint:gosec // G115: free bytes in GiB fit in int.
}

func maxZero(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
