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
		{PID: 5, Role: "postgres", App: "psql"},
		{PID: 6, Role: "pg_maintain", App: ""},
		{PID: 7, Role: "pg_signal_backend", App: ""},
	}
	out := withoutEruSessions(in)
	if len(out) != 2 || out[0].PID != 3 || out[1].PID != 4 {
		t.Fatalf("kept %+v, want the application's two sessions", out)
	}
	if len(in) != 7 {
		t.Fatal("withoutEruSessions changed its input")
	}
}
