package backup

import "testing"

func TestLiveDatabase(t *testing.T) {
	for name, want := range map[string]string{
		"app":                              "app",
		"app__restoring":                   "app",
		"app__drill":                       "app",
		"app__pre_restore_20261014T085103": "app",
		"my_app__restoring":                "my_app",
	} {
		if got := LiveDatabase(name); got != want {
			t.Errorf("LiveDatabase(%q) = %q, want %q", name, got, want)
		}
	}
}
