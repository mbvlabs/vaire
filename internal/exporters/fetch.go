package exporters

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// maxTarball bounds a download; the exporters are 5-15 MiB.
const maxTarball = 128 << 20

// fetchTimeout bounds one download.
const fetchTimeout = 5 * time.Minute

// Installed reports whether a's binary is already in place.
func Installed(a Artifact) bool {
	info, err := os.Stat(a.BinaryPath())
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// Fetch downloads a's tarball, refuses it unless its sha256 is the pinned
// one, and extracts the one binary to a.BinaryPath() (root 0755). A binary
// already in place is not downloaded again.
func Fetch(ctx context.Context, a Artifact) error {
	if Installed(a) {
		return nil
	}
	if err := os.MkdirAll(a.Dir(), 0o755); err != nil { //nolint:gosec // G301: system binaries are 0755.
		return err
	}
	archive, err := os.CreateTemp(a.Dir(), ".download-*")
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	if err := download(ctx, a.URL, archive); err != nil {
		return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Check that the server reaches github.com, then run vaire apply again.").WithDetails(map[string]any{"url": a.URL})
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := Verify(archive, a.SHA256); err != nil {
		return output.WrapError(output.CodeReleaseVerification, err, output.ExitReleaseVerification,
			"The download does not match the sha256 pinned in vaire; do not install it.").WithDetails(map[string]any{"url": a.URL, "exporter": a.Name})
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return Extract(archive, a.Member, a.BinaryPath())
}

// download copies url's body into w, capped at maxTarball.
func download(ctx context.Context, url string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{CheckRedirect: httpsOnly}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxTarball+1))
	if err != nil {
		return err
	}
	if n > maxTarball {
		return errors.New("the exporter archive is larger than expected")
	}
	return nil
}

// httpsOnly refuses a redirect off https. GitHub sends release downloads to
// its object storage on another host, so the host may change.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL == nil || req.URL.Scheme != "https" {
		return errors.New("refusing a non-https redirect")
	}
	return nil
}

// Verify reads r to the end and compares its sha256 with want (hex).
func Verify(r io.Reader, want string) error {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// Extract reads a gzipped tar from r and writes the regular file member to
// dest, atomically, mode 0755. Nothing else in the archive is written, so a
// path that escapes the target can't land anywhere.
func Extract(r io.Reader, member, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive has no %s", member)
		}
		if err != nil {
			return err
		}
		if path.Clean(hdr.Name) != member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s in the archive is not a regular file", member)
		}
		return writeBinary(tr, dest)
	}
}

func writeBinary(r io.Reader, dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".bin-*")
	if err != nil {
		return err
	}
	n, err := io.Copy(tmp, io.LimitReader(r, maxTarball+1))
	if err == nil && n > maxTarball {
		err = errors.New("the exporter binary is larger than expected")
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755) //nolint:gosec // G302: an executable.
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dest)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// RemoveOld removes every version directory of exporter name except keep's,
// once its unit points at the new one.
func RemoveOld(name, keepDir string) error { return removeOldIn(Root, name, keepDir) }

func removeOldIn(root, name, keepDir string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if !e.IsDir() || dir == keepDir || !strings.HasPrefix(e.Name(), name+"-") {
			continue
		}
		// "pgbouncer_exporter-" must not match "pgbouncer_exporter-session": a
		// version starts with a digit.
		rest := strings.TrimPrefix(e.Name(), name+"-")
		if rest == "" || rest[0] < '0' || rest[0] > '9' {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}
