package cli

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/top"
)

func TestWithoutVaireSessions(t *testing.T) {
	in := []top.Session{
		{PID: 1, Role: "vaire_admin", App: ""},
		{PID: 2, Role: "app_app", App: "vaire-console"},
		{PID: 3, Role: "app_app", App: "myapp"},
		{PID: 4, Role: "app_migrate", App: "psql"},
		{PID: 5, Role: "postgres", App: "psql"},
		{PID: 6, Role: "pg_maintain", App: ""},
		{PID: 7, Role: "pg_signal_backend", App: ""},
	}
	out := withoutVaireSessions(in)
	if len(out) != 2 || out[0].PID != 3 || out[1].PID != 4 {
		t.Fatalf("kept %+v, want the application's two sessions", out)
	}
	if len(in) != 7 {
		t.Fatal("withoutVaireSessions changed its input")
	}
}
