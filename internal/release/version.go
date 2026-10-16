package release

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Stable versions are MAJOR.MINOR.PATCH with an optional -alpha.N, -beta.N
// or -rc.N pre-release. Keep the pattern in step with scripts/build-release.sh
// and publish-stable.yml.
var stableVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$`)

// lineChannelPattern matches a major line's channel, such as stable-1.x.
var lineChannelPattern = regexp.MustCompile(`^stable-(0|[1-9][0-9]*)\.x$`)

// StableVersion is a parsed stable release version.
type StableVersion struct {
	Major int
	Minor int
	Patch int
	// Pre is "", "alpha", "beta" or "rc"; PreN is its number.
	Pre  string
	PreN int
}

// ParseStable parses a stable version such as 1.0.0 or 1.0.0-alpha.2.
func ParseStable(v string) (StableVersion, bool) {
	m := stableVersionPattern.FindStringSubmatch(v)
	if m == nil {
		return StableVersion{}, false
	}
	var out StableVersion
	for i, dst := range []*int{&out.Major, &out.Minor, &out.Patch} {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return StableVersion{}, false
		}
		*dst = n
	}
	if m[4] != "" {
		n, err := strconv.Atoi(m[5])
		if err != nil {
			return StableVersion{}, false
		}
		out.Pre, out.PreN = m[4], n
	}
	return out, true
}

// Prerelease reports whether v is an alpha, beta or release candidate.
func (v StableVersion) Prerelease() bool {
	return v.Pre != ""
}

// Line is the version's major line, such as 1.x.
func (v StableVersion) Line() string {
	return strconv.Itoa(v.Major) + ".x"
}

// Channel is the major line's release channel, such as stable-1.x.
func (v StableVersion) Channel() string {
	return LineChannel(v.Major)
}

// LineChannel names the release channel for a major line.
func LineChannel(major int) string {
	return ChannelStable + "-" + strconv.Itoa(major) + ".x"
}

// lineMajor returns the major version of a stable-<major>.x channel.
func lineMajor(channel string) (int, bool) {
	m := lineChannelPattern.FindStringSubmatch(channel)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// CompareStable orders two stable versions by semver precedence: a
// pre-release sorts before its release, and alpha < beta < rc.
func CompareStable(a, b StableVersion) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if c := cmp.Compare(d[0], d[1]); c != 0 {
			return c
		}
	}
	switch {
	case a.Pre == "" && b.Pre == "":
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	if c := strings.Compare(a.Pre, b.Pre); c != 0 {
		return c
	}
	return cmp.Compare(a.PreN, b.PreN)
}

// CompareVersions orders two stable version strings by semver precedence.
// It falls back to a string comparison when either is not a stable version.
func CompareVersions(a, b string) int {
	pa, okA := ParseStable(a)
	pb, okB := ParseStable(b)
	if !okA || !okB {
		return strings.Compare(a, b)
	}
	return CompareStable(pa, pb)
}

// Promotes reports whether publishing candidate moves a stable channel
// manifest that currently points at current ("" when the channel has none).
// The channel moves only forward, and a pre-release moves it only while the
// channel has never had a final release.
func Promotes(current, candidate string) (bool, error) {
	next, ok := ParseStable(candidate)
	if !ok {
		return false, fmt.Errorf("stable release version %q is invalid", candidate)
	}
	if current == "" {
		return true, nil
	}
	cur, ok := ParseStable(current)
	if !ok {
		return false, fmt.Errorf("current channel version %q is invalid", current)
	}
	if next.Prerelease() && !cur.Prerelease() {
		return false, nil
	}
	return CompareStable(next, cur) > 0, nil
}
