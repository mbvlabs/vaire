package release

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	ChannelStable = "stable"
	ChannelEdge   = "edge"

	Issuer = "https://token.actions.githubusercontent.com"
)

var (
	stableVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	edgeVersionPattern   = regexp.MustCompile(`^edge-[0-9a-f]{12}$`)
)

// ChannelRoot is the HTTPS origin for a channel's install.sh and pointer manifest.
func ChannelRoot(channel string) (string, error) {
	base := strings.TrimRight(currentBaseURL(), "/")
	switch channel {
	case ChannelStable:
		return base, nil
	case ChannelEdge:
		return base + "/edge", nil
	default:
		return "", fmt.Errorf("release channel %q is invalid", channel)
	}
}

// Identity returns the exact certificate SAN for a channel and version.
func Identity(channel, version string) (string, error) {
	if err := matchVersion(channel, version); err != nil {
		return "", err
	}
	switch channel {
	case ChannelStable:
		return "https://github.com/mbvlabs/eru/.github/workflows/publish-stable.yml@refs/tags/v" + version, nil
	case ChannelEdge:
		return "https://github.com/mbvlabs/eru/.github/workflows/publish-edge.yml@refs/heads/master", nil
	default:
		return "", fmt.Errorf("release channel %q is invalid", channel)
	}
}

func matchVersion(channel, version string) error {
	switch channel {
	case ChannelStable:
		if !stableVersionPattern.MatchString(version) {
			return fmt.Errorf("stable release version %q is invalid", version)
		}
	case ChannelEdge:
		if !edgeVersionPattern.MatchString(version) {
			return fmt.Errorf("edge release version %q is invalid", version)
		}
	default:
		return fmt.Errorf("release channel %q is invalid", channel)
	}
	return nil
}

func versionRoot(version string) string {
	return strings.TrimRight(currentBaseURL(), "/") + "/releases/" + version
}
