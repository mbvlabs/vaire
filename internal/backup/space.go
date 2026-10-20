package backup

import (
	"fmt"
	"os"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

// Space headroom: what a copy of a database needs beyond the database itself
// (system catalogs, WAL replayed from the archive) and what the filesystem
// must keep free afterwards.
const (
	copyOverhead = 1 << 30
	keepFree     = 2 << 30
)

// SpaceNeed is a free-space requirement on one filesystem.
type SpaceNeed struct {
	Path      string `json:"path"`
	Need      uint64 `json:"need_bytes"`
	Free      uint64 `json:"free_bytes"`
	Unknown   bool   `json:"unknown,omitempty"`
	Purpose   string `json:"purpose"`
	Shortfall uint64 `json:"shortfall_bytes,omitempty"`
}

// CheckSpace checks the needs and returns precondition_failed with reason
// insufficient_space when one filesystem is short. Needs on the same
// filesystem as an earlier one add up.
func CheckSpace(needs []SpaceNeed) ([]SpaceNeed, error) {
	byDev := map[uint64]uint64{}
	out := make([]SpaceNeed, 0, len(needs))
	for _, n := range needs {
		free, ok := hostinfo.FreeBytes(existingParent(n.Path))
		if !ok {
			n.Unknown = true
			out = append(out, n)
			continue
		}
		n.Free = free
		dev := deviceOf(existingParent(n.Path))
		byDev[dev] += n.Need
		if total := byDev[dev] + keepFree; total > free {
			n.Shortfall = total - free
		}
		out = append(out, n)
	}
	for _, n := range out {
		if n.Shortfall > 0 {
			return out, output.NewError(output.CodePrecondition,
				fmt.Sprintf("not enough free space on %s for %s: need %s more", n.Path, n.Purpose, humanBytes(n.Shortfall)),
				output.ExitPrecondition,
				"Free space on that filesystem, or resize the disk, then retry.").
				WithDetails(map[string]any{"reason": "insufficient_space", "space": out})
		}
	}
	return out, nil
}

// CopyNeed is the space a temporary cluster copying a database of size
// bytes needs: a restore, a fork or a drill.
func CopyNeed(size int64) uint64 {
	if size < 0 {
		size = 0
	}
	return uint64(size) + copyOverhead
}

// existingParent walks up to the nearest path that exists.
func existingParent(path string) string {
	for p := path; ; {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := parentDir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func parentDir(p string) string {
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "/"
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
