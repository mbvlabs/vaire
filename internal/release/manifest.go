package release

import (
	"bytes"
	"encoding/json"
	"regexp"

	"github.com/mbvlabs/eru/internal/cli/output"
)

const schemaVersion = 1

var artifactKeys = []string{
	"linux/amd64",
	"linux/arm64",
	"darwin/amd64",
	"darwin/arm64",
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Artifact is one signed binary listed in a release manifest.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Manifest is a signed channel or version manifest.
type Manifest struct {
	SchemaVersion  int                 `json:"schemaVersion"`
	Channel        string              `json:"channel"`
	Version        string              `json:"version"`
	ReleaseBaseURL string              `json:"releaseBaseUrl"`
	Artifacts      map[string]Artifact `json:"artifacts"`
	Raw            []byte              `json:"-"`
	Bundle         []byte              `json:"-"`
}

type peekManifest struct {
	Version string `json:"version"`
}

// Validate checks a verified manifest against the channel policy.
func Validate(m Manifest, channel, wantVersion string) error {
	if m.SchemaVersion != schemaVersion {
		return verificationFailed("release manifest schemaVersion is invalid")
	}
	if m.Channel != channel {
		return verificationFailed("release manifest channel is invalid")
	}
	if err := matchVersion(m.Channel, m.Version); err != nil {
		return verificationFailed("release manifest version is invalid")
	}
	if wantVersion != "" && m.Version != wantVersion {
		return verificationFailed("release manifest version is invalid")
	}
	wantBase := versionRoot(m.Version)
	if m.ReleaseBaseURL != wantBase {
		return verificationFailed("release manifest releaseBaseUrl is invalid")
	}
	if len(m.Artifacts) != len(artifactKeys) {
		return verificationFailed("release manifest artifacts are incomplete")
	}
	for _, key := range artifactKeys {
		art, ok := m.Artifacts[key]
		if !ok {
			return verificationFailed("release manifest is missing " + key)
		}
		if art.URL != wantBase+"/"+key+"/eru" {
			return verificationFailed("release manifest artifact URL is invalid")
		}
		if !sha256Pattern.MatchString(art.SHA256) {
			return verificationFailed("release manifest artifact sha256 is invalid")
		}
	}
	return nil
}

func decodeManifest(raw []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, verificationFailed("release manifest is malformed")
	}
	if dec.More() {
		return Manifest{}, verificationFailed("release manifest is malformed")
	}
	m.Raw = append([]byte(nil), raw...)
	return m, nil
}

func peekVersion(raw []byte) string {
	var peek peekManifest
	if err := json.Unmarshal(raw, &peek); err != nil {
		return ""
	}
	return peek.Version
}

func invalidVersion(version string) error {
	return output.NewError(
		output.CodeValidation,
		"unknown release version",
		output.ExitConfig,
		"Pass a version that exists on the channel.",
	).WithDetails(map[string]any{"version": version})
}
