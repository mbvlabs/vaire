package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

// Ordinary node tests run without a real node state directory. Give them an
// explicitly empty trusted directory; tests of unavailable metadata replace it.
func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "vaire-node-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	state := filepath.Join(base, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		_ = os.RemoveAll(base)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	materialisationAncestorBase = base
	materialisationStateDir = state
	materialisationRecordUID = uint32(os.Geteuid()) //nolint:gosec // test process identity
	backup.RecordsPath = filepath.Join(state, "records.json")
	// Job runtime tests must not use Detect's external route probes.
	oldJobHostRead, oldMaterialisationHost, oldDetectHost := jobRuntimeHostRead, materialisationHostInfo, detectHost
	testHost := func(string) (hostinfo.Info, error) {
		return hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}, nil
	}
	jobRuntimeHostRead, materialisationHostInfo, detectHost = testHost, testHost, testHost
	oldRelease, oldReap := releaseMaterialisationJob, reapDrillJob
	releaseMaterialisationJob = func(string) {}
	reapDrillJob = func(context.Context, string) error { return errors.New("test reaper unavailable") }
	code := m.Run()
	releaseMaterialisationJob, reapDrillJob = oldRelease, oldReap
	jobRuntimeHostRead, materialisationHostInfo, detectHost = oldJobHostRead, oldMaterialisationHost, oldDetectHost
	if err := os.RemoveAll(base); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}
