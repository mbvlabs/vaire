package cli

import (
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/secretsdoc"
	"github.com/mbvlabs/eru/internal/steps"
	"golang.org/x/crypto/bcrypt"
)

// metricsBcryptCost is the bcrypt cost of the /metrics hash (Phase 4 D5).
// The password has 190 bits of entropy, so the cost protects nothing an
// attacker could brute-force anyway, while nginx checks it on every scrape
// of every path: cost 8 is about 15 ms.
const metricsBcryptCost = 8

// ensureMetricsCreds fills creds with the /metrics credential when it has
// none, and returns whether it generated one. The operator generates it like
// every eru secret (Phase 4 D5).
func ensureMetricsCreds(creds **contexts.MetricsCreds) (bool, error) {
	if *creds != nil && (*creds).Password != "" {
		if (*creds).Username == "" {
			(*creds).Username = contexts.MetricsUser
		}
		secrets.Add((*creds).Password)
		return false, nil
	}
	pw, err := secrets.Base62(32)
	if err != nil {
		return false, err
	}
	*creds = &contexts.MetricsCreds{Username: contexts.MetricsUser, Password: pw}
	secrets.Add(pw)
	return true, nil
}

// metricsHash is the htpasswd line's hash of the credential. The node never
// sees the password: only this hash travels, as the request secret
// metrics.htpasswd, and nginx checks it with crypt(3).
func metricsHash(creds *contexts.MetricsCreds) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(creds.Password), metricsBcryptCost)
	if err != nil {
		return "", err
	}
	secrets.Add(string(h))
	return string(h), nil
}

// addMetricsSecret tops up the credential in creds and puts its hash in
// rpc. It returns whether a new credential was generated.
func addMetricsSecret(creds **contexts.MetricsCreds, rpc map[string]string) (bool, error) {
	generated, err := ensureMetricsCreds(creds)
	if err != nil {
		return false, err
	}
	h, err := metricsHash(*creds)
	if err != nil {
		return false, err
	}
	rpc[steps.SecretMetricsHash] = h
	return generated, nil
}

// metricsDocument is the secrets document's metrics section for creds at
// the node hostname, or nil without a credential.
func metricsDocument(creds *contexts.MetricsCreds, hostname, pathPrefix string) *secretsdoc.Metrics {
	if creds == nil || creds.Password == "" {
		return nil
	}
	return &secretsdoc.Metrics{BaseURL: metricsBaseURL(hostname, pathPrefix), Username: creds.Username, Password: creds.Password}
}

// metricsBaseURL is https://<hostname><path_prefix>.
func metricsBaseURL(hostname, pathPrefix string) string {
	if pathPrefix == "" {
		pathPrefix = "/metrics"
	}
	if hostname == "" {
		return ""
	}
	return "https://" + hostname + pathPrefix
}
