package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/engine"
)

func TestSwapState(t *testing.T) {
	cases := []struct {
		name  string
		dirs  []string
		state string
	}{
		{"not started", []string{"app", "app__restoring"}, SwapNotStarted},
		{"halfway", []string{"app__restoring", "app__pre_restore_1"}, SwapHalfway},
		{"done", []string{"app", "app__pre_restore_1"}, SwapDone},
		{"conflict", []string{"app", "app__restoring", "app__pre_restore_1"}, SwapConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range c.dirs {
				if err := os.Mkdir(filepath.Join(root, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if got := SwapState(root, "app", "app__restoring", "app__pre_restore_1"); got != c.state {
				t.Errorf("SwapState = %s, want %s", got, c.state)
			}
		})
	}
}

func TestSwapHalfwayMovesDataAndKeepsGuard(t *testing.T) {
	oldRoot, oldIDs, oldWrite := pgConfRoot, resolveClusterIDs, writeClusterFile
	pgConfRoot = t.TempDir()
	resolveClusterIDs = func(_, _ string) (int, int, error) { return -1, -1, nil }
	writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // test-owned path
	}
	oldAuto := writeAutoConf
	writeAutoConf = func(path string, data []byte, perm os.FileMode, _, _ string) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // test-owned path
	}
	t.Cleanup(func() {
		pgConfRoot, resolveClusterIDs, writeClusterFile = oldRoot, oldIDs, oldWrite
		writeAutoConf = oldAuto
	})
	dataRoot := t.TempDir()
	fromData := filepath.Join(dataRoot, "app__restoring")
	liveData := filepath.Join(dataRoot, "app")
	aside := "app__pre_restore_1"
	for _, dir := range []string{fromData, filepath.Join(dataRoot, aside), filepath.Join(pgConfRoot, "app", "conf.d")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pgConfRoot, "app", "postgresql.conf"),
		[]byte("data_directory = '"+liveData+"'\ninclude_dir = 'conf.d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := CopyGuard{Preloads: []string{"retained_lib"}, Settings: map[string]string{"cron.launch_active_jobs": "off"}}
	oldGuardWriter := writeCopyGuardFile
	writeCopyGuardFile = func(path string, data []byte, perm os.FileMode) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // test-owned path
	}
	t.Cleanup(func() { writeCopyGuardFile = oldGuardWriter })
	if _, err := InstallCopyGuard(fromData, guard); err != nil {
		t.Fatal(err)
	}
	autoConf := filepath.Join(fromData, "postgresql.auto.conf")
	raw, err := os.ReadFile(autoConf) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	raw = append([]byte("restore_command = 'archive-get --pg1-path="+fromData+"'\n"), raw...)
	if err := os.WriteFile(autoConf, raw, 0o600); err != nil { //nolint:gosec // test-owned path
		t.Fatal(err)
	}
	if state := SwapState(dataRoot, "app", "app__restoring", aside); state != SwapHalfway {
		t.Fatalf("pre-swap state = %s", state)
	}
	if err := Swap(context.Background(), engine.Shell{}, "restore.promote", nil, dataRoot, "app", "app__restoring", aside, 6001); err != nil {
		t.Fatal(err)
	}
	if state := SwapState(dataRoot, "app", "app__restoring", aside); state != SwapDone {
		t.Fatalf("post-swap state = %s", state)
	}
	if _, err := os.Stat(filepath.Join(liveData, "postgresql.auto.conf")); err != nil {
		t.Fatalf("the moved data lost its auto.conf: %v", err)
	}
	moved, err := os.ReadFile(filepath.Join(liveData, "postgresql.auto.conf")) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(moved), fromData) || !strings.Contains(string(moved), liveData) {
		t.Fatalf("the moved auto.conf was not repointed:\n%s", moved)
	}
	if !strings.Contains(string(moved), copyGuardBegin) {
		t.Fatalf("the swap lost the copy guard:\n%s", moved)
	}
	if _, err := os.Stat(filepath.Join(pgConfRoot, aside, "postgresql.conf")); err != nil {
		t.Fatalf("the aside registration was not written: %v", err)
	}
}

func TestConfMode(t *testing.T) {
	for name, want := range map[string]os.FileMode{
		"postgresql.conf":            0o644,
		"start.conf":                 0o644,
		"conf.d/99-materialise.conf": 0o644,
		"pg_hba.conf":                0o640,
		"pg_ident.conf":              0o640,
	} {
		if got := confMode(name); got != want {
			t.Errorf("confMode(%s) = %o, want %o", name, got, want)
		}
	}
}
