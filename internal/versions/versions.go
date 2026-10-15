// Package versions pins artifact versions and checksums used by install.sh and lint.
package versions

import "fmt"

const (
	PostgresMajor = 18
	CosignVersion = "3.1.3"
	// NginxMinVersion is the oldest distribution nginx tls-v1 accepts: it
	// needs the stream module with ssl_preread and ssl_alpn (§23.3).
	NginxMinVersion = "1.26"
	// PgBackRestMinVersion is the oldest pgBackRest step 8 installs. 2.59.3
	// fixes an unchecked error when generating random data for repository
	// encryption, and eru always encrypts the repository. pgBackRest comes from
	// PGDG and is held like PostgreSQL.
	PgBackRestMinVersion = "2.59.3"
)

// CosignSHA256 is keyed "<os>-<arch>" for the GitHub release binaries.
var CosignSHA256 = map[string]string{
	"linux-amd64":  "4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71",
	"linux-arm64":  "c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a",
	"darwin-amd64": "2347488e5d5b25336644024dfeca5601b190e91197a71a917bda44744aff106c",
	"darwin-arm64": "5cf948c2f4dfe59687bdd0b8523709067383e03982cc543475c8a7dc70e92a76",
}

// CosignTargets is the stable print and lint order for CosignSHA256.
var CosignTargets = []string{
	"linux-amd64",
	"linux-arm64",
	"darwin-amd64",
	"darwin-arm64",
}

// Exporter pins (Phase 4). The upstream release binaries are the same on
// Debian 13 and Ubuntu 26.04, unlike the distribution packages, and their
// sha256 lives here, inside the signed eru binary. Re-check them before each
// phase, as for pgBackRest.
const (
	NodeExporterVersion      = "1.12.1"
	PostgresExporterVersion  = "0.20.1"
	PgbouncerExporterVersion = "0.12.1"
)

// Exporter names, as the upstream projects call their binaries.
const (
	NodeExporter      = "node_exporter"
	PostgresExporter  = "postgres_exporter"
	PgbouncerExporter = "pgbouncer_exporter"
)

// Exporters lists the pinned exporters in install order.
var Exporters = []string{NodeExporter, PostgresExporter, PgbouncerExporter}

// ExporterArches are the server architectures eru installs exporters for.
var ExporterArches = []string{"amd64", "arm64"}

// ExporterVersion is the pinned version of an exporter, "" for an unknown
// name.
func ExporterVersion(name string) string {
	switch name {
	case NodeExporter:
		return NodeExporterVersion
	case PostgresExporter:
		return PostgresExporterVersion
	case PgbouncerExporter:
		return PgbouncerExporterVersion
	}
	return ""
}

// ExporterSHA256 is keyed "<name>-<arch>": the sha256 of the release
// tarball, from each release's sha256sums.txt.
var ExporterSHA256 = map[string]string{
	"node_exporter-amd64":      "b51d8a76aa2a9156a55d501aca6276fae09e262259a5e4e831d2c2222f084e63",
	"node_exporter-arm64":      "ad35b605f9954b9f1ffddf5ba054bdc5a98d790b9eae5291e1eeb83f1ecbd0e7",
	"postgres_exporter-amd64":  "89d4f7e7920cad48fdc3133f789556ef5253c330a9f5fdace3bdb6344c0a8b5a",
	"postgres_exporter-arm64":  "d5d86fb98bb1f26b088d1a6fda07fd6b6f035cb5d40492f75ec3bfebb5ddfe9d",
	"pgbouncer_exporter-amd64": "fa391b513d3a3f03843a6e0d64e711fda1f057dc43133db5c1b130ba38767857",
	"pgbouncer_exporter-arm64": "0abb808ae8e251126c854ebdce382993876df629b34c0401816d3818db92c700",
}

// CheckExporterPins reports a pinned exporter without a version or without a
// sha256 for one of ExporterArches. just lint runs it through eru commands
// --check-steps.
func CheckExporterPins() error {
	for _, name := range Exporters {
		if ExporterVersion(name) == "" {
			return fmt.Errorf("exporter %s has no pinned version", name)
		}
		for _, arch := range ExporterArches {
			sum := ExporterSHA256[name+"-"+arch]
			if len(sum) != 64 {
				return fmt.Errorf("exporter %s has no sha256 for %s", name, arch)
			}
		}
	}
	return nil
}
