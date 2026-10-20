package localenv

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/spf13/cobra"
)

// Tool is an optional local program a feature needs (§20.1).
type Tool struct {
	// Name is what the error and prompt call it.
	Name string
	// Binaries must all be on PATH.
	Binaries []string
	// Packages maps a package manager (apt-get, dnf, pacman, brew) to the
	// package that provides the binaries.
	Packages map[string]string
	// Reason completes "needed to ...".
	Reason string
}

// Tools vaire knows how to offer.
var (
	ToolPsql = Tool{
		Name:     "psql",
		Binaries: []string{"psql"},
		Packages: map[string]string{"apt-get": "postgresql-client", "dnf": "postgresql", "pacman": "postgresql-libs", "brew": "libpq"},
		Reason:   "open an interactive SQL session",
	}
	ToolWireGuard = Tool{
		Name:     "wireguard-tools",
		Binaries: []string{"wg", "wg-quick"},
		Packages: map[string]string{"apt-get": "wireguard-tools", "dnf": "wireguard-tools", "pacman": "wireguard-tools", "brew": "wireguard-tools"},
		Reason:   "install a system WireGuard interface",
	}
)

// PackageManager returns the first supported package manager on PATH.
func PackageManager() string {
	order := []string{"apt-get", "dnf", "pacman"}
	if runtime.GOOS == "darwin" {
		order = []string{"brew"}
	}
	for _, pm := range order {
		if _, err := exec.LookPath(pm); err == nil {
			return pm
		}
	}
	return ""
}

// InstallCommand returns the exact install command for tool on this machine,
// or nil when no supported package manager is present.
func InstallCommand(tool Tool) []string {
	pm := PackageManager()
	pkg, ok := tool.Packages[pm]
	if !ok {
		return nil
	}
	var cmd []string
	switch pm {
	case "apt-get":
		cmd = []string{"apt-get", "install", "-y", pkg}
	case "dnf":
		cmd = []string{"dnf", "install", "-y", pkg}
	case "pacman":
		cmd = []string{"pacman", "-S", "--noconfirm", pkg}
	case "brew":
		return []string{"brew", "install", pkg}
	}
	if os.Geteuid() != 0 {
		cmd = append([]string{"sudo"}, cmd...)
	}
	return cmd
}

func missingBinaries(tool Tool) []string {
	var missing []string
	for _, bin := range tool.Binaries {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	return missing
}

// RequireTool makes sure tool is installed (§20.2). In a terminal it offers
// the install command; without one it returns missing_tool with
// details.install_command, or runs the command when --install-missing is set.
func RequireTool(cmd *cobra.Command, tool Tool) error {
	missing := missingBinaries(tool)
	if len(missing) == 0 {
		return nil
	}
	install := InstallCommand(tool)
	line := strings.Join(install, " ")
	run := false
	switch {
	case flagBool(cmd, "install-missing"):
		run = install != nil
	case canPrompt(cmd) && install != nil:
		ok, err := ask(cmd, "Install "+tool.Name+" with `"+line+"`?")
		if err != nil {
			return err
		}
		run = ok
	}
	if run {
		args := install
		if args[0] == "sudo" && !canPrompt(cmd) {
			args = append([]string{"sudo", "-n"}, args[1:]...)
		}
		child := exec.Command(args[0], args[1:]...) //nolint:gosec // G204: fixed package-manager command for a known tool.
		child.Stdin = os.Stdin
		child.Stdout = os.Stderr
		child.Stderr = os.Stderr
		if err := child.Run(); err != nil {
			return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "Install "+tool.Name+" by hand: "+line).
				WithDetails(map[string]any{"tool": tool.Name, "install_command": line})
		}
		if missing = missingBinaries(tool); len(missing) == 0 {
			return nil
		}
	}
	hint := "Install " + tool.Name + " and retry."
	if line != "" {
		hint = "Run `" + line + "`, or re-run with --install-missing."
	}
	return output.NewError(
		output.CodeMissingTool,
		tool.Name+" is needed to "+tool.Reason+" but "+strings.Join(missing, ", ")+" is not on PATH",
		output.ExitMissingTool,
		hint,
	).WithDetails(map[string]any{"tool": tool.Name, "missing": missing, "install_command": line})
}

// CheckReachable dials addr over TCP once (§20.1, server init remote).
func CheckReachable(ctx context.Context, addr string, timeout time.Duration) error {
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		hint := "Check the address and that SSH is open on it."
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Timeout() {
			hint = "The connection timed out. Check the provider firewall and the address."
		}
		return output.WrapError(output.CodeRemoteUnreachable, err, output.ExitTransport, hint).
			WithDetails(map[string]any{"address": addr})
	}
	_ = conn.Close()
	return nil
}

// CanPrompt reports whether this invocation may prompt.
func CanPrompt(cmd *cobra.Command) bool { return canPrompt(cmd) }
