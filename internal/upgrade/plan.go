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
// build follows the logical channel of its version: alpha and beta follow
// edge and rc follows rc. A final release follows its own major line
// (stable-1.x for 1.y.z), so moving to the next major is an explicit
// --channel stable or stable-<major>.x.
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
			"A dev build has no channel to follow. Pass --channel stable, --channel rc or --channel edge.",
		).WithDetails(map[string]any{"flag": "--channel"})
	}

	if logical, ok := release.VersionChannel(version); ok {
		if logical == release.ChannelStable {
			v, _ := release.ParseStable(version)
			return v.Channel(), nil
		}

		return logical, nil
	}

	return channel, nil
}

// SelectVersionChannel returns the channel for an explicit --version unless
// --channel overrides it. Named alpha and beta versions are edge versions.
func SelectVersionChannel(flag, version string) (string, error) {
	if strings.TrimSpace(flag) == "" && strings.TrimSpace(version) != "" {
		logical, ok := release.VersionChannel(version)
		if !ok {
			return "", output.NewError(
				output.CodeValidation,
				"release version "+version+" is invalid",
				output.ExitConfig,
				"Pass a tagged MAJOR.MINOR.PATCH[-alpha.N|-beta.N|-rc.N] or an edge-<12-hex> version.",
			).WithDetails(map[string]any{"version": version})
		}

		if logical == release.ChannelStable {
			v, _ := release.ParseStable(version)
			return v.Channel(), nil
		}

		return logical, nil
	}

	return SelectChannel(flag)
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

	// Only tagged releases have a semver order. Edge pointers can move
	// between rolling edge-<hash> builds and named alpha/beta versions; that
	// ordering belongs to the publisher's source-chain policy, not here.
	_, fromTagged := release.ParseStable(from.Version)

	_, toTagged := release.ParseStable(m.Version)
	tagged := fromTagged && toTagged
	older := tagged && cmpSemver(m.Version, from.Version) < 0

	if tagged && !versionSet && older {
		plan.To = from
		return plan
	}

	plan.Changed = true

	if tagged && older {
		message := "installing an older release"
		if from.Channel == release.ChannelStable && m.Channel == release.ChannelStable {
			message = "installing an older stable version"
		}

		plan.Warnings = append(plan.Warnings, output.Warning{Code: "downgrade", Message: message})
	}

	if tagged {
		fromV, _ := release.ParseStable(from.Version)
		toV, _ := release.ParseStable(m.Version)

		if fromV.Major != toV.Major {
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

// cmpSemver orders tagged releases by semver precedence, so
// 1.0.0-alpha.2 < 1.0.0-alpha.10 < 1.0.0-beta.1 < 1.0.0-rc.1 < 1.0.0.
func cmpSemver(a, b string) int {
	return release.CompareVersions(a, b)
}
