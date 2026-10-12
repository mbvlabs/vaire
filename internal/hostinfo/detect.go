package hostinfo

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Info is the host description the tuning calculator and steps use (WP1).
type Info struct {
	OS         OS     `json:"os"`
	Arch       string `json:"arch"`   // amd64|arm64
	Kernel     string `json:"kernel"` // uname -r
	RAMMiB     int    `json:"ram_mib"`
	VCPU       int    `json:"vcpu"`
	DataDisk   Disk   `json:"data_disk"`
	PublicIPv4 string `json:"public_ipv4,omitempty"`
	PublicIPv6 string `json:"public_ipv6,omitempty"`
}

// Disk is the block device backing the data directory.
type Disk struct {
	Device     string `json:"device,omitempty"`
	SizeGiB    int    `json:"size_gib"`
	Rotational bool   `json:"rotational"`
	Mount      string `json:"mount,omitempty"`
}

// Detect reads the host facts (§12). It never fails hard: a field that cannot
// be read keeps its zero value. dataDirectory selects the disk; an empty or
// missing path falls back to the nearest existing ancestor, then "/".
func Detect(dataDirectory string) (Info, error) {
	info := Info{Arch: runtime.GOARCH}
	if osInfo, err := ReadOS(); err == nil {
		info.OS = osInfo
	}
	if mib, err := MemTotalMiB(); err == nil {
		info.RAMMiB = mib
	}
	info.Kernel = kernelRelease()
	info.VCPU = vcpuCount()
	info.PublicIPv4 = publicIPv4()
	info.PublicIPv6 = publicIPv6()
	info.DataDisk = detectDisk(dataDirectory)
	return info, nil
}

// kernelRelease reads /proc/sys/kernel/osrelease. It returns "" off Linux.
func kernelRelease() string {
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// vcpuCount honours a cgroup CPU quota before falling back to /proc/cpuinfo,
// because runtime.NumCPU reports the host's cores, not the container's share.
func vcpuCount() int {
	if n := cgroupVCPU(); n > 0 {
		return n
	}
	if n := cpuinfoVCPU(); n > 0 {
		return n
	}
	return runtime.NumCPU()
}

// cgroupVCPU reads the cgroup v2 cpu.max, then the cgroup v1 cfs quota.
func cgroupVCPU() int {
	if raw, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		if n := parseCPUMax(strings.Fields(string(raw))); n > 0 {
			return n
		}
	}
	quotaRaw, err := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	if err != nil {
		return 0
	}
	periodRaw, err := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err != nil {
		return 0
	}
	quota, err1 := strconv.Atoi(strings.TrimSpace(string(quotaRaw)))
	period, err2 := strconv.Atoi(strings.TrimSpace(string(periodRaw)))
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return (quota + period - 1) / period
}

// parseCPUMax turns cpu.max fields ("<quota> <period>") into a vCPU count. The
// literal "max" means no quota.
func parseCPUMax(fields []string) int {
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, err1 := strconv.Atoi(fields[0])
	period, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return (quota + period - 1) / period
}

// cpuinfoVCPU counts "processor" lines in /proc/cpuinfo.
func cpuinfoVCPU() int {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}

// publicIPv4 is the source address the default route would use, or "" when
// there is none.
func publicIPv4() string {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	udp, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	addr, ok := netip.AddrFromSlice(udp.IP)
	if !ok {
		return ""
	}
	return addr.Unmap().String()
}

// publicIPv6 is the source address the IPv6 default route would use, or "".
func publicIPv6() string {
	conn, err := net.Dial("udp6", "[2606:4700:4700::1111]:9")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	udp, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	addr, ok := netip.AddrFromSlice(udp.IP)
	if !ok || !addr.Is6() || addr.Is4In6() {
		return ""
	}
	if addr.IsLinkLocalUnicast() || addr.IsLoopback() {
		return ""
	}
	return addr.String()
}

// detectDisk resolves the block device backing path and its size/rotation.
func detectDisk(path string) Disk {
	mount := nearestExisting(path)
	var d Disk
	d.Mount = mount
	source, target, ok := findmnt(mount)
	if ok {
		d.Mount = target
		d.Device = source
		if device, sizeGiB, rotational, ok := blockInfo(source); ok {
			d.Device = device
			d.SizeGiB = sizeGiB
			d.Rotational = rotational
			return d
		}
	}
	if sizeGiB, ok := statfsGiB(d.Mount); ok {
		d.SizeGiB = sizeGiB
	}
	return d
}

// nearestExisting walks up path until a directory exists.
func nearestExisting(path string) string {
	if path == "" {
		return "/"
	}
	p := path
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "/"
		}
		p = parent
	}
}

// findmnt runs findmnt -no SOURCE,TARGET -T <path> and returns the block
// device and mount point.
func findmnt(path string) (source, target string, ok bool) {
	out, err := exec.Command("findmnt", "-no", "SOURCE,TARGET", "-T", path).Output() //nolint:gosec // G204: fixed findmnt invocation with an operator-supplied path.
	if err != nil {
		return "", "", false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// blockInfo resolves a device node to its parent whole disk and reads its
// size and rotational flag from /sys/class/block.
func blockInfo(device string) (name string, sizeGiB int, rotational bool, ok bool) {
	if !strings.HasPrefix(device, "/dev/") {
		return "", 0, false, false
	}
	dir, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", filepath.Base(device)))
	if err != nil {
		return "", 0, false, false
	}
	for {
		slaves, err := os.ReadDir(filepath.Join(dir, "slaves"))
		if err == nil && len(slaves) > 0 {
			next := filepath.Join(dir, "slaves", slaves[0].Name())
			if resolved, err := filepath.EvalSymlinks(next); err == nil {
				dir = resolved
				continue
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
			dir = filepath.Dir(dir)
			continue
		}
		break
	}
	rotRaw, _ := os.ReadFile(filepath.Join(dir, "queue", "rotational"))
	sizeRaw, _ := os.ReadFile(filepath.Join(dir, "size"))
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(sizeRaw)), 10, 64)
	if err != nil || sectors <= 0 {
		return filepath.Base(dir), 0, strings.TrimSpace(string(rotRaw)) == "1", false
	}
	return filepath.Base(dir), int(sectors * 512 / (1 << 30)), strings.TrimSpace(string(rotRaw)) == "1", true
}
