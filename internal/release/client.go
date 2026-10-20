package release

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// Client resolves signed manifests and fetches verified artifacts.
type Client struct {
	HTTP     *http.Client
	Verifier Verifier
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = newHTTPClient()
	}
	return c.HTTP
}

func (c *Client) verifier() Verifier {
	if c.Verifier == nil {
		c.Verifier = newDefaultVerifier()
	}
	return c.Verifier
}

// Resolve fetches and verifies the channel manifest (version == "") or a
// version manifest. channel is stable, edge or a major line such as
// stable-1.x; a line's manifest is signed by the stable workflow.
func (c *Client) Resolve(ctx context.Context, channel, version string) (Manifest, error) {
	var manifestURL string
	var wantVersion string
	switch version {
	case "":
		root, err := ChannelRoot(channel)
		if err != nil {
			return Manifest{}, invalidChannel(channel)
		}
		manifestURL = root + "/manifest.json"
	default:
		if err := matchVersion(channel, version); err != nil {
			return Manifest{}, output.NewError(
				output.CodeValidation,
				err.Error(),
				output.ExitConfig,
				"Pass a stable MAJOR.MINOR.PATCH (optionally -alpha.N, -beta.N or -rc.N) on the channel's major line, or an edge-<12-hex> version.",
			).WithDetails(map[string]any{"version": version})
		}
		wantVersion = version
		manifestURL = versionRoot(version) + "/manifest.json"
	}
	bundleURL := manifestURL + ".sigstore.json"
	raw, status, err := getCapped(ctx, c.httpClient(), manifestURL, maxMetadata)
	if err != nil {
		return Manifest{}, err
	}
	if status == http.StatusNotFound && version != "" {
		return Manifest{}, invalidVersion(version)
	}
	if status != http.StatusOK {
		return Manifest{}, unreachable(manifestURL, fmt.Errorf("status %d", status))
	}
	bundleBytes, status, err := getCapped(ctx, c.httpClient(), bundleURL, maxMetadata)
	if err != nil {
		return Manifest{}, err
	}
	if status != http.StatusOK {
		return Manifest{}, unreachable(bundleURL, fmt.Errorf("status %d", status))
	}

	identityVersion := version
	if identityVersion == "" {
		identityVersion = peekVersion(raw)
	}
	identity, err := Identity(channel, identityVersion)
	if err != nil {
		return Manifest{}, verificationFailed("release manifest version is invalid")
	}
	if err := c.verifier().VerifyBlob(ctx, raw, bundleBytes, identity); err != nil {
		return Manifest{}, err
	}
	m, err := decodeManifest(raw)
	if err != nil {
		return Manifest{}, err
	}
	m.Bundle = append([]byte(nil), bundleBytes...)
	if err := Validate(m, channel, wantVersion); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Fetch returns a verified local path for os/arch, using the release cache.
func (c *Client) Fetch(ctx context.Context, m Manifest, goos, goarch string) (string, error) {
	key := goos + "/" + goarch
	art, ok := m.Artifacts[key]
	if !ok {
		return "", output.NewError(
			output.CodeValidation,
			"no release artifact for "+key,
			output.ExitConfig,
			"",
		)
	}
	dir := releaseCacheDir(m.Version, goos, goarch)
	dest := filepath.Join(dir, "vaire")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create release cache: %w", err)
	}
	if hashOK, err := fileHasSHA256(dest, art.SHA256); err != nil {
		_ = os.Remove(dest)
	} else if hashOK {
		if err := persistManifest(dir, m); err != nil {
			return "", err
		}
		return dest, nil
	} else {
		_ = os.Remove(dest)
	}

	tmp, err := os.CreateTemp(dir, ".vaire-download-*")
	if err != nil {
		return "", fmt.Errorf("create release download: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o700); err != nil {
		return "", fmt.Errorf("protect release download: %w", err)
	}

	resp, err := openHTTPS(ctx, c.httpClient(), art.URL)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", unreachable(art.URL, fmt.Errorf("status %d", resp.StatusCode))
	}

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, maxArtifact+1))
	if err != nil {
		return "", unreachable(art.URL, err)
	}
	if written > maxArtifact {
		return "", verificationFailed("release artifact exceeds the size limit")
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close release download: %w", err)
	}
	want, err := hex.DecodeString(art.SHA256)
	if err != nil || len(want) != sha256.Size {
		return "", verificationFailed("release artifact sha256 is invalid")
	}
	got := hash.Sum(nil)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return "", verificationFailed("release artifact sha256 mismatch")
	}
	if err := persistManifest(dir, m); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("install cached release: %w", err)
	}
	cleanup = false
	return dest, nil
}

func persistManifest(dir string, m Manifest) error {
	if len(m.Raw) == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), m.Raw, 0o600); err != nil {
		return fmt.Errorf("write cached manifest: %w", err)
	}
	if len(m.Bundle) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json.sigstore.json"), m.Bundle, 0o600); err != nil {
			return fmt.Errorf("write cached manifest bundle: %w", err)
		}
	}
	return nil
}

func fileHasSHA256(path, wantHex string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("cached release is not a regular file")
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is under the local release cache.
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return false, err
	}
	want, err := hex.DecodeString(wantHex)
	if err != nil || len(want) != sha256.Size {
		return false, nil
	}
	return subtle.ConstantTimeCompare(hash.Sum(nil), want) == 1, nil
}
