package localenv

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/spf13/cobra"
)

// UsqlVersion is the usql release eru downloads when none is on PATH.
const UsqlVersion = "v0.21.4"

// usqlSHA256 pins the release archive for each platform.
var usqlSHA256 = map[string]string{
	"linux/amd64":  "78bd9b221e223d7a954d41f51e9eca98bdd94b401618367ba0f3887abebd44fc",
	"linux/arm64":  "ccad89d6f4c67a9bf595df0aa8a550e0a9e3d6a6f9356356ba1e164e311335e4",
	"darwin/amd64": "60feb0d73b2e29e4ecf6b36bf34b23c230d8233c778893b0b56811036d16d17e",
	"darwin/arm64": "cee9fd2117c17fd622fe0df37b334c356b80cf48d6e9bb164084830ae4ce6d05",
}

const usqlMaxArchive = 200 << 20

// UsqlPath returns a usql binary: the one on PATH, or eru's pinned copy under
// $XDG_CACHE_HOME/eru/tools. When neither exists it offers to download the
// pinned release (or does so with --install-missing) and verifies its SHA-256.
func UsqlPath(cmd *cobra.Command) (string, error) {
	if p, err := exec.LookPath("usql"); err == nil {
		return p, nil
	}
	cached := filepath.Join(toolsDir(), "usql", UsqlVersion, "usql")
	if info, err := os.Stat(cached); err == nil && info.Mode().IsRegular() {
		return cached, nil
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	want, ok := usqlSHA256[platform]
	if !ok {
		return "", output.NewError(output.CodeMissingTool, "usql is not on PATH and eru has no pinned build for "+platform,
			output.ExitMissingTool, "Install usql from https://github.com/xo/usql and retry.").
			WithDetails(map[string]any{"tool": "usql", "missing": []string{"usql"}})
	}
	url := usqlURL(runtime.GOOS, runtime.GOARCH)
	run := flagBool(cmd, "install-missing")
	if !run && canPrompt(cmd) {
		ok, err := ask(cmd, "Download usql "+UsqlVersion+" into "+filepath.Dir(cached)+"?")
		if err != nil {
			return "", err
		}
		run = ok
	}
	if !run {
		return "", output.NewError(output.CodeMissingTool, "usql is needed to open the console but is not on PATH",
			output.ExitMissingTool, "Re-run with --install-missing to download usql "+UsqlVersion+", or install it yourself.").
			WithDetails(map[string]any{"tool": "usql", "missing": []string{"usql"}, "download_url": url})
	}
	if err := downloadUsql(cmd.Context(), url, want, cached); err != nil {
		return "", output.WrapError(output.CodeExternalCommand, err, output.ExitExternal,
			"Install usql from https://github.com/xo/usql and retry.").
			WithDetails(map[string]any{"tool": "usql", "download_url": url})
	}
	return cached, nil
}

func usqlURL(goos, goarch string) string {
	return fmt.Sprintf("https://github.com/xo/usql/releases/download/%s/usql-%s-%s-%s.tar.bz2",
		UsqlVersion, UsqlVersion[1:], goos, goarch)
}

func toolsDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "eru-tools")
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "eru", "tools")
}

// downloadUsql fetches the archive, checks it against wantSHA, and extracts
// the usql binary to dest.
func downloadUsql(ctx context.Context, url, wantSHA, dest string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	archive, err := os.CreateTemp(filepath.Dir(dest), "usql-*.tar.bz2")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(archive.Name()); _ = archive.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(resp.Body, usqlMaxArchive+1))
	if err != nil {
		return err
	}
	if n > usqlMaxArchive {
		return errors.New("the usql archive is larger than expected")
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantSHA {
		return fmt.Errorf("usql archive sha256 mismatch: got %s, want %s", got, wantSHA)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extractUsql(archive, dest)
}

func extractUsql(r io.Reader, dest string) error {
	tr := tar.NewReader(bzip2.NewReader(r))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the usql archive has no usql binary")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != "usql" {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(dest), "usql-bin-*")
		if err != nil {
			return err
		}
		_, err = io.Copy(tmp, io.LimitReader(tr, usqlMaxArchive))
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
}
