// Package hostinfo detects the host eru runs on. Phase 1 needs the OS release
// and RAM; Phase 2 adds CPU, disks and the kernel.
package hostinfo

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
)

// OSReleasePath is the os-release file.
const OSReleasePath = "/etc/os-release"

// OS is the subset of os-release eru checks.
type OS struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
	Pretty    string `json:"pretty_name"`
}

// supported lists the server releases eru supports (§0.1).
var supported = map[string][]string{
	"debian": {"13"},
	"ubuntu": {"26.04"},
}

// SupportedList is the human-readable list of supported server releases.
const SupportedList = "Debian 13, Ubuntu 26.04"

// Supported reports whether o is a supported server release.
func (o OS) Supported() bool {
	for _, v := range supported[o.ID] {
		if v == o.VersionID {
			return true
		}
	}
	return false
}

// ParseOSRelease parses os-release content.
func ParseOSRelease(raw []byte) OS {
	var o OS
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			o.ID = value
		case "VERSION_ID":
			o.VersionID = value
		case "PRETTY_NAME":
			o.Pretty = value
		}
	}
	return o
}

// ReadOS reads /etc/os-release.
func ReadOS() (OS, error) {
	raw, err := os.ReadFile(OSReleasePath)
	if err != nil {
		return OS{}, err
	}
	return ParseOSRelease(raw), nil
}

// MemTotalMiB returns MemTotal from /proc/meminfo in MiB.
func MemTotalMiB() (int, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, err
			}
			return kib / 1024, nil
		}
	}
	return 0, os.ErrNotExist
}

// GoArch maps uname -m output to a Go architecture name. It returns "" for an
// unsupported machine.
func GoArch(machine string) string {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return ""
	}
}
