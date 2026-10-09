//go:build e2e

package harness

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

const (
	releaseHost      = "release.eru.test"
	releaseHostPort  = "18443"
	releaseCaddyPort = "8443"
	pebbleMgmtURL    = "https://127.0.0.1:" + pebbleMgmtPort
	pebbleDirURL     = "https://127.0.0.1:" + pebblePort + "/dir"
)

var (
	releaseMu   sync.Mutex
	releaseDirs = map[string]string{}
)

// ReleaseServer starts a Caddy container serving a temp dir over HTTPS with a
// Pebble-issued certificate. It returns the public base URL the VM should use.
func ReleaseServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required to boot the release server")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "health"), []byte("ok\n"), 0o644); err != nil { //nolint:gosec // G306: published over the local HTTPS test origin.
		t.Fatal(err)
	}
	certPEM, keyPEM := pebbleCertificate(t, releaseHost)
	cfgDir := t.TempDir()
	certPath := filepath.Join(cfgDir, "tls.crt")
	keyPath := filepath.Join(cfgDir, "tls.key")
	caddyfile := filepath.Join(cfgDir, "Caddyfile")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil { //nolint:gosec // G306: leaf cert is mounted read-only into Caddy.
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	caddyBody := fmt.Sprintf(`{
	admin off
	auto_https off
}
https://%s:%s {
	tls /etc/caddy/tls.crt /etc/caddy/tls.key
	root * /srv
	file_server
}
`, releaseHost, releaseCaddyPort)
	if err := os.WriteFile(caddyfile, []byte(caddyBody), 0o644); err != nil { //nolint:gosec // G306: Caddyfile is mounted read-only.
		t.Fatal(err)
	}
	_ = docker("rm", "-f", releaseCaddy)
	if err := docker(
		"run", "-d", "--name", releaseCaddy, "--network", networkName,
		"-p", releaseHostPort+":"+releaseCaddyPort,
		"-v", dir+":/srv",
		"-v", caddyfile+":/etc/caddy/Caddyfile:ro",
		"-v", certPath+":/etc/caddy/tls.crt:ro",
		"-v", keyPath+":/etc/caddy/tls.key:ro",
		"caddy:2",
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker("rm", "-f", releaseCaddy) })
	url := "https://" + releaseHost + ":" + releaseHostPort
	waitReleaseTLS(t, url)
	releaseMu.Lock()
	releaseDirs[url] = dir
	releaseMu.Unlock()
	return url
}

// ReleaseDir is the host directory served at url.
func ReleaseDir(url string) string {
	releaseMu.Lock()
	defer releaseMu.Unlock()
	return releaseDirs[url]
}

// BuildRelease builds, key-signs, and publishes version into the ReleaseServer tree.
func BuildRelease(t *testing.T, version, url, keyDir string) {
	t.Helper()
	dir := ReleaseDir(url)
	if dir == "" {
		t.Fatalf("ReleaseServer was not started for %s", url)
	}
	ensureReleaseKey(t, keyDir)
	if err := copyFile(filepath.Join(keyDir, "cosign.pub"), filepath.Join(dir, "cosign.pub")); err != nil {
		t.Fatal(err)
	}
	root := moduleRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "build-release.sh"), version, "edge", url) //nolint:gosec // G204: e2e invokes the repo's build-release.sh.
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "ERU_RELEASE_TEST_KEY=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build-release %s: %v\n%s", version, err, out)
	}
	built := filepath.Join(root, "dist", "release-edge")
	signReleaseTree(t, built, keyDir)
	if err := copyTree(filepath.Join(built, "releases", version), filepath.Join(dir, "releases", version)); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(filepath.Join(built, "edge"), filepath.Join(dir, "edge")); err != nil {
		t.Fatal(err)
	}
}

// PebbleRootPEM returns the Pebble ACME root the VM must trust.
func PebbleRootPEM(t *testing.T) []byte {
	t.Helper()
	pem, err := pebbleGET(pebbleMgmtURL + "/roots/0")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pem, []byte("BEGIN CERTIFICATE")) {
		t.Fatalf("pebble root is not a certificate: %s", pem)
	}
	return pem
}

func ensureReleaseKey(t *testing.T, keyDir string) {
	t.Helper()
	pub := filepath.Join(keyDir, "cosign.pub")
	key := filepath.Join(keyDir, "cosign.key")
	if _, err := os.Stat(pub); err == nil {
		if _, err := os.Stat(key); err == nil {
			return
		}
	}
	cmd := exec.Command("cosign", "generate-key-pair") //nolint:gosec // G204: fixed cosign invocation.
	cmd.Dir = keyDir
	cmd.Env = append(os.Environ(), "COSIGN_PASSWORD=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cosign generate-key-pair: %v\n%s", err, out)
	}
}

func signReleaseTree(t *testing.T, root, keyDir string) {
	t.Helper()
	key := filepath.Join(keyDir, "cosign.key")
	pub := filepath.Join(keyDir, "cosign.pub")
	matches, err := filepath.Glob(filepath.Join(root, "releases", "*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one version directory under %s/releases, got %v: %v", root, matches, err)
	}
	versionRoot := matches[0]
	var channelManifest string
	switch {
	case fileExists(filepath.Join(root, "manifest.json")):
		channelManifest = filepath.Join(root, "manifest.json")
	case fileExists(filepath.Join(root, "edge", "manifest.json")):
		channelManifest = filepath.Join(root, "edge", "manifest.json")
	default:
		t.Fatalf("channel manifest is missing under %s", root)
	}
	files := []string{channelManifest, filepath.Join(versionRoot, "manifest.json")}
	for _, osName := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			files = append(files, filepath.Join(versionRoot, osName, arch, "eru"))
		}
	}
	for _, file := range files {
		signBlob(t, key, pub, file)
	}
}

func signBlob(t *testing.T, key, pub, file string) {
	t.Helper()
	bundle := file + ".sigstore.json"
	sign := exec.Command( //nolint:gosec // G204: e2e signs the local release tree.
		"cosign", "sign-blob", "--yes",
		// Sign offline: no signing config and no public Rekor upload.
		"--use-signing-config=false", "--tlog-upload=false",
		"--key", key, "--bundle", bundle, file,
	)
	sign.Env = append(os.Environ(), "COSIGN_PASSWORD=")
	if out, err := sign.CombinedOutput(); err != nil {
		t.Fatalf("cosign sign-blob %s: %v\n%s", file, err, out)
	}
	verify := exec.Command( //nolint:gosec // G204: e2e verifies the local release tree.
		"cosign", "verify-blob", "--insecure-ignore-tlog=true",
		"--key", pub, "--bundle", bundle, file,
	)
	verify.Env = append(os.Environ(), "COSIGN_PASSWORD=")
	if out, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("cosign verify-blob %s: %v\n%s", file, err, out)
	}
}

func pebbleCertificate(t *testing.T, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &acme.Client{
		Key:          accountKey,
		DirectoryURL: pebbleDirURL,
		HTTPClient:   pebbleHTTP(),
	}
	if _, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil {
		t.Fatalf("pebble register: %v", err)
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(name))
	if err != nil {
		t.Fatalf("pebble authorize: %v", err)
	}
	// Only the new-order response carries a Location header; orders fetched
	// later come back with an empty URI, so keep the URL from here.
	orderURL := order.URI
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			t.Fatalf("pebble authorization: %v", err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}
		if len(authz.Challenges) == 0 {
			t.Fatalf("pebble authorization %s has no challenges", authzURL)
		}
		chal := authz.Challenges[0]
		for _, candidate := range authz.Challenges {
			if candidate.Type == "http-01" {
				chal = candidate
				break
			}
		}
		if _, err := client.Accept(ctx, chal); err != nil {
			t.Fatalf("pebble accept %s: %v", chal.Type, err)
		}
		if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
			t.Fatalf("pebble wait authorization: %v", err)
		}
	}
	order, err = client.WaitOrder(ctx, orderURL)
	if err != nil {
		t.Fatalf("pebble wait order: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: name},
		DNSNames: []string{name},
	}, leafKey)
	if err != nil {
		t.Fatal(err)
	}
	ders, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		// Pebble finalizes asynchronously and its "processing" finalize response
		// has no Location header, so x/crypto/acme polls an empty URL. Poll the
		// order URL from AuthorizeOrder instead.
		done, waitErr := client.WaitOrder(ctx, orderURL)
		if waitErr != nil {
			t.Fatalf("pebble create cert: %v (wait order: %v)", err, waitErr)
		}
		ders, err = client.FetchCert(ctx, done.CertURL, true)
		if err != nil {
			t.Fatalf("pebble fetch cert: %v", err)
		}
	}
	var certBuf bytes.Buffer
	for _, der := range ders {
		if err := pem.Encode(&certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			t.Fatal(err)
		}
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBuf := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certBuf.Bytes(), keyBuf
}

func waitReleaseTLS(t *testing.T, url string) {
	t.Helper()
	rootPEM := PebbleRootPEM(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		t.Fatal("pebble root did not parse")
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 2 * time.Second},
		Config: &tls.Config{
			ServerName: releaseHost,
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
	}
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		conn, err := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", releaseHostPort))
		if err == nil {
			_ = conn.Close()
			return
		}
		last = err
		time.Sleep(time.Second)
	}
	logs, _ := dockerOutput("logs", releaseCaddy)
	t.Fatalf("release server at %s did not become ready: %v\n%s", url, last, logs)
}

func pebbleHTTP() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: Pebble's directory uses its minica test CA.
		},
	}
}

func pebbleGET(url string) ([]byte, error) {
	resp, err := pebbleHTTP().Get(url) //nolint:gosec // G107: Pebble management URL on localhost.
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, body)
	}
	return body, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // G301: release tree is served over HTTPS.
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // G301: release tree is served over HTTPS.
		return err
	}
	in, err := os.Open(src) //nolint:gosec // G304: e2e copies files under the release temp dir.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()) //nolint:gosec // G304: e2e copies files under the release temp dir.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
