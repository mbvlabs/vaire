package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/release"
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
			"Upgrade with the package manager that installed vaire.",
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

// SelectChannel returns the release channel to follow. Without --channel, a
// stable build follows its own major line (stable-1.x for 1.y.z), so moving
// to the next major is an explicit --channel stable or stable-<major>.x.
func SelectChannel(flag string) (string, error) {
	return selectChannel(flag, buildinfo.Channel, buildinfo.Version)
}

func selectChannel(flag, channel, version string) (string, error) {
	if strings.TrimSpace(flag) != "" {
		return flag, nil
	}
	if channel == "dev" {
		return "", output.NewError(
			output.CodeInputRequired,
			"upgrade needs --channel",
			output.ExitInput,
			"A dev build has no channel to follow. Pass --channel stable, --channel stable-<major>.x or --channel edge.",
		).WithDetails(map[string]any{"flag": "--channel"})
	}
	if channel == release.ChannelStable {
		if v, ok := release.ParseStable(version); ok {
			return v.Channel(), nil
		}
	}
	return channel, nil
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
	if from.Channel == release.ChannelStable && m.Channel == release.ChannelStable {
		fromV, okFrom := release.ParseStable(from.Version)
		toV, okTo := release.ParseStable(m.Version)
		if okFrom && okTo && fromV.Major != toV.Major {
			plan.Warnings = append(plan.Warnings, output.Warning{
				Code:    "major_line_changed",
				Message: fmt.Sprintf("switching major line from %s to %s", fromV.Line(), toV.Line()),
			})
		}
	}
	if from.Channel != "" && from.Channel != "dev" && from.Channel != m.Channel {
		plan.Warnings = append(plan.Warnings, output.Warning{
			Code:    "channel_changed",
			Message: fmt.Sprintf("switching channel from %s to %s", from.Channel, m.Channel),
		})
	}
	return plan
}

// cmpSemver orders stable versions by semver precedence, so
// 1.0.0-alpha.2 < 1.0.0-alpha.10 < 1.0.0-beta.1 < 1.0.0-rc.1 < 1.0.0.
func cmpSemver(a, b string) int {
	return release.CompareVersions(a, b)
}
