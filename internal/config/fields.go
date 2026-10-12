package config

import (
	"errors"
	"net/netip"
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

// CheckDataDirectory checks server.data_directory.
func CheckDataDirectory(v string) error {
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
	for _, suffix := range []string{"_owner", "_migrate", "_app", "_ro"} {
		if len(v)+len(suffix) > 63 {
			return errors.New("is too long: " + v + suffix + " would exceed 63 characters")
		}
	}
	return nil
}
