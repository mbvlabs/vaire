package config

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"
)

// The Check functions validate one value each. Validate uses them for the
// whole file and the init interview uses them per field, so both reject the
// same values with the same wording. Each error completes the sentence
// "<key> <value> ...".

// CheckAdminUser checks server.admin_user.
func CheckAdminUser(v string) error {
	if !userPattern.MatchString(v) || v == "root" {
		return errors.New("must be a lower-case user name other than root")
	}
	return nil
}

// CheckTimezone checks timezone.
func CheckTimezone(v string) error {
	if !timezonePattern.MatchString(v) {
		return errors.New("is not a timezone name")
	}
	return nil
}

// CheckDataRoot checks server.data_root.
func CheckDataRoot(v string) error {
	if !strings.HasPrefix(v, "/") {
		return errors.New("must be absolute")
	}
	return nil
}

// CheckDomain checks a non-empty domain.
func CheckDomain(v string) error {
	if !fqdnPattern.MatchString(v) {
		return errors.New("is not a fully qualified domain name")
	}
	return nil
}

// CheckACMEEmail checks a non-empty acme_email.
func CheckACMEEmail(v string) error {
	if !strings.Contains(v, "@") {
		return errors.New("is not an email address")
	}
	return nil
}

// CheckAllowlistEntry checks one network.client_allowlist entry.
func CheckAllowlistEntry(v string) error {
	if _, err := netip.ParsePrefix(v); err != nil {
		return errors.New("is not a CIDR")
	}
	return nil
}

// CheckClientConnections checks postgres.client_connections.
func CheckClientConnections(n int) error {
	if n < 10 || n > 100000 {
		return errors.New("must be 10-100000")
	}
	return nil
}

// CheckDatabaseName checks one databases[].name.
func CheckDatabaseName(v string) error {
	if !DatabasePattern.MatchString(v) {
		return errors.New("must match " + DatabasePattern.String())
	}
	switch v {
	case "postgres", "template0", "template1":
		return errors.New("is reserved")
	}
	if strings.HasSuffix(v, RestoringSuffix) || strings.Contains(v, PreRestoreMarker) {
		return errors.New("is reserved: names ending in " + RestoringSuffix + " or containing " + PreRestoreMarker + " belong to eru restore")
	}
	for _, suffix := range []string{"_owner", "_migrate", "_app", "_ro"} {
		if len(v)+len(suffix) > 63 {
			return errors.New("is too long: " + v + suffix + " would exceed 63 characters")
		}
	}
	return nil
}

// Names eru restore gives the databases beside a live one: <db>__restoring
// holds the restored copy until promote, and <db>__pre_restore_<ts> keeps the
// replaced copy until cleanup.
const (
	RestoringSuffix  = "__restoring"
	PreRestoreMarker = "__pre_restore_"
)

// IsRestoreName reports whether name is one of eru restore's databases.
func IsRestoreName(name string) bool {
	return strings.HasSuffix(name, RestoringSuffix) || strings.Contains(name, PreRestoreMarker)
}

// Providers are the backup provider presets.
var Providers = []string{"r2", "s3", "b2", "hetzner", "minio", "custom"}

// CheckProvider checks backup.provider.
func CheckProvider(v string) error {
	for _, p := range Providers {
		if v == p {
			return nil
		}
	}
	return errors.New("must be one of " + strings.Join(Providers, ", "))
}

// CheckEndpoint checks backup.endpoint: an https:// URL with a host (and
// port) and nothing after it. pgBackRest talks to S3 over TLS only.
func CheckEndpoint(v string) error {
	rest, ok := strings.CutPrefix(v, "https://")
	rest = strings.TrimSuffix(rest, "/")
	if !ok || rest == "" || strings.ContainsAny(rest, "/?# ") {
		return errors.New("must be an https:// URL with only a host (and port)")
	}
	return nil
}

// CheckBucket checks backup.bucket.
func CheckBucket(v string) error {
	if !bucketPattern.MatchString(v) {
		return errors.New("is not an S3 bucket name")
	}
	return nil
}

// CheckPrefix checks backup.prefix: no leading or trailing slash.
func CheckPrefix(v string) error {
	if v == "" || !prefixPattern.MatchString(v) || strings.HasPrefix(v, "/") || strings.HasSuffix(v, "/") || strings.Contains(v, "//") {
		return errors.New("must be a non-empty key prefix without leading or trailing slash")
	}
	return nil
}

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)
