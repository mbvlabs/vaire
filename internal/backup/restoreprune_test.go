package backup

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
)

// TestPruneForRestoreRetiresInterruptedUnfollows rebuilds a master from a real
// bundle for each interrupted unfollow state: after the continuation was
// recorded but before cluster.toml was made independent, after independence
// before privilege settlement, and after settlement before retirement. The
// rebuilt records must carry no lost-host unfollow continuation or routing
// authority, while extension and stanza history and the new awaiting-recovery
// intent stay.
func TestPruneForRestoreRetiresInterruptedUnfollows(t *testing.T) {
	states := []struct {
		name     string
		settled  bool
		phase    string
		awaiting string
		cluster  string
	}{
		{
			name:    "before independent intent saved",
			phase:   "prepared",
			cluster: "name = \"prod\"\n\n[[clusters]]\nname = \"app\"\n\n[[clusters]]\nname = \"app_follower\"\nfollows = \"app\"\n",
		},
		{
			name:     "after independence saved before settlement",
			phase:    "promoted",
			awaiting: "app_follower-f00d",
			cluster:  "name = \"prod\"\n\n[[clusters]]\nname = \"app\"\n\n[[clusters]]\nname = \"app_follower\"\nstanza = \"app_follower-f00d\"\n",
		},
		{
			name:     "after settlement before retirement",
			settled:  true,
			phase:    "routing",
			awaiting: "app_follower-f00d",
			cluster:  "name = \"prod\"\n\n[[clusters]]\nname = \"app\"\n\n[[clusters]]\nname = \"app_follower\"\nstanza = \"app_follower-f00d\"\n",
		},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			tempRecords(t)
			now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			newStanza := "app_follower-f00d"
			r, _ := decode(nil)
			r.RecordExtensionRequirements(newStanza, "app_follower", []catalog.Extension{ext("pg_trgm")}, now)
			r.DroppedStanzas = append(r.DroppedStanzas, DroppedStanza{
				Database: "old", Stanza: "old-abc123", DroppedAt: now.Format(time.RFC3339), ExpiresAt: now.AddDate(0, 0, 14).Format(time.RFC3339),
			})
			r.Timelines["app_follower"] = TimelineSwitch{Stanza: newStanza, Timeline: "3"}
			r.AwaitingRecovery["app_follower"] = Awaiting{Stanza: "lost-stanza", Since: "2026-01-01T00:00:00Z"}
			r.ExtensionGuardHandoffs["app_follower"] = GuardHandoff{
				Instance: "lost-instance", Stanza: newStanza,
				Values: map[string]string{"hist.worker": "off"},
			}
			r.Unfollowing["app_follower"] = Unfollow{
				Source: "app", Stanza: newStanza, StartedAt: now.Format(time.RFC3339),
				OperationID: "op-lost", Settled: state.settled,
				Cron: &UnfollowCron{
					MetadataDB: "app", MetadataOID: "42", OldDB: "app", NewDB: "app_follower",
					Deactivated: true, Retargeted: true,
				},
			}
			r.Activation["app_follower"] = ActivationFence{
				OperationID: "op-lost", Kind: "unfollow", Database: "app_follower",
				Instance: "lost-instance", TargetStanza: newStanza, OldStanza: newStanza, NewStanza: newStanza,
				Registration: "app_follower:5434", Ownership: "lost-generation", Port: 5434,
				DataDir: "/var/lib/postgresql/18/app_follower", ConfDir: "/etc/postgresql/18/app_follower",
				Phase: state.phase, CreatedAt: now.Format(time.RFC3339),
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}

			// Build and read back a real master bundle, as the bucket and the
			// rebuilt node do.
			tarBytes, _, err := masterbackup.Build(masterbackup.Meta{
				Server: "prod", Node: "db1", Epoch: 1, CreatedAt: now, VaireVersion: "dev",
			}, []masterbackup.File{
				masterbackup.NewFile(masterbackup.ClusterPath, "root", "root", 0o640, []byte(state.cluster)),
				masterbackup.NewFile(masterbackup.RecordsPath, "root", "root", 0o600, raw),
			})
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := masterbackup.ReadTar(tarBytes)
			if err != nil {
				t.Fatal(err)
			}
			bundled, ok := bundle.File(masterbackup.RecordsPath)
			if !ok {
				t.Fatal("the bundle lost records.json")
			}

			// Prune and place records.json, then read it as the rebuilt node does.
			prunedRaw, pruned, err := PruneForRestore(bundled.Data, []AwaitingDatabase{{
				Name: "app_follower", Stanza: state.awaiting,
			}}, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.WriteFileAtomic(RecordsPath, prunedRaw, RecordsMode); err != nil {
				t.Fatal(err)
			}
			got, err := LoadRecords()
			if err != nil {
				t.Fatal(err)
			}

			if len(got.Unfollowing) != 0 {
				t.Fatalf("the rebuilt host inherited lost-unfollow continuations: %+v", got.Unfollowing)
			}
			if len(got.Activation) != 0 {
				t.Fatalf("the rebuilt host inherited lost activation authority: %+v", got.Activation)
			}
			if len(got.ExtensionGuardHandoffs) != 0 {
				t.Fatalf("the rebuilt host inherited lost guard handoffs: %+v", got.ExtensionGuardHandoffs)
			}
			if req, ok := got.ExtensionRequirementsFor(newStanza); !ok || len(req.Requirements) != 1 || req.Requirements[0].Name != "pg_trgm" {
				t.Fatalf("the rebuild dropped retained extension history: %+v", req)
			}
			if len(got.DroppedStanzas) != 1 || got.DroppedStanzas[0].Stanza != "old-abc123" {
				t.Fatalf("the rebuild dropped retained stanza history: %+v", got.DroppedStanzas)
			}
			if got.Timelines["app_follower"].Timeline != "3" {
				t.Fatalf("the rebuild dropped the timeline switch: %+v", got.Timelines)
			}
			if aw, ok := got.AwaitingRecovery["app_follower"]; !ok || aw.Stanza != state.awaiting || aw.Since != now.Format(time.RFC3339) {
				t.Fatalf("the rebuild lost its own awaiting-recovery intent: %+v", got.AwaitingRecovery)
			}
			// beginUnfollow resumes only an existing record. With the map
			// empty, a new unfollow by this name starts with its own operation
			// and no cron checkpoint.
			wantPruned := map[PrunedRecord]bool{
				{Kind: "activation_fence", Database: "app_follower", Name: "op-lost"}: true,
				{Kind: "unfollow", Database: "app_follower", Name: "op-lost"}:         true,
			}
			if len(pruned) != len(wantPruned) {
				t.Fatalf("pruned %+v, want the lost fence and unfollow", pruned)
			}
			for _, p := range pruned {
				if !wantPruned[p] {
					t.Fatalf("unexpected pruned record %+v", p)
				}
			}
		})
	}
}
