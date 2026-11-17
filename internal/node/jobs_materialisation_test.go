package node

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// The bucket dump job is gone: a stale timer or an old RPC must be refused as
// an unknown job before any lock, host command or bucket operation.
func TestRemovedDumpJobIsUnknownBeforeEffects(t *testing.T) {
	_, _, err := RunJob(context.Background(), "db-dump", JobOptions{DB: "app"}, nil)
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != output.ExitUsage {
		t.Fatalf("db-dump job = %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	jobs, _ := details["jobs"].([]string)
	for _, job := range jobs {
		if job == "db-dump" {
			t.Fatalf("db-dump is still an accepted job: %v", jobs)
		}
	}
	for _, name := range jobNames {
		if name == "db-dump" {
			t.Fatal("jobNames still lists db-dump")
		}
	}
}
