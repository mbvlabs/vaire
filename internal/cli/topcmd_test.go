package cli

import (
	"testing"

	"github.com/mbvlabs/eru/internal/cli/top"
)

func TestWithoutEruSessions(t *testing.T) {
	in := []top.Session{
		{PID: 1, Role: "eru_admin", App: ""},
		{PID: 2, Role: "app_app", App: "eru-console"},
		{PID: 3, Role: "app_app", App: "myapp"},
		{PID: 4, Role: "app_migrate", App: "psql"},
	}
	out := withoutEruSessions(in)
	if len(out) != 2 || out[0].PID != 3 || out[1].PID != 4 {
		t.Fatalf("kept %+v, want the application's two sessions", out)
	}
	if len(in) != 4 {
		t.Fatal("withoutEruSessions changed its input")
	}
}
