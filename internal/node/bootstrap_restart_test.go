package node

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

type bootstrapRestartSpy struct{ calls int }

func (s *bootstrapRestartSpy) Restart(context.Context, string) (time.Duration, error) {
	s.calls++
	return 0, nil
}

func TestBootstrapCompletedTargetPreservesRestartConfirmation(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "unfinished"
		if completed {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}}}
			state := &provisionState{roles: true, installed: true}
			catalog := []engine.Step{
				provisionStep{id: "postgres-config-v1", state: state},
				provisionStep{id: "postgres-roles-v1", state: state},
				provisionAccess{provisionStep{id: "postgres-access-v1", state: state}},
				bootstrapGlobal{id: "pgbouncer-v1"},
			}
			path := filepath.Join(t.TempDir(), "journal.json")
			pending := "postgres-config-v1@app"
			journal := engine.Journal{
				Schema: engine.JournalSchema,
				Steps: map[string]engine.JournalEntry{
					pending: {
						Status:     engine.StatusCompleted,
						InputsHash: engine.InputsHash(pending, []byte(pending)),
					},
				},
				RestartPending: []string{pending},
			}
			if err := journal.Save(path); err != nil {
				t.Fatal(err)
			}
			spy := &bootstrapRestartSpy{}
			run := func(ctx context.Context, steps []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
				opts.JournalPath = path
				return engine.Apply(ctx, steps, cfg, rt, opts)
			}
			owner := &bootstrapOwnership{Targets: map[string]bool{"app": completed}}
			_, warnings, err := bootstrapDatabaseSteps(context.Background(), cfg, engine.Runtime{}, nil, catalog, run, spy, "", owner, func(target string) error {
				if completed {
					t.Fatal("completed ownership was reused")
				}
				owner.Targets[target] = true
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := engine.LoadJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			if completed {
				if spy.calls != 0 || !slices.Equal(got.RestartPending, []string{pending}) {
					t.Fatalf("completed replay restarted or cleared pending work: calls=%d pending=%v", spy.calls, got.RestartPending)
				}
				if !slices.ContainsFunc(warnings, func(w output.Warning) bool { return w.Code == "restart_pending" }) {
					t.Fatal("completed replay did not report the pending restart")
				}
			} else if spy.calls != 1 || len(got.RestartPending) != 0 {
				t.Fatalf("unfinished target lost provisioning authority: calls=%d pending=%v", spy.calls, got.RestartPending)
			}
			if slices.Contains(state.events, "grant") {
				t.Fatal("installed access was widened")
			}
		})
	}
}
