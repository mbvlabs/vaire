package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/mbvlabs/vaire/internal/config"
)

// Restore owns its setup access but must not claim the databases as fresh
// bootstrap targets. Their restored records still gate data recovery.
const restoreAccessManifest = "restore-access.json"

func claimRestoreAccess(cfg config.Cluster, node, operation, instance string) error {
	if _, ok := cfg.Node(node); !ok || operation == "" || instance == "" {
		return errors.New("restore setup identity is incomplete")
	}

	hash, err := bootstrapHash(cfg)
	if err != nil {
		return err
	}

	if err := bootstrapDir(true); err != nil {
		return err
	}

	want := bootstrapOwnership{Version: 1, Server: cfg.Name(), Node: node, Operation: operation,
		Instance: instance, ConfigHash: hash, Targets: map[string]bool{}}

	old, err := readBootstrapOwnership(restoreAccessManifest)
	if err == nil {
		if old.Server != want.Server || old.Node != want.Node || old.Operation != want.Operation ||
			old.Instance != want.Instance || old.ConfigHash != want.ConfigHash {
			return errors.New("restore access belongs to another setup")
		}

		return nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := bootstrapEnsureUnowned(); err != nil {
		return err
	}

	raw, err := json.Marshal(want)
	if err != nil {
		return err
	}

	return bootstrapWriteNew(filepath.Join(bootstrapStateDir, restoreAccessManifest), append(raw, '\n'))
}
