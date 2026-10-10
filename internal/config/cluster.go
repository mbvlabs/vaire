package config

import (
	"bytes"
	"errors"
	"os"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/pelletier/go-toml/v2"
)

// ClusterPath is the desired state on the central command.
const ClusterPath = "/etc/eru/cluster.toml"

// Cluster is the part of cluster.toml read so far. Unknown keys are allowed
// until Phase 2 adds the full schema and switches to a strict decode.
type Cluster struct {
	Cluster ClusterTable `toml:"cluster"`
}

// ClusterTable is the [cluster] table.
type ClusterTable struct {
	Name string `toml:"name"`
}

// Name returns [cluster] name.
func (c Cluster) Name() string { return c.Cluster.Name }

// LoadCluster reads path. A missing file returns cluster_not_initialized. A
// file that does not parse, or has no [cluster] name, returns config_error.
func LoadCluster(path string) (Cluster, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed cluster config path.
	if err != nil {
		if IsNotExist(err) {
			return Cluster{}, output.NewError(
				output.CodeClusterNotInitialized,
				"this node has no cluster.toml",
				output.ExitContext,
				"Initialise the cluster first with eru cluster init.",
			).WithDetails(map[string]any{"path": path})
		}
		return Cluster{}, configError(path, err)
	}
	var cfg Cluster
	if err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&cfg); err != nil {
		return Cluster{}, configError(path, err)
	}
	if strings.TrimSpace(cfg.Cluster.Name) == "" {
		return Cluster{}, configError(path, errors.New("missing [cluster] name"))
	}
	return cfg, nil
}
