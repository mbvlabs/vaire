package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/steps"
)

// Physical ownership fixtures model the files that PostgreSQL packages would
// supply, without changing the live host or bypassing file verification.
func useExtensionFileFixture(t *testing.T) {
	t.Helper()
	control, libraries := t.TempDir(), t.TempDir()
	for _, ext := range catalog.List() {
		if ext.ExtName != "" {
			if err := os.WriteFile(filepath.Join(control, ext.ExtName+".control"), []byte("default_version = '1.0'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if ext.Preload != "" {
			name := strings.TrimSuffix(ext.Preload, ".so") + ".so"
			if err := os.WriteFile(filepath.Join(libraries, name), []byte("test library"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(steps.SetExtensionDirectoriesForTest(control, libraries))
	oldWriter := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, mode os.FileMode) error {
		return os.WriteFile(path, data, mode)
	})
	t.Cleanup(func() { backup.RestoreCopyGuardWriterForTest(oldWriter) })
}
