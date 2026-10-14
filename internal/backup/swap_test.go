package backup

import (
	"os"
	"path/filepath"
	"testing"
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
