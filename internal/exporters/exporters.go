// Package exporters fetches the pinned Prometheus exporters (Phase 4): the
// upstream release tarball for the node's architecture, checked against the
// sha256 pinned in internal/versions before anything is extracted.
package exporters

import (
	"fmt"
	"path"
	"path/filepath"

	"github.com/mbvlabs/eru/internal/versions"
)

// Root is where eru keeps the exporter binaries, one directory per version:
// /usr/local/lib/eru/exporters/<name>-<version>/<name>.
const Root = "/usr/local/lib/eru/exporters"

// Artifact is one exporter's release tarball for one architecture.
type Artifact struct {
	Name    string
	Version string
	Arch    string
	// URL is the GitHub release download.
	URL string
	// Tarball is the archive's file name.
	Tarball string
	// Member is the binary's path inside the archive.
	Member string
	// SHA256 is the pinned digest of the tarball.
	SHA256 string
}

// repos are the GitHub repositories of the pinned exporters.
var repos = map[string]string{
	versions.NodeExporter:      "prometheus/node_exporter",
	versions.PostgresExporter:  "prometheus-community/postgres_exporter",
	versions.PgbouncerExporter: "prometheus-community/pgbouncer_exporter",
}

// ArtifactFor returns the pinned artifact of exporter name for arch (amd64 or
// arm64). The Prometheus projects all lay their tarballs out the same way:
// <name>-<version>.linux-<arch>.tar.gz holding <name>-<version>.linux-<arch>/<name>.
func ArtifactFor(name, arch string) (Artifact, error) {
	version := versions.ExporterVersion(name)
	repo, ok := repos[name]
	if version == "" || !ok {
		return Artifact{}, fmt.Errorf("unknown exporter %q", name)
	}
	sum, ok := versions.ExporterSHA256[name+"-"+arch]
	if !ok {
		return Artifact{}, fmt.Errorf("exporter %s has no pinned build for %s", name, arch)
	}
	stem := fmt.Sprintf("%s-%s.linux-%s", name, version, arch)
	return Artifact{
		Name:    name,
		Version: version,
		Arch:    arch,
		URL:     fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s.tar.gz", repo, version, stem),
		Tarball: stem + ".tar.gz",
		Member:  path.Join(stem, name),
		SHA256:  sum,
	}, nil
}

// Dir is the directory that holds this artifact's binary.
func (a Artifact) Dir() string { return filepath.Join(Root, a.Name+"-"+a.Version) }

// BinaryPath is where the binary is installed.
func (a Artifact) BinaryPath() string { return filepath.Join(a.Dir(), a.Name) }

// BinaryPath is the installed path of exporter name at its pinned version.
func BinaryPath(name string) string {
	return filepath.Join(Root, name+"-"+versions.ExporterVersion(name), name)
}
