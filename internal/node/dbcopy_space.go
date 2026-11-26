package node

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

// workstationCopyFreeBytes is read while db.create holds the exclusive node
// lock. An unavailable filesystem fails closed.
var workstationCopyFreeBytes = hostinfo.FreeBytes

// A first database may not have a data root yet. Its directories will be
// created on the nearest existing ancestor's filesystem.
func workstationCopyDataFilesystem(dataRoot string) (string, bool) {
	if !filepath.IsAbs(dataRoot) {
		return "", false
	}

	for path := filepath.Clean(dataRoot); ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(path)
			}

			return path, err == nil && info.IsDir()
		}

		// Do not fall back on permission errors, non-directory components,
		// or a broken symlink at the first existing path.
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(path) == path {
			return "", false
		}
	}
}

func checkWorkstationCopySpace(dataRoot string, sourceBytes int64) error {
	if sourceBytes <= 0 {
		return output.NewError(output.CodeValidation, "invalid copy size", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "invalid_copy_size"})
	}

	path, ok := workstationCopyDataFilesystem(dataRoot)
	if !ok {
		return copySpaceUnavailable()
	}

	free, ok := workstationCopyFreeBytes(path)
	if !ok {
		return copySpaceUnavailable()
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

func copySpaceUnavailable() error {
	return output.NewError(output.CodePrecondition, "destination space could not be checked", output.ExitPrecondition,
		"Ask an administrator to check the destination filesystem.").WithDetails(map[string]any{"reason": "copy_space_unavailable"})
}
