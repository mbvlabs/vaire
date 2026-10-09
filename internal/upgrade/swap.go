package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/release"
)

const preflightTimeout = 10 * time.Second

// Preflight runs the cached binary and checks version and channel.
func Preflight(ctx context.Context, cached string, m release.Manifest) error {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cached, "version", "--output", "json") //nolint:gosec // G204: path is a verified cached release.
	out, err := cmd.Output()
	if err != nil {
		return output.WrapError(
			output.CodeReleaseVerification,
			err,
			output.ExitReleaseVerification,
			"Do not retry. Report this to a human.",
		)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Version string `json:"version"`
			Channel string `json:"channel"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		return output.NewError(
			output.CodeReleaseVerification,
			"cached release failed the version preflight",
			output.ExitReleaseVerification,
			"Do not retry. Report this to a human.",
		)
	}
	if env.Data.Version != m.Version || env.Data.Channel != m.Channel {
		return output.NewError(
			output.CodeReleaseVerification,
			fmt.Sprintf("cached release reported %s/%s, expected %s/%s", env.Data.Version, env.Data.Channel, m.Version, m.Channel),
			output.ExitReleaseVerification,
			"Do not retry. Report this to a human.",
		)
	}
	return nil
}

// Swap replaces target with the cached binary. previous is target+".prev" when kept.
func Swap(target, cached string) (previous string, warnings []output.Warning, err error) {
	dir := filepath.Dir(target)
	previous = target + ".prev"
	if _, statErr := os.Lstat(target); statErr == nil {
		_ = os.Remove(previous)
		if linkErr := os.Link(target, previous); linkErr != nil {
			previous = ""
			warnings = append(warnings, output.Warning{
				Code:    "no_previous_kept",
				Message: "could not keep the previous binary as a hard link",
			})
		}
	} else {
		previous = ""
	}

	suffix, err := randomHex(8)
	if err != nil {
		return previous, warnings, err
	}
	tmp := filepath.Join(dir, ".eru.new-"+suffix)
	if err := copyFile(tmp, cached, 0o755); err != nil {
		_ = os.Remove(tmp)
		return previous, warnings, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if os.Geteuid() == 0 {
		if chownErr := copyOwner(tmp, target); chownErr != nil {
			return previous, warnings, chownErr
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil { //nolint:gosec // G302: the installed binary must be executable.
		return previous, warnings, err
	}
	if err := fsync(tmp); err != nil {
		return previous, warnings, err
	}
	if err := fsync(dir); err != nil {
		return previous, warnings, err
	}
	if err := os.Rename(tmp, target); err != nil {
		return previous, warnings, err
	}
	if err := fsync(dir); err != nil {
		return previous, warnings, err
	}
	return previous, warnings, nil
}

func copyFile(dst, src string, perm os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is the verified cache path.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: dst is in the target directory.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyOwner(dst, src string) error {
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(dst, int(stat.Uid), int(stat.Gid))
}

func fsync(path string) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is the upgrade target or its directory.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
