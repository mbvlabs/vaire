package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/release"
)

var managedPrefixes = []string{
	"/opt/homebrew/Cellar",
	"/usr/local/Cellar",
	"/nix/store",
}

// Version is one installed or target release identity.
type Version struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
}

// Plan is the local upgrade decision before download.
type Plan struct {
	Target   string
	From     Version
	To       Version
	Changed  bool
	Warnings []output.Warning
}

// ResolveSelf returns the real path of the running binary.
func ResolveSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	if managedByPackageManager(resolved) {
		return "", output.NewError(
			output.CodePrecondition,
			"this binary is managed by a package manager",
			output.ExitPrecondition,
			"Upgrade with the package manager that installed eru.",
		).WithDetails(map[string]any{"reason": "managed_by_package_manager", "path": resolved})
	}
	return resolved, nil
}

func managedByPackageManager(path string) bool {
	for _, prefix := range managedPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// SelectChannel returns the release channel to follow.
func SelectChannel(flag string) (string, error) {
	if strings.TrimSpace(flag) != "" {
		return flag, nil
	}
	if buildinfo.Channel == "dev" {
		return "", output.NewError(
			output.CodeInputRequired,
			"upgrade needs --channel",
			output.ExitInput,
			"A dev build has no channel to follow. Pass --channel stable or --channel edge.",
		).WithDetails(map[string]any{"flag": "--channel"})
	}
	return buildinfo.Channel, nil
}

// Decide compares the running binary with a verified manifest.
func Decide(from Version, m release.Manifest, versionSet bool) Plan {
	plan := Plan{
		From: from,
		To:   Version{Version: m.Version, Channel: m.Channel},
	}
	if from.Version == m.Version && from.Channel == m.Channel {
		return plan
	}
	if from.Channel == release.ChannelStable && m.Channel == release.ChannelStable && !versionSet {
		if cmpSemver(m.Version, from.Version) < 0 {
			plan.To = from
			return plan
		}
	}
	plan.Changed = true
	if from.Channel == release.ChannelStable && m.Channel == release.ChannelStable && cmpSemver(m.Version, from.Version) < 0 {
		plan.Warnings = append(plan.Warnings, output.Warning{Code: "downgrade", Message: "installing an older stable version"})
	}
	if from.Channel != "" && from.Channel != "dev" && from.Channel != m.Channel {
		plan.Warnings = append(plan.Warnings, output.Warning{
			Code:    "channel_changed",
			Message: fmt.Sprintf("switching channel from %s to %s", from.Channel, m.Channel),
		})
	}
	return plan
}

func cmpSemver(a, b string) int {
	pa, okA := parseSemver(a)
	pb, okB := parseSemver(b)
	if !okA || !okB {
		return strings.Compare(a, b)
	}
	for i := range pa {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}
