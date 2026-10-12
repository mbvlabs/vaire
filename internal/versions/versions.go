// Package versions pins artifact versions and checksums used by install.sh and lint.
package versions

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
