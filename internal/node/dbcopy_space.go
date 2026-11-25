package node

import (
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

// workstationCopyFreeBytes is read on the actual data root while db.create
// holds the exclusive node lock. An unavailable filesystem fails closed.
var workstationCopyFreeBytes = hostinfo.FreeBytes

func checkWorkstationCopySpace(dataRoot string, sourceBytes int64) error {
	if sourceBytes <= 0 {
		return output.NewError(output.CodeValidation, "invalid copy size", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "invalid_copy_size"})
	}

	free, ok := workstationCopyFreeBytes(dataRoot)
	if !ok {
		return output.NewError(output.CodePrecondition, "destination space could not be checked", output.ExitPrecondition,
			"Ask an administrator to check the destination filesystem.").WithDetails(map[string]any{"reason": "copy_space_unavailable"})
	}

	// The source footprint already includes its indexes. Allow another full
	// footprint for index build/transient growth, plus 1 GiB for WAL and
	// cluster overhead and 2 GiB left free. This is an estimate, not a quota
	// or a guarantee: concurrent writes and restore behavior can exceed it.
	need := uint64(sourceBytes)*2 + 3<<30
	if free < need {
		return output.NewError(output.CodePrecondition, "destination has insufficient space for the estimated copy", output.ExitPrecondition,
			"Free space or choose a larger destination, then retry. This estimate is not a quota.").WithDetails(map[string]any{"reason": "insufficient_copy_space"})
	}

	return nil
}
