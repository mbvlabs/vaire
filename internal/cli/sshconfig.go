package cli

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/contexts"
	"golang.org/x/crypto/ssh"
)

// hostFlagExplicit reports whether a user@host[:port] value names the user
// and the port, so ~/.ssh/config only fills what the operator left out.
func hostFlagExplicit(v string) (user, port bool) {
	rest := strings.TrimSpace(v)
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		user, rest = true, rest[at+1:]
	}
	_, _, err := net.SplitHostPort(rest)
	return user, err == nil
}

// applySSHConfig fills HostName, User, Port and IdentityFile from the
// matching Host block in ~/.ssh/config for first contact. Values the operator
// gave explicitly win. It returns a short description of what was used, or ""
// when nothing matched.
func applySSHConfig(boot *contexts.Bootstrap, userGiven, portGiven, identityGiven bool) string {
	home, err := os.UserHomeDir()
	if err != nil || boot.Host == "" {
		return ""
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config")) //nolint:gosec // G304: the operator's own ssh config.
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	cfg, err := ssh_config.Decode(f)
	if err != nil {
		return ""
	}
	alias := boot.Host
	var used []string
	if v, _ := cfg.Get(alias, "HostName"); v != "" && v != alias {
		boot.Host = v
		used = append(used, "HostName")
	}
	if v, _ := cfg.Get(alias, "User"); v != "" && !userGiven {
		boot.User = v
		used = append(used, "User")
	}
	if v, _ := cfg.Get(alias, "Port"); v != "" && !portGiven {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			boot.Port = n
			used = append(used, "Port")
		}
	}
	if !identityGiven {
		files, _ := cfg.GetAll(alias, "IdentityFile")
		for _, file := range files {
			path := expandHome(file)
			if _, err := os.Stat(path); err == nil {
				boot.Identity = path
				used = append(used, "IdentityFile")
				break
			}
		}
	}
	if len(used) == 0 {
		return ""
	}
	return "~/.ssh/config Host " + alias + " (" + strings.Join(used, ", ") + ")"
}

// checkIdentity fails early when the --identity file can't be used, before
// anything is prompted or generated.
func checkIdentity(path string) error {
	if path == "" {
		return nil
	}
	full := expandHome(path)
	raw, err := os.ReadFile(full) //nolint:gosec // G304: the operator chose this identity file.
	if err != nil {
		return output.NewError(output.CodeUsage, "the --identity file "+full+" can't be read: "+unwrapPathError(err), output.ExitUsage,
			"Pass the path of a readable private key with --identity.").WithDetails(map[string]any{"flag": "--identity", "path": full})
	}
	if _, err := ssh.ParsePrivateKey(raw); err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil
		}
		return output.NewError(output.CodeUsage, "the --identity file "+full+" is not an SSH private key: "+err.Error(), output.ExitUsage,
			"Pass the private key, not the .pub file.").WithDetails(map[string]any{"flag": "--identity", "path": full})
	}
	return nil
}

func unwrapPathError(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
