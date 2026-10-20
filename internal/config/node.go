package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/pelletier/go-toml/v2"
)

// NodePath marks a host as an vaire node.
const NodePath = "/etc/vaire/node.toml"

// Node is /etc/vaire/node.toml (§6.2). Bootstrap writes it: the server
// (setup) this node belongs to, the node's name and hostname, and its role.
type Node struct {
	Server      string    `toml:"server"`
	NodeName    string    `toml:"node_name"`
	Hostname    string    `toml:"hostname"`
	Role        string    `toml:"role"`
	InstalledAt time.Time `toml:"installed_at"`
}

// LoadNode reads path. A missing file returns an error matching fs.ErrNotExist.
// A file that does not parse returns config_error.
func LoadNode(path string) (Node, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed node config path.
	if err != nil {
		return Node{}, err
	}
	var node Node
	if err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&node); err != nil {
		return Node{}, configError(path, err)
	}
	var missing []string
	if strings.TrimSpace(node.Server) == "" {
		missing = append(missing, "server")
	}
	if strings.TrimSpace(node.NodeName) == "" {
		missing = append(missing, "node_name")
	}
	if len(missing) > 0 {
		return Node{}, configError(path, fmt.Errorf("missing %s", strings.Join(missing, ", ")))
	}
	return node, nil
}

// IsNotExist reports whether err means the config file is absent.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

func configError(path string, cause error) *output.CLIError {
	return output.NewError(
		output.CodeConfig,
		fmt.Sprintf("%s does not parse: %v", path, cause),
		output.ExitConfig,
		"Fix the file, or restore it from the secrets document.",
	).WithDetails(map[string]any{"path": path})
}
