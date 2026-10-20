package release

import (
	"strings"
	"testing"
)

func TestParseStable(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want StableVersion
	}{
		{"1.0.0", true, StableVersion{Major: 1}},
		{"0.1.0", true, StableVersion{Minor: 1}},
		{"10.20.30", true, StableVersion{Major: 10, Minor: 20, Patch: 30}},
		{"1.0.0-alpha.1", true, StableVersion{Major: 1, Pre: "alpha", PreN: 1}},
		{"1.0.0-alpha.0", true, StableVersion{Major: 1, Pre: "alpha"}},
		{"1.2.3-beta.10", true, StableVersion{Major: 1, Minor: 2, Patch: 3, Pre: "beta", PreN: 10}},
		{"2.0.0-rc.3", true, StableVersion{Major: 2, Pre: "rc", PreN: 3}},
		{"", false, StableVersion{}},
		{"1.0", false, StableVersion{}},
		{"1.0.0.0", false, StableVersion{}},
		{"v1.0.0", false, StableVersion{}},
		{"01.0.0", false, StableVersion{}},
		{"1.00.0", false, StableVersion{}},
		{"1.0.0-alpha", false, StableVersion{}},
		{"1.0.0-alpha.01", false, StableVersion{}},
		{"1.0.0-alpha1", false, StableVersion{}},
		{"1.0.0-dev.1", false, StableVersion{}},
		{"1.0.0-ALPHA.1", false, StableVersion{}},
		{"1.0.0-alpha.1.2", false, StableVersion{}},
		{"1.0.0+build.1", false, StableVersion{}},
		{"1.0.0-alpha.1\n", false, StableVersion{}},
		{"99999999999999999999.0.0", false, StableVersion{}},
		{"edge-0123456789ab", false, StableVersion{}},
	}
	for _, c := range cases {
		got, ok := ParseStable(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseStable(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCompareVersionsPrecedence(t *testing.T) {
	// Ascending semver precedence.
	order := []string{
		"0.9.9",
		"1.0.0-alpha.1",
		"1.0.0-alpha.2",
		"1.0.0-alpha.10",
		"1.0.0-beta.1",
		"1.0.0-beta.2",
		"1.0.0-rc.1",
		"1.0.0-rc.11",
		"1.0.0",
		"1.0.1-alpha.1",
		"1.0.1",
		"1.0.10",
		"1.1.0",
		"1.10.0",
		"2.0.0-alpha.1",
		"2.0.0",
		"10.0.0",
	}
	for i, a := range order {
		for j, b := range order {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := CompareVersions(a, b); got != want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
}

func TestCompareVersionsFallsBackForUnparsed(t *testing.T) {
	if got := CompareVersions("edge-aaaaaaaaaaaa", "edge-bbbbbbbbbbbb"); got != -1 {
		t.Fatalf("got %d, want -1", got)
	}
}

func TestPromotes(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		candidate string
		want      bool
		err       bool
	}{
		{"empty channel takes a release", "", "1.0.0", true, false},
		{"empty channel takes an alpha", "", "1.0.0-alpha.1", true, false},
		{"alpha follows alpha", "1.0.0-alpha.1", "1.0.0-alpha.2", true, false},
		{"alpha.10 after alpha.9", "1.0.0-alpha.9", "1.0.0-alpha.10", true, false},
		{"beta after alpha", "1.0.0-alpha.5", "1.0.0-beta.1", true, false},
		{"final after rc", "1.0.0-rc.2", "1.0.0", true, false},
		{"next major alpha before any final", "1.0.0-alpha.3", "2.0.0-alpha.1", true, false},
		{"patch after final", "1.0.0", "1.0.1", true, false},
		{"minor after final", "1.0.1", "1.1.0", true, false},
		{"next major", "1.4.2", "2.0.0", true, false},
		{"alpha never moves a final", "1.0.0", "1.1.0-alpha.1", false, false},
		{"next major alpha never moves a final", "1.4.2", "2.0.0-rc.1", false, false},
		{"old line patch stays off the newer line", "2.0.0", "1.4.3", false, false},
		{"older alpha", "1.0.0-alpha.3", "1.0.0-alpha.2", false, false},
		{"same version", "1.0.0", "1.0.0", false, false},
		{"invalid candidate", "1.0.0", "1.0", false, true},
		{"invalid current", "garbage", "1.0.0", false, true},
		{"null current from jq", "null", "1.0.0", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Promotes(c.current, c.candidate)
			if (err != nil) != c.err {
				t.Fatalf("Promotes(%q, %q) error = %v, want error %v", c.current, c.candidate, err, c.err)
			}
			if got != c.want {
				t.Fatalf("Promotes(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
			}
		})
	}
}

func TestLineChannel(t *testing.T) {
	v, ok := ParseStable("1.0.0-alpha.1")
	if !ok {
		t.Fatal("parse failed")
	}
	if v.Line() != "1.x" || v.Channel() != "stable-1.x" {
		t.Fatalf("line %q channel %q", v.Line(), v.Channel())
	}
	for _, c := range []struct {
		in    string
		major int
		ok    bool
	}{
		{"stable-1.x", 1, true},
		{"stable-0.x", 0, true},
		{"stable-12.x", 12, true},
		{"stable", 0, false},
		{"stable-1", 0, false},
		{"stable-01.x", 0, false},
		{"stable-1.0", 0, false},
		{"stable-x", 0, false},
		{"edge", 0, false},
	} {
		major, ok := lineMajor(c.in)
		if major != c.major || ok != c.ok {
			t.Errorf("lineMajor(%q) = %d, %v; want %d, %v", c.in, major, ok, c.major, c.ok)
		}
	}
}

func TestChannelRoot(t *testing.T) {
	for _, c := range []struct {
		channel string
		want    string
	}{
		{"stable", "https://vaire.mbvlabs.com"},
		{"edge", "https://vaire.mbvlabs.com/edge"},
		{"stable-1.x", "https://vaire.mbvlabs.com/1.x"},
		{"stable-2.x", "https://vaire.mbvlabs.com/2.x"},
	} {
		got, err := ChannelRoot(c.channel)
		if err != nil || got != c.want {
			t.Errorf("ChannelRoot(%q) = %q, %v; want %q", c.channel, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "beta", "stable-1", "1.x", "stable-1.x/"} {
		if _, err := ChannelRoot(bad); err == nil {
			t.Errorf("ChannelRoot(%q) accepted an invalid channel", bad)
		}
	}
}

func TestIdentity(t *testing.T) {
	const stablePrefix = "https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v"
	const edgeIdentity = "https://github.com/mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master"
	cases := []struct {
		channel string
		version string
		want    string
	}{
		{"stable", "1.0.0", stablePrefix + "1.0.0"},
		{"stable", "1.0.0-alpha.1", stablePrefix + "1.0.0-alpha.1"},
		{"stable", "2.3.4-rc.2", stablePrefix + "2.3.4-rc.2"},
		{"stable-1.x", "1.0.0-alpha.2", stablePrefix + "1.0.0-alpha.2"},
		{"stable-1.x", "1.4.1", stablePrefix + "1.4.1"},
		{"stable-2.x", "2.0.0", stablePrefix + "2.0.0"},
		{"edge", "edge-0123456789ab", edgeIdentity},
	}
	for _, c := range cases {
		got, err := Identity(c.channel, c.version)
		if err != nil || got != c.want {
			t.Errorf("Identity(%q, %q) = %q, %v; want %q", c.channel, c.version, got, err, c.want)
		}
	}
	bad := []struct{ channel, version string }{
		{"stable", "1.0"},
		{"stable", "1.0.0-alpha"},
		{"stable", "edge-0123456789ab"},
		{"stable-1.x", "2.0.0"},
		{"stable-2.x", "1.9.9"},
		{"stable-1.x", "edge-0123456789ab"},
		{"edge", "1.0.0"},
		{"beta", "1.0.0"},
	}
	for _, c := range bad {
		if got, err := Identity(c.channel, c.version); err == nil {
			t.Errorf("Identity(%q, %q) = %q, want an error", c.channel, c.version, got)
		}
	}
}

func testManifest(channel, version string) Manifest {
	base := versionRoot(version)
	m := Manifest{
		SchemaVersion:  schemaVersion,
		Channel:        channel,
		Version:        version,
		ReleaseBaseURL: base,
		Artifacts:      map[string]Artifact{},
	}
	for _, key := range artifactKeys {
		m.Artifacts[key] = Artifact{URL: base + "/" + key + "/vaire", SHA256: strings.Repeat("a", 64)}
	}
	return m
}

func TestValidateLineChannel(t *testing.T) {
	cases := []struct {
		name    string
		m       Manifest
		channel string
		want    string
		ok      bool
	}{
		{"root takes an alpha", testManifest("stable", "1.0.0-alpha.1"), "stable", "", true},
		{"line takes its alpha", testManifest("stable", "1.0.0-alpha.1"), "stable-1.x", "", true},
		{"line takes its release", testManifest("stable", "1.2.3"), "stable-1.x", "1.2.3", true},
		{"line refuses another major", testManifest("stable", "2.0.0"), "stable-1.x", "", false},
		{"line manifest must say stable", testManifest("stable-1.x", "1.0.0"), "stable-1.x", "", false},
		{"stable refuses an edge manifest", testManifest("edge", "edge-0123456789ab"), "stable", "", false},
		{"pinned version mismatch", testManifest("stable", "1.0.0"), "stable-1.x", "1.0.1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.m, c.channel, c.want)
			if (err == nil) != c.ok {
				t.Fatalf("Validate = %v, want ok %v", err, c.ok)
			}
		})
	}
}
