package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/release"
)

// EnvNodeBinary points a dev build at the linux binary to push (§7.3).
const EnvNodeBinary = "ERU_NODE_BINARY"

// Artifact is a verified linux binary ready to push.
type Artifact struct {
	Path   string
	SHA256 string
	Build  node.BuildID
}

var safeVersion = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// NodeArtifact returns the node binary for arch (§7.5 step 1). Release builds
// download and verify the matching linux artifact into the release cache. Dev
// builds push ERU_NODE_BINARY, or the running binary when it is already
// linux/<arch>.
func NodeArtifact(ctx context.Context, arch string) (Artifact, error) {
	build := LocalBuild()
	var path string
	if buildinfo.Channel == "dev" {
		path = os.Getenv(EnvNodeBinary)
		if path == "" {
			if runtime.GOOS != "linux" || runtime.GOARCH != arch {
				return Artifact{}, output.NewError(
					output.CodeInputRequired,
					"a dev build needs "+EnvNodeBinary+" to push a linux/"+arch+" binary",
					output.ExitInput,
					"Build one with GOOS=linux GOARCH="+arch+" from the same commit and set "+EnvNodeBinary+" to its path.",
				).WithDetails(map[string]any{"env": EnvNodeBinary, "arch": arch})
			}
			self, err := os.Executable()
			if err != nil {
				return Artifact{}, err
			}
			path = self
		}
	} else {
		client := &release.Client{}
		m, err := client.Resolve(ctx, buildinfo.Channel, buildinfo.Version)
		if err != nil {
			return Artifact{}, err
		}
		path, err = client.Fetch(ctx, m, "linux", arch)
		if err != nil {
			return Artifact{}, err
		}
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Path: path, SHA256: sum, Build: build}, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the release cache or the operator's chosen dev binary.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Arch returns the node's Go architecture from uname -m.
func (n *Node) Arch(ctx context.Context) (string, error) {
	stdout, _, err := n.SSH.Output(ctx, "uname -m", nil)
	if err != nil {
		return "", output.WrapError(output.CodeTransportFailed, fmt.Errorf("uname -m on %s: %w", n.SSH.Target, err), output.ExitTransport, "")
	}
	arch := hostinfo.GoArch(string(stdout))
	if arch == "" {
		return "", output.NewError(
			output.CodePrecondition,
			"the node's architecture "+strings.TrimSpace(string(stdout))+" is not supported",
			output.ExitPrecondition,
			"eru nodes run on amd64 and arm64.",
		).WithDetails(map[string]any{"reason": "arch_unsupported"})
	}
	return arch, nil
}

// PublicIPv6 returns the source address of the node's IPv6 default route,
// or "" when it has none.
func (n *Node) PublicIPv6(ctx context.Context) string {
	stdout, _, err := n.SSH.Output(ctx, hostinfo.IPv6RouteCommand, nil)
	if err != nil {
		// No IPv6 route makes ip exit non-zero.
		return ""
	}
	return hostinfo.ParseIPv6Route(stdout)
}

// OS returns the node's /etc/os-release.
func (n *Node) OS(ctx context.Context) (hostinfo.OS, error) {
	stdout, _, err := n.SSH.Output(ctx, "cat "+hostinfo.OSReleasePath, nil)
	if err != nil {
		return hostinfo.OS{}, output.WrapError(output.CodeTransportFailed, fmt.Errorf("read os-release on %s: %w", n.SSH.Target, err), output.ExitTransport, "")
	}
	return hostinfo.ParseOSRelease(stdout), nil
}

// Upload streams art to /tmp/eru-<version>-<rand> as the SSH user (§7.5
// step 2) and returns the remote path.
func (n *Node) Upload(ctx context.Context, art Artifact) (string, error) {
	if !safeVersion.MatchString(art.Build.Version) {
		return "", fmt.Errorf("version %q is not safe in a path", art.Build.Version)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	remote := "/tmp/eru-" + art.Build.Version + "-" + hex.EncodeToString(suffix)
	f, err := os.Open(art.Path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var body io.Reader = f
	if n.OnUpload != nil {
		if info, err := f.Stat(); err == nil {
			body = &countingReader{r: f, total: info.Size(), report: n.OnUpload}
		}
	}
	var stderr bytes.Buffer
	if err := n.SSH.Run(ctx, "umask 077 && cat > "+remote, body, io.Discard, &stderr); err != nil {
		return "", output.WrapError(output.CodeTransportFailed, fmt.Errorf("upload the node binary: %w: %s", err, strings.TrimSpace(stderr.String())), output.ExitTransport, "")
	}
	return remote, nil
}

// countingReader reports how much of an upload has been read.
type countingReader struct {
	r      io.Reader
	sent   int64
	total  int64
	report func(sent, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.sent += int64(n)
	c.report(c.sent, c.total)
	return n, err
}

// bootstrapInstall installs the first node binary as root (§7.5 step 4): it
// copies the upload into a root-only staging file, checks the copy's sha256
// and that it runs, then moves it into place. The upload is always removed.
const bootstrapInstall = `set -eu
src="$1"
want="$2"
dst=/usr/local/bin/eru
stage=/usr/local/bin/eru.new
trap 'rm -f "$src" "$stage"' EXIT
install -d -m 0755 /usr/local/bin
rm -f "$stage"
install -o root -g root -m 0700 "$src" "$stage"
got="$(sha256sum "$stage" | cut -d' ' -f1)"
if [ "$got" != "$want" ]; then
  echo "sha256 mismatch: got $got" >&2
  exit 3
fi
"$stage" version --output json >/dev/null
chmod 0755 "$stage"
if [ -e "$dst" ]; then
  ln -f "$dst" "$dst.prev"
fi
mv -f "$stage" "$dst"
echo "installed $dst"
`

// InstallBootstrap pushes and installs art on a node that has no eru yet.
func (n *Node) InstallBootstrap(ctx context.Context, art Artifact) error {
	remote, err := n.Upload(ctx, art)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd := n.asRoot("/bin/sh -s -- " + remote + " " + art.SHA256)
	if err := n.SSH.Run(ctx, cmd, strings.NewReader(bootstrapInstall), io.Discard, &stderr); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "sha256 mismatch") {
			return output.NewError(output.CodeReleaseVerification, "the pushed binary does not match its sha256", output.ExitReleaseVerification,
				"Do not retry. Report this to a human.").WithDetails(map[string]any{"reason": "sha256_mismatch"})
		}
		return output.WrapError(output.CodeExternalCommand, fmt.Errorf("install the node binary: %w: %s", err, msg), output.ExitExternal,
			"The SSH user needs root or passwordless sudo on first contact.")
	}
	return nil
}

// InstallUpgrade pushes art and installs it through upgrade.install, which
// verifies, swaps and self-checks the binary on the node (§7.5 step 3).
func (n *Node) InstallUpgrade(ctx context.Context, art Artifact, opts CallOptions) (node.InstallResult, error) {
	remote, err := n.Upload(ctx, art)
	if err != nil {
		return node.InstallResult{}, err
	}
	reply, err := n.Call(ctx, "upgrade.install", map[string]string{
		"path":    remote,
		"sha256":  art.SHA256,
		"version": art.Build.Version,
		"commit":  art.Build.Commit,
		"channel": art.Build.Channel,
	}, opts)
	if err != nil {
		return node.InstallResult{}, err
	}
	var res node.InstallResult
	return res, reply.Decode(&res)
}
