// Package state is the desired-state store (§5.1, D15): the file at
// /etc/eru/cluster.toml. Every save marks the master backup dirty, so the
// bundle in the bucket follows within seconds (Phase 5).
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/masterbackup"
)

// Path is the default desired-state file.
const Path = config.ClusterPath

// Store reads and writes the cluster's desired state.
type Store interface {
	// Load returns the parsed cluster and its raw bytes.
	Load() (config.Cluster, []byte, error)
	// Save validates raw and writes it atomically.
	Save(raw []byte) error
	// Revision is the sha256 of the file contents.
	Revision() (string, error)
}

// FileStore is the node's /etc/eru/cluster.toml.
type FileStore struct {
	// Path defaults to Path.
	Path string
}

func (s FileStore) path() string {
	if s.Path == "" {
		return Path
	}
	return s.Path
}

// Load reads and validates cluster.toml.
func (s FileStore) Load() (config.Cluster, []byte, error) {
	raw, err := os.ReadFile(s.path()) //nolint:gosec // G304: fixed desired-state path.
	if err != nil {
		return config.Cluster{}, nil, err
	}
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return config.Cluster{}, nil, configError(err)
	}
	if err := cfg.Validate(); err != nil {
		return config.Cluster{}, nil, config.ValidationError(err)
	}
	return cfg, raw, nil
}

// Save validates raw with the full scope and writes it atomically.
func (s FileStore) Save(raw []byte) error {
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return configError(err)
	}
	if err := cfg.Validate(); err != nil {
		return config.ValidationError(err)
	}
	if err := engine.WriteFileAtomic(s.path(), raw, config.FileMode); err != nil {
		return err
	}
	if s.path() == Path {
		masterbackup.MarkDirty("cluster.toml")
	}
	return nil
}

// Revision returns the sha256 of the file.
func (s FileStore) Revision() (string, error) {
	raw, err := os.ReadFile(s.path()) //nolint:gosec // G304: fixed desired-state path.
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func configError(err error) error {
	return output.NewError(
		output.CodeConfig,
		"cluster.toml does not parse: "+err.Error(),
		output.ExitConfig,
		"Fix the file, or restore it from the secrets document.",
	)
}
