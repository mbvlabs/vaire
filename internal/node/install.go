package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/release"
	"github.com/mbvlabs/vaire/internal/rpc"
)

type BuildID struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
}

type InstallResult struct {
	Previous   *BuildID `json:"previous"`
	Installed  BuildID  `json:"installed"`
	RolledBack bool     `json:"rolled_back"`
	Changed    bool     `json:"changed"`
}

type installParams struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
}

// The release client derives both caches from the process environment.
// RPC is a short-lived process, but serialize our temporary overrides.
var installCacheMu sync.Mutex

var installFetch = func(ctx context.Context, p installParams, cache string) (string, string, error) {
	installCacheMu.Lock()
	defer installCacheMu.Unlock()

	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		old, present := os.LookupEnv(key)
		if err := os.Setenv(key, cache); err != nil {
			return "", "", err
		}
		defer func(key, old string, present bool) {
			if present {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		}(key, old, present)
	}

	client := &release.Client{}

	m, err := client.Resolve(ctx, p.Channel, p.Version)
	if err != nil {
		return "", "", err
	}

	path, err := client.Fetch(ctx, m, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", "", err
	}

	return path, m.Artifacts[runtime.GOOS+"/"+runtime.GOARCH].SHA256, nil
}

var installVersion = binaryVersion
var installSelfcheck = newBinarySelfcheck
var installCurrentBinary = readInstalledBuild

func allowedInstallVersion(version, channel string) bool {
	v, ok := release.ParseStable(version)
	if !ok || (v.Pre != "" && v.Pre != "rc") {
		return false
	}

	if v.Pre == "rc" && channel != release.ChannelRC {
		return false
	}

	if v.Pre == "" && channel != release.ChannelStable {
		return false
	}

	floor, _ := release.ParseStable("1.0.0-rc.2")

	return release.CompareStable(v, floor) >= 0
}

func handleUpgradeInstall(ctx context.Context, env *Env) (Result, error) {
	if env == nil {
		return Result{}, installDenied("missing request")
	}

	if masterbackup.IsFenced() {
		return Result{}, fencedError()
	}

	verifiedOwner := env.DeviceIdentity != nil && env.DeviceIdentity.IdentityKind == rpc.IdentityVerifiedDevice &&
		env.DeviceIdentity.Role == config.AccessRoleOwner
	if !env.TrustedHostAdmin && !verifiedOwner {
		cfg, err := loadGuardCluster()
		if err != nil {
			return Result{}, err
		}

		if _, err := legacySSHAccount(os.Getenv("SUDO_UID"), cfg.Server.AdminUser); err != nil {
			return Result{}, err
		}
	}

	if env.Req.DryRun {
		return Result{}, installDenied("dry run is not supported")
	}

	var p installParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	if !allowedInstallVersion(p.Version, p.Channel) {
		return Result{}, installDenied("target must be a tagged RC2-or-later or final release")
	}

	running := BuildID{Version: buildinfo.Version, Commit: buildinfo.Commit, Channel: buildinfo.Channel}
	if !validInstalledBuild(running) {
		return Result{}, installDenied("the running node is not a tagged RC2-or-later or final release")
	}

	current, err := installCurrentBinary(ctx, BinaryPath)
	if err != nil {
		return Result{}, err
	}

	if !validInstalledBuild(current) {
		return Result{}, installDenied("the installed node is not a tagged RC2-or-later or final release")
	}

	if release.CompareVersions(p.Version, current.Version) < 0 {
		return Result{}, installDenied("downgrades are not allowed")
	}

	if current.Channel == release.ChannelStable && p.Channel != release.ChannelStable {
		return Result{}, installDenied("a stable node cannot install a prerelease")
	}

	if release.CompareVersions(p.Version, current.Version) == 0 {
		return Result{Data: InstallResult{Previous: &current, Installed: current}}, nil
	}

	return installRelease(ctx, p, current, BinaryPath)
}

func validInstalledBuild(build BuildID) bool {
	return allowedInstallVersion(build.Version, build.Channel) && build.Commit != "" && build.Commit != "none"
}

func installDenied(message string) error {
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, "")
}

// installRelease uses a private directory on the executable's filesystem for
// both release caches and staging. Its parent must not be writable by non-root.
func readInstalledBuild(ctx context.Context, binary string) (BuildID, error) {
	dir := filepath.Dir(binary)
	if err := secureInstallDir(dir); err != nil {
		return BuildID{}, err
	}

	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return BuildID{}, installDenied("installed binary is not a regular file")
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return BuildID{}, installDenied("installed binary is not root controlled")
	}

	return installVersion(ctx, binary)
}

func installRelease(ctx context.Context, p installParams, current BuildID, binary string) (Result, error) {
	disk, err := readInstalledBuild(ctx, binary)
	if err != nil || disk != current {
		return Result{}, installDenied("installed binary changed during upgrade")
	}

	return installReleaseAt(ctx, p, current, binary)
}

// installReleaseAt runs only after the caller has checked the installed path.
func installReleaseAt(ctx context.Context, p installParams, current BuildID, binary string) (Result, error) {
	dir := filepath.Dir(binary)

	stageDir, err := os.MkdirTemp(dir, ".vaire-upgrade-")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	source, sum, err := installFetch(ctx, p, stageDir)
	if err != nil {
		return Result{}, err
	}

	stage := filepath.Join(stageDir, "candidate")
	if err := copyVerifiedInstall(source, stage, sum); err != nil {
		return Result{}, err
	}

	got, err := installVersion(ctx, stage)
	if err != nil || got.Version != p.Version || got.Channel != p.Channel || got.Commit == "" || got.Commit == "none" {
		return Result{}, installDenied("signed candidate did not report the requested build")
	}

	if err := os.Chmod(stage, 0o755); err != nil { //nolint:gosec // The installed executable must be readable and executable by all users.
		return Result{}, err
	}

	if masterbackup.IsFenced() {
		return Result{}, fencedError()
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	previous := binary + ".prev"
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}

	if err := os.Link(binary, previous); err != nil {
		return Result{}, err
	}

	if err := os.Rename(stage, binary); err != nil {
		return Result{}, err
	}

	rollback := func(cause error, checks any) (Result, error) {
		if err := os.Rename(previous, binary); err != nil {
			return Result{}, fmt.Errorf("upgrade failed (%w); rollback failed: %w", cause, err)
		}

		if err := syncInstallDir(dir); err != nil {
			return Result{}, fmt.Errorf("upgrade rolled back but directory sync failed: %w", err)
		}

		return Result{}, output.NewError(output.CodeStepFailed, "the upgrade failed and was rolled back: "+cause.Error(), output.ExitStep, "").
			WithDetails(map[string]any{"reason": "upgrade_failed_rolled_back", "checks": checks, "previous": current, "installed": got, "rolled_back": true, "changed": false})
	}
	if err := syncInstallDir(dir); err != nil {
		return rollback(err, nil)
	}

	checks, passed := installSelfcheck(ctx, binary)
	if !passed {
		return rollback(fmt.Errorf("selfcheck failed"), checks)
	}

	return Result{Data: InstallResult{Previous: &current, Installed: got, Changed: true}}, nil
}

func secureInstallDir(dir string) error {
	for {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}

		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return installDenied("binary directory is not root controlled")
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}

		dir = parent
	}
}

func copyVerifiedInstall(source, dest, expected string) error {
	in, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return installDenied("release cache artifact is not a regular file")
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700) //nolint:gosec // Root-only executable required for version preflight.
	if err != nil {
		return err
	}

	defer func() { _ = out.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hash), in); err != nil {
		return err
	}

	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return installDenied("release artifact checksum mismatch")
	}

	return out.Sync()
}

func binaryVersion(ctx context.Context, path string) (BuildID, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	raw, err := exec.CommandContext(ctx, path, "version", "--output", "json").Output()
	if err != nil {
		return BuildID{}, err
	}

	var envelope struct {
		OK   bool    `json:"ok"`
		Data BuildID `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return BuildID{}, err
	}

	if !envelope.OK {
		return BuildID{}, fmt.Errorf("version reported ok=false")
	}

	return envelope.Data, nil
}

func newBinarySelfcheck(ctx context.Context, binary string) (any, bool) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	raw, err := exec.CommandContext(ctx, binary, "node", "selfcheck", "--output", "json").Output()

	var envelope struct {
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
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, false
	}

	if err != nil || !envelope.OK {
		if envelope.Error != nil {
			return envelope.Error.Details.Checks, false
		}

		return nil, false
	}

	return envelope.Data.Checks, true
}

func syncInstallDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	return d.Sync()
}
