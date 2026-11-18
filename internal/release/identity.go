package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	ChannelStable = "stable"
	ChannelEdge   = "edge"
	ChannelRC     = "rc"

	Issuer = "https://token.actions.githubusercontent.com"
)

var edgeVersionPattern = regexp.MustCompile(`^edge-[0-9a-f]{12}$`)

// ChannelRoot is the HTTPS origin for a channel's install.sh and pointer
// manifest. stable is the site root, a major line such as stable-1.x lives
// under /1.x, and edge and rc live under /edge and /rc.
func ChannelRoot(channel string) (string, error) {
	base := strings.TrimRight(currentBaseURL(), "/")

	switch channel {
	case ChannelStable:
		return base, nil
	case ChannelEdge:
		return base + "/edge", nil
	case ChannelRC:
		return base + "/rc", nil
	}

	if major, ok := lineMajor(channel); ok {
		return base + "/" + strconv.Itoa(major) + ".x", nil
	}

	return "", fmt.Errorf("release channel %q is invalid", channel)
}

// SigningChannel is the logical channel a partition's manifests name: a major
// line is published as stable. Named alpha and beta versions are edge versions
// and their manifests say edge.
func SigningChannel(channel string) string {
	if _, ok := lineMajor(channel); ok {
		return ChannelStable
	}

	return channel
}

// Identity returns the exact certificate SAN for a channel and version. Only a
// rolling edge-<hash> build is signed by a push to master; every tagged
// release, including alpha, beta and rc, is signed by its v* tag.
func Identity(channel, version string) (string, error) {
	if err := matchVersion(channel, version); err != nil {
		return "", err
	}

	if edgeVersionPattern.MatchString(version) {
		return "https://github.com/mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master", nil
	}

	return "https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v" + version, nil
}

func matchVersion(channel, version string) error {
	logical, ok := VersionChannel(version)
	if !ok {
		return fmt.Errorf("release version %q is invalid", version)
	}

	switch channel {
	case ChannelStable, ChannelEdge, ChannelRC:
		if logical != channel {
			return fmt.Errorf("release version %q is not on the %s channel", version, channel)
		}

		return nil
	}

	major, ok := lineMajor(channel)
	if !ok {
		return fmt.Errorf("release channel %q is invalid", channel)
	}

	v, ok := ParseStable(version)
	if !ok || logical != ChannelStable {
		return fmt.Errorf("release version %q is not on the %s channel", version, channel)
	}

	if v.Major != major {
		return fmt.Errorf("release version %q is not on the %s channel", version, channel)
	}

	return nil
}

func versionRoot(version string) string {
	return strings.TrimRight(currentBaseURL(), "/") + "/releases/" + version
}
