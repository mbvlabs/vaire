package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// upgrade.install params and result are frozen with vaire-rpc/v1: a newer
// operator calls this method on an older node binary. Additions must be
// optional fields.

const (
	installStaging      = BinaryPath + ".new"
	installPrevious     = BinaryPath + ".prev"
	installVersionLimit = 10 * time.Second
	installCheckLimit   = 60 * time.Second
)

var (
	installPathPattern = regexp.MustCompile(`^/tmp/vaire-[A-Za-z0-9._-]+$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type installParams struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
}

// BuildID names one vaire build.
type BuildID struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
}

// InstallResult is the upgrade.install result.
type InstallResult struct {
	Previous   *BuildID `json:"previous"`
	Installed  BuildID  `json:"installed"`
	RolledBack bool     `json:"rolled_back"`
}

func handleUpgradeInstall(ctx context.Context, env *Env) (Result, error) {
	var p installParams
	// Lenient decode: a newer operator may add optional fields.
	if len(bytes.TrimSpace(env.Req.Params)) > 0 {
		if err := json.Unmarshal(env.Req.Params, &p); err != nil {
			return Result{}, output.NewError(output.CodeUsage, "params do not decode: "+err.Error(), output.ExitUsage, "")
		}
	}
	p.SHA256 = strings.ToLower(strings.TrimSpace(p.SHA256))
	if err := validateInstallParams(p); err != nil {
		return Result{}, err
	}
	progress := func(line string) {
		env.Report(output.Event{Type: "log", Step: "upgrade.install", Line: line})
	}

	// 1. The source must be a regular file in /tmp owned by the SSH user or root.
	if err := checkInstallSource(p.Path); err != nil {
		return Result{}, err
	}
	defer func() { _ = os.Remove(p.Path) }()

	// 2. Copy into a root-only staging file while hashing, so the verified
	// bytes are the installed bytes.
	sum, err := stageBinary(p.Path)
	if err != nil {
		_ = os.Remove(installStaging)
		return Result{}, err
	}
	if subtle.ConstantTimeCompare([]byte(sum), []byte(p.SHA256)) != 1 {
		_ = os.Remove(installStaging)
		return Result{}, verificationFailed("the pushed binary does not match the expected sha256", map[string]any{"reason": "sha256_mismatch"})
	}
	progress("sha256 verified")

	// 3. The staged binary must report the expected build.
	got, err := binaryVersion(ctx, installStaging)
	if err != nil {
		_ = os.Remove(installStaging)
		return Result{}, verificationFailed("the pushed binary failed its version preflight", map[string]any{"reason": "preflight_failed"})
	}
	want := BuildID{Version: p.Version, Commit: p.Commit, Channel: p.Channel}
	if got != want {
		_ = os.Remove(installStaging)
		return Result{}, verificationFailed(
			fmt.Sprintf("the pushed binary reports %s (%s, %s), expected %s (%s, %s)", got.Version, got.Commit, got.Channel, want.Version, want.Commit, want.Channel),
			map[string]any{"reason": "version_mismatch", "got": got, "want": want},
		)
	}
	progress("version preflight passed")

	// 4. Swap: vaire -> vaire.prev (hard link), vaire.new -> vaire.
	var previous *BuildID
	hadPrevious := false
	if _, err := os.Lstat(BinaryPath); err == nil {
		if prev, err := binaryVersion(ctx, BinaryPath); err == nil {
			previous = &prev
		}
		_ = os.Remove(installPrevious)
		if err := os.Link(BinaryPath, installPrevious); err != nil {
			_ = os.Remove(installStaging)
			return Result{}, err
		}
		hadPrevious = true
	}
	if err := os.Chmod(installStaging, 0o755); err != nil { //nolint:gosec // G302: §6.2 binary mode.
		_ = os.Remove(installStaging)
		return Result{}, err
	}
	if err := os.Rename(installStaging, BinaryPath); err != nil {
		_ = os.Remove(installStaging)
		return Result{}, err
	}
	if err := syncDir(filepath.Dir(BinaryPath)); err != nil {
		return Result{}, err
	}
	progress("installed " + BinaryPath)

	// 5. The new binary checks itself. A failure rolls back.
	checks, ok := newBinarySelfcheck(ctx)
	if !ok {
		if hadPrevious {
			err = os.Rename(installPrevious, BinaryPath)
		} else {
			err = os.Remove(BinaryPath)
		}
		if err != nil {
			return Result{}, output.WrapError(output.CodeError, err, output.ExitGeneric, "Rollback failed. Inspect /usr/local/bin by hand.")
		}
		_ = syncDir(filepath.Dir(BinaryPath))
		progress("selfcheck failed; rolled back")
		return Result{}, (&output.CLIError{
			Code:     output.CodeStepFailed,
			Message:  "the new binary failed its selfcheck and was rolled back",
			Hint:     "Fix the failed checks in details.checks, then push the binary again.",
			ExitCode: output.ExitStep,
		}).WithDetails(map[string]any{"reason": "selfcheck_failed_rolled_back", "checks": checks})
	}
	progress("selfcheck passed")

	// 6. The deferred remove deletes the source.
	return Result{Data: InstallResult{Previous: previous, Installed: want, RolledBack: false}}, nil
}

func validateInstallParams(p installParams) error {
	var bad []string
	if !installPathPattern.MatchString(p.Path) {
		bad = append(bad, "path")
	}
	if !sha256Pattern.MatchString(p.SHA256) {
		bad = append(bad, "sha256")
	}
	if p.Version == "" {
		bad = append(bad, "version")
	}
	if p.Commit == "" {
		bad = append(bad, "commit")
	}
	if p.Channel == "" {
		bad = append(bad, "channel")
	}
	if len(bad) == 0 {
		return nil
	}
	return output.NewError(
		output.CodeValidation,
		"upgrade.install params are invalid: "+strings.Join(bad, ", "),
		output.ExitConfig,
		"path must match /tmp/vaire-*, sha256 is 64 hex characters, and version, commit and channel are required.",
	).WithDetails(map[string]any{"invalid": bad})
}

func checkInstallSource(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return installSourceInvalid(path, "missing")
	}
	if !info.Mode().IsRegular() {
		return installSourceInvalid(path, "not a regular file")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return installSourceInvalid(path, "owner unknown")
	}
	allowed := map[uint32]bool{0: true}
	if raw := os.Getenv("SUDO_UID"); raw != "" {
		if uid, err := strconv.ParseUint(raw, 10, 32); err == nil {
			allowed[uint32(uid)] = true
		}
	}
	if !allowed[st.Uid] {
		return installSourceInvalid(path, "owned by uid "+strconv.FormatUint(uint64(st.Uid), 10))
	}
	return nil
}

func installSourceInvalid(path, why string) error {
	return output.NewError(
		output.CodePrecondition,
		"the pushed binary "+path+" is unusable: "+why,
		output.ExitPrecondition,
		"Push the binary again as the SSH user or root.",
	).WithDetails(map[string]any{"reason": "install_source_invalid", "path": path})
}

// stageBinary copies src to installStaging (root, 0700) and returns the
// sha256 of the copied bytes.
func stageBinary(src string) (string, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: path matched /tmp/vaire-* and was checked.
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", installSourceInvalid(src, "changed while opening")
	}
	_ = os.Remove(installStaging)
	out, err := os.OpenFile(installStaging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // G302: root-only staging file, made 0755 after verification.
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Chown(0, 0); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func binaryVersion(ctx context.Context, path string) (BuildID, error) {
	ctx, cancel := context.WithTimeout(ctx, installVersionLimit)
	defer cancel()
	raw, err := exec.CommandContext(ctx, path, "version", "--output", "json").Output() //nolint:gosec // G204: verified staged or installed binary.
	if err != nil {
		return BuildID{}, err
	}
	var env struct {
		OK   bool    `json:"ok"`
		Data BuildID `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return BuildID{}, err
	}
	if !env.OK {
		return BuildID{}, errors.New("version reported ok=false")
	}
	return env.Data, nil
}

// newBinarySelfcheck runs the installed binary's selfcheck and returns its
// checks and whether it passed.
func newBinarySelfcheck(ctx context.Context) (any, bool) {
	ctx, cancel := context.WithTimeout(ctx, installCheckLimit)
	defer cancel()
	raw, runErr := exec.CommandContext(ctx, BinaryPath, "node", "selfcheck", "--output", "json").Output() //nolint:gosec // G204: fixed node binary path.
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Checks any `json:"checks"`
		} `json:"data"`
		Error *struct {
			Details struct {
				Checks any `json:"checks"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return []Check{{Name: "selfcheck", Status: CheckFail, Detail: "the new binary's selfcheck output did not parse"}}, false
	}
	if runErr != nil || !env.OK {
		if env.Error != nil && env.Error.Details.Checks != nil {
			return env.Error.Details.Checks, false
		}
		return []Check{{Name: "selfcheck", Status: CheckFail, Detail: "the new binary's selfcheck failed"}}, false
	}
	return env.Data.Checks, true
}

func verificationFailed(message string, details map[string]any) error {
	return output.NewError(
		output.CodeReleaseVerification,
		message,
		output.ExitReleaseVerification,
		"Do not retry. Report this to a human.",
	).WithDetails(details)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: fixed binary directory.
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
