package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/transport"
)

// LocalBuild is the operator's own build.
func LocalBuild() node.BuildID {
	return node.BuildID{Version: buildinfo.Version, Commit: buildinfo.Commit, Channel: buildinfo.Channel}
}

// SameBuild applies the node's version rule (§7.4): version and channel must
// match, and for dev builds the commit too.
func SameBuild(a, b node.BuildID) bool {
	if a.Version != b.Version || a.Channel != b.Channel {
		return false
	}
	if a.Channel == "dev" {
		return a.Commit == b.Commit
	}
	return true
}

// Version runs eru node version on the node. installed is false when the
// node has no eru binary yet.
func (n *Node) Version(ctx context.Context) (node.BuildID, bool, error) {
	stdout, stderr, err := n.SSH.Output(ctx, node.BinaryPath+" node version --output json", nil)
	if err != nil {
		if status, ok := transport.ExitStatus(err); ok && (status == 127 || status == 126) {
			return node.BuildID{}, false, nil
		}
		if strings.Contains(string(stderr), "No such file") || strings.Contains(string(stderr), "not found") {
			return node.BuildID{}, false, nil
		}
		return node.BuildID{}, false, output.WrapError(output.CodeTransportFailed, fmt.Errorf("eru node version failed on %s: %w", n.SSH.Target, err), output.ExitTransport, "")
	}
	var env struct {
		OK   bool         `json:"ok"`
		Data node.BuildID `json:"data"`
	}
	if err := json.Unmarshal(stdout, &env); err != nil || !env.OK {
		return node.BuildID{}, true, output.NewError(output.CodeTransportFailed, "eru node version on "+n.SSH.Target.String()+" printed no version", output.ExitTransport, "")
	}
	return env.Data, true, nil
}

// HandshakeOptions steer Handshake.
type HandshakeOptions struct {
	// AutoUpgrade installs the operator's build on a mismatched node.
	AutoUpgrade bool
	// Progress receives one line per action.
	Progress func(line string)
}

// Handshake checks that the node runs the operator's build (§7.3). With
// AutoUpgrade a mismatched node gets the operator's build first.
func (n *Node) Handshake(ctx context.Context, opts HandshakeOptions) error {
	got, installed, err := n.Version(ctx)
	if err != nil {
		return err
	}
	want := LocalBuild()
	if installed && SameBuild(got, want) {
		return nil
	}
	if !opts.AutoUpgrade {
		return versionMismatch(got, want, installed)
	}
	if opts.Progress != nil {
		opts.Progress(fmt.Sprintf("node runs %s (%s); installing %s (%s)", got.Version, got.Channel, want.Version, want.Channel))
	}
	arch, err := n.Arch(ctx)
	if err != nil {
		return err
	}
	art, err := NodeArtifact(ctx, arch)
	if err != nil {
		return err
	}
	if installed {
		if _, err := n.InstallUpgrade(ctx, art, CallOptions{}); err != nil {
			return err
		}
	} else if err := n.InstallBootstrap(ctx, art); err != nil {
		return err
	}
	got, installed, err = n.Version(ctx)
	if err != nil {
		return err
	}
	if !installed || !SameBuild(got, want) {
		return versionMismatch(got, want, installed)
	}
	return nil
}

func versionMismatch(got, want node.BuildID, installed bool) *output.CLIError {
	message := fmt.Sprintf("the node runs eru %s (%s, %s); this CLI is %s (%s, %s)",
		got.Version, got.Commit, got.Channel, want.Version, want.Commit, want.Channel)
	if !installed {
		message = "eru is not installed on the node"
	}
	return output.NewError(
		output.CodeNodeVersionMismatch,
		message,
		output.ExitVersion,
		"Run eru node upgrade, or re-run with --auto-upgrade-nodes.",
	).WithDetails(map[string]any{"node": got, "client": want, "installed": installed})
}
