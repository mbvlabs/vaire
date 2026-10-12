package node

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/steps"
	"golang.org/x/crypto/acme"
)

// phase2Jobs are the timer jobs Phase 2 implements. The rest arrive later.
var phase2Jobs = map[string]bool{"certs-renew": true}

// jobNames lists every job eru node run accepts.
var jobNames = []string{
	"backup-full", "backup-diff", "backup-incr", "backup-verify", "restore-drill",
	"vps-backup", "maint-daily", "maint-weekly", "certs-renew", "metrics-refresh",
}

// Certificate lifetimes (D11).
const (
	// renewBefore renews a certificate this long before it expires.
	renewBefore = 30 * 24 * time.Hour
	// selfSignedLifetime is how long a self-signed-mode certificate lasts.
	selfSignedLifetime = 397 * 24 * time.Hour
	// acmeTimeout bounds one issuance.
	acmeTimeout = 5 * time.Minute
)

// RunJob runs one timer job. certs-renew takes the shared lock with a zero
// timeout; a busy lock exits 0 with a skipped result.
func RunJob(ctx context.Context, job string, report engine.Reporter) (any, error) {
	if !knownJob(job) {
		return nil, output.NewError(output.CodeUsage, "unknown job "+job, output.ExitUsage, "Use one of details.jobs.").
			WithDetails(map[string]any{"jobs": jobNames})
	}
	if !phase2Jobs[job] {
		return nil, output.NewError(output.CodeError, "job "+job+" is not implemented yet", output.ExitGeneric,
			"This job arrives in a later phase of the roadmap.")
	}
	release, err := engine.Acquire(ctx, engine.LockShared, job, rpc.NewRequestID(), 0)
	if err != nil {
		logLine(report, job, "the node lock is busy; skipping "+job)
		return map[string]any{"job": job, "skipped": "lock_busy"}, nil
	}
	defer release()
	return certsRenew(ctx, report)
}

func knownJob(job string) bool {
	return slices.Contains(jobNames, job)
}

func logLine(report engine.Reporter, step, line string) {
	if report != nil {
		report(output.Event{Type: "log", Step: step, Line: line})
	}
}

// certsRenew makes sure /etc/eru/tls holds a current certificate for the
// domain: an ACME certificate in acme mode, a self-signed one otherwise. It
// does nothing while the certificate has more than 30 days left, and reloads
// PostgreSQL, PgBouncer and nginx after writing a new one (D11).
func certsRenew(ctx context.Context, report engine.Reporter) (any, error) {
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return nil, err
	}
	if cfg.Domain == "" {
		return map[string]any{"changed": false, "reason": "no domain configured"}, nil
	}
	selfSigned := cfg.TLS.Mode == "self-signed"
	reason := renewReason(readCert(steps.TLSCertPath), cfg.Domain, selfSigned)
	if reason == "" {
		return map[string]any{"changed": false, "not_after": certNotAfter(steps.TLSCertPath)}, nil
	}
	logLine(report, "certs-renew", "renewing the certificate for "+cfg.Domain+": "+reason)

	var chainPEM, keyPEM []byte
	if selfSigned {
		chainPEM, keyPEM, err = selfSignedCert(cfg.Domain)
	} else {
		ctx, cancel := context.WithTimeout(ctx, acmeTimeout)
		defer cancel()
		chainPEM, keyPEM, err = issueACME(ctx, cfg, report)
	}
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) && cliErr.Code == output.CodePrecondition {
		// DNS doesn't point here yet: that's the normal state right after
		// init, not a failed unit. The timer tries again in an hour.
		logLine(report, "certs-renew", cliErr.Error())
		return map[string]any{"changed": false, "pending": cliErr.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := installCert(chainPEM, keyPEM); err != nil {
		return nil, err
	}
	logLine(report, "certs-renew", "installed the certificate for "+cfg.Domain)
	_ = runQuiet(ctx, "pg_ctlcluster", "18", steps.PGCluster, "reload")
	_ = runQuiet(ctx, "systemctl", "reload", steps.PgbouncerService)
	_ = runQuiet(ctx, "systemctl", "reload", steps.NginxService)
	return map[string]any{"changed": true, "not_after": certNotAfter(steps.TLSCertPath)}, nil
}

// renewReason says why cert needs replacing, or "" when it is current.
func renewReason(cert *x509.Certificate, domain string, selfSigned bool) string {
	switch {
	case cert == nil:
		return "no certificate"
	case cert.VerifyHostname(domain) != nil:
		return "the certificate is for another name"
	case !selfSigned && isSelfSignedCert(cert):
		return "the placeholder is still served"
	case selfSigned && !isSelfSignedCert(cert):
		return "tls.mode is self-signed"
	case time.Until(cert.NotAfter) < renewBefore:
		return "it expires " + cert.NotAfter.UTC().Format(time.RFC3339)
	}
	return ""
}

// installCert writes the chain (0644) and key (root:ssl-cert 0640). The key is
// chowned before the rename, so PostgreSQL, PgBouncer and nginx never see it
// with the wrong owner.
func installCert(chainPEM, keyPEM []byte) error {
	if err := engine.WriteFileAtomicOwned(steps.TLSKeyPath, keyPEM, 0o640, steps.TLSKeyOwner, steps.TLSKeyGroup); err != nil {
		return err
	}
	return engine.WriteFileAtomic(steps.TLSCertPath, chainPEM, 0o644)
}

// issueACME gets a certificate for the domain over HTTP-01. nginx serves the
// challenge files from the ACME webroot on port 80.
func issueACME(ctx context.Context, cfg config.Cluster, report engine.Reporter) (chainPEM, keyPEM []byte, err error) {
	reached, err := probeHTTP(ctx, cfg.Domain)
	switch {
	case err != nil && reached:
		return nil, nil, output.NewError(output.CodePrecondition, err.Error(), output.ExitPrecondition,
			"Point "+cfg.Domain+" at this server (DNS only, not proxied); eru-certs.timer retries every hour.").
			WithDetails(map[string]any{"reason": "tls_pending"})
	case err != nil:
		// Some networks can't reach their own public address; let the ACME
		// server decide.
		logLine(report, "certs-renew", "self-check inconclusive, trying anyway: "+err.Error())
	}
	accountKey, err := loadOrCreateKey(steps.ACMEAccountKeyPath)
	if err != nil {
		return nil, nil, err
	}
	client := &acme.Client{Key: accountKey, DirectoryURL: acme.LetsEncryptURL, UserAgent: "eru"}
	if cfg.TLS.ACMECA != "" {
		client.DirectoryURL = cfg.TLS.ACMECA
	}
	account := &acme.Account{}
	if cfg.ACMEEmail != "" {
		account.Contact = []string{"mailto:" + cfg.ACMEEmail}
	}
	if _, err := client.Register(ctx, account, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, nil, acmeError("register the ACME account", err)
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(cfg.Domain))
	if err != nil {
		return nil, nil, acmeError("create the order", err)
	}
	for _, url := range order.AuthzURLs {
		if err := authorizeHTTP01(ctx, client, url, report); err != nil {
			return nil, nil, err
		}
	}
	if order, err = client.WaitOrder(ctx, order.URI); err != nil {
		return nil, nil, acmeError("wait for the order", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: cfg.Domain},
		DNSNames: []string{cfg.Domain},
	}, leafKey)
	if err != nil {
		return nil, nil, err
	}
	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, nil, acmeError("finalize the order", err)
	}
	for _, block := range der {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block})...)
	}
	keyPEM, err = encodeKey(leafKey)
	return chainPEM, keyPEM, err
}

// authorizeHTTP01 completes one authorization with the http-01 challenge.
func authorizeHTTP01(ctx context.Context, client *acme.Client, url string, report engine.Reporter) error {
	authz, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return acmeError("read the authorization", err)
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	var chal *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return output.NewError(output.CodeExternalCommand, "the ACME server offered no http-01 challenge", output.ExitExternal, "")
	}
	body, err := client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return err
	}
	path := filepath.Join(steps.ACMEChallengeDir, chal.Token)
	if err := engine.WriteFileAtomic(path, []byte(body), 0o644); err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	logLine(report, "certs-renew", "answering the http-01 challenge for "+authz.Identifier.Value)
	if _, err := client.Accept(ctx, chal); err != nil {
		return acmeError("accept the challenge", err)
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return acmeError("validate "+authz.Identifier.Value, err)
	}
	return nil
}

// probeHTTP checks that http://<domain>/.well-known/acme-challenge/ reaches
// this server's nginx before an order is placed, so a wrong DNS record
// doesn't burn Let's Encrypt's failed-validation limit. reached reports
// whether some HTTP server answered: an error with reached means another
// host answers for the domain.
func probeHTTP(ctx context.Context, domain string) (reached bool, err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return false, err
	}
	token := "eru-probe-" + hex.EncodeToString(raw)
	path := filepath.Join(steps.ACMEChallengeDir, token)
	if err := engine.WriteFileAtomic(path, []byte(token), 0o644); err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(path) }()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+domain+"/.well-known/acme-challenge/"+token, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req) //nolint:gosec // G704: the configured domain, probed for our own token.
	if err != nil {
		return false, fmt.Errorf("%s does not reach this server over HTTP: %w", domain, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(got)) != token {
		return true, fmt.Errorf("%s does not reach this server over HTTP yet (another host answered with HTTP %d)", domain, resp.StatusCode)
	}
	return true, nil
}

func acmeError(what string, err error) error {
	hint := "eru-certs.timer retries every hour. See eru logs --unit eru-certs."
	if d, ok := acme.RateLimit(err); ok {
		hint = "Let's Encrypt rate limit: retry after " + d.Round(time.Minute).String() + "."
	}
	return output.WrapError(output.CodeExternalCommand, fmt.Errorf("ACME: %s: %w", what, err), output.ExitExternal, hint)
}

// selfSignedCert makes a self-signed P-256 certificate for domain.
func selfSignedCert(domain string) (chainPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: domain},
		DNSNames:              []string{domain},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodeKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, err
}

// loadOrCreateKey reads a PEM EC key, or creates one (0600).
func loadOrCreateKey(path string) (crypto.Signer, error) {
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: fixed key path.
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s is not PEM", path)
		}
		return x509.ParseECPrivateKey(block.Bytes)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	raw, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return key, engine.WriteFileAtomic(path, raw, 0o600)
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func runQuiet(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed reload commands.
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	return cmd.Run()
}
