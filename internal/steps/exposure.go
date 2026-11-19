package steps

import (
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/state"
)

func applyExposure(cfg config.Cluster, apply func() (engine.ApplyResult, error)) (engine.ApplyResult, error) {
	var result engine.ApplyResult

	err := (state.FileStore{}).WithExposure(cfg, func() error {
		var err error

		result, err = apply()

		return err
	})

	return result, err
}
