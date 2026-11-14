package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func followRetentionFixture(t *testing.T, recover bool) (followOwner, followAttemptKey) {
	t.Helper()
	oldPath, oldInstance, oldWrite := backup.RecordsPath, followHostInstance, followRetentionWrite
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	followHostInstance = func() (string, error) { return "install", nil }
	t.Cleanup(func() { backup.RecordsPath, followHostInstance, followRetentionWrite = oldPath, oldInstance, oldWrite })
	spec := config.ClusterSpec{Name: "follower", Follows: "source", FollowGeneration: "generation"}
	owner := followOwner{Name: spec.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec}
	if recover {
		owner.Generation = ""
		owner.Awaiting = backup.Awaiting{Since: "2026-01-01", Stanza: "follower-abcdef"}
	}
	key := followAttemptKey{
		Job: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Operation: "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		SourceStanza: "source-abcdef", TemporaryPort: config.TemporaryPortMin,
	}
	if err := writeFollowOwnership(owner, followPhaseCopying); err != nil {
		t.Fatal(err)
	}
	return owner, key
}

// Parser-only metadata fixture: these IDs are not used to authorize effects.
func seedFollowRetentionBindingForParser(t *testing.T, owner followOwner, key followAttemptKey) {
	t.Helper()
	old := materialisationPostgresUID
	materialisationPostgresUID = func() (uint32, error) { return 123, nil }
	t.Cleanup(func() { materialisationPostgresUID = old })
	o, err := pendingFollowAttempt(owner, key)
	if err != nil {
		t.Fatal(err)
	}
	o.Attempt.ArtifactStage = materialisationRegistered
	o.Attempt.DataIdentity = materialisationFileID{Device: 1, Inode: 1, UID: 123}
	o.Attempt.ConfigIdentity = materialisationFileID{Device: 1, Inode: 2, UID: 123}
	if err := writeFollowRetention(o); err != nil {
		t.Fatal(err)
	}
}

func TestFollowRetentionAttempts(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		for _, reason := range []string{"recovery_requirements_unmet", "recovery_parameter_mismatch"} {
			t.Run(reason+map[bool]string{true: "_recover", false: "_follow"}[recovering], func(t *testing.T) {
				owner, key := followRetentionFixture(t, recovering)
				if err := beginFollowAttempt(owner, key); err != nil {
					t.Fatal(err)
				}
				o, owned, err := readFollowOwnership(owner)
				if err != nil || !owned || o.Phase != followPhaseCopying || o.Attempt.State != followAttemptPending {
					t.Fatalf("pending: %+v %v %v", o, owned, err)
				}
				if err := refuseIncompleteFollowAttempt(o); err == nil {
					t.Fatal("pending accepted")
				}
				if err := writeFollowOwnership(owner, followPhaseCopied); err == nil {
					t.Fatal("legacy writer erased pending")
				}
				if err := retainFollowAttempt(owner, key, reason); err != nil {
					t.Fatal(err)
				}
				o, owned, err = readFollowOwnership(owner)
				if err != nil || !owned || o.Phase != followPhaseCopying || o.Attempt.State != followAttemptRetained || o.Attempt.Reason != reason {
					t.Fatalf("retained: %+v %v %v", o, owned, err)
				}
				if refuseIncompleteFollowAttempt(o) == nil {
					t.Fatal("retained accepted")
				}
				if clearFollowOwnership(owner.Name) == nil {
					t.Fatal("legacy clear erased retained attempt")
				}
				if retireFollowOwnershipAfterDrop(owner.Name, owner.Source, owner.Generation) == nil {
					t.Fatal("legacy drop retirement erased retained attempt")
				}
				if completeFollowAttempt(owner, key) == nil {
					t.Fatal("retained attempt completed")
				}
			})
		}
	}
}

func TestFollowerDropRefusesUnverifiedAttempt(t *testing.T) {
	owner, key := followRetentionFixture(t, false)
	if err := beginFollowAttempt(owner, key); err != nil {
		t.Fatal(err)
	}
	if err := retainFollowAttempt(owner, key, "recovery_parameter_mismatch"); err != nil {
		t.Fatal(err)
	}
	if err := refuseUnverifiedFollowerDrop(owner.Name, owner.Spec); err == nil {
		t.Fatal("retained attempt accepted for name-only drop")
	}
	own, found, err := readFollowOwnership(owner)
	if err != nil || !found || own.Attempt.State != followAttemptRetained {
		t.Fatalf("retained record changed: %+v %v", own, err)
	}
}

func TestFollowerRuntimeRefusesRetainedOrForeignAttemptBeforeEffects(t *testing.T) {
	for _, recover := range []bool{false, true} {
		for _, foreign := range []bool{false, true} {
			t.Run(fmt.Sprintf("recover=%t/foreign=%t", recover, foreign), func(t *testing.T) {
				owner, key := followRetentionFixture(t, recover)
				if err := beginFollowAttempt(owner, key); err != nil {
					t.Fatal(err)
				}
				if err := retainFollowAttempt(owner, key, "recovery_parameter_mismatch"); err != nil {
					t.Fatal(err)
				}
				path, err := followOwnershipPath(owner.Name)
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path) //nolint:gosec // test-owned path
				if err != nil {
					t.Fatal(err)
				}
				if foreign {
					followHostInstance = func() (string, error) { return "different-install", nil }
				}
				cfg := config.DefaultCluster()
				cfg.Server.DataRoot = t.TempDir()
				source := config.ClusterSpec{Name: owner.Source, Port: 5433, Stanza: "source-abcdef", Size: "small"}
				cfg.Clusters = []config.ClusterSpec{source, owner.Spec}
				if recover {
					if err := backup.UpdateRecords(func(s *backup.Records) error {
						s.AwaitingRecovery[owner.Name] = owner.Awaiting
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				oldSlots, oldNew := followerSlotOps, newFollowerCopy
				t.Cleanup(func() { followerSlotOps, newFollowerCopy = oldSlots, oldNew })
				effects := 0
				followerSlotOps.inspect = func(context.Context, config.ClusterSpec, string) (followerSlotState, error) {
					effects++
					return followerSlotState{}, nil
				}
				newFollowerCopy = func(string, string, string, int, engine.Shell, string, engine.Reporter) (followerCopy, error) {
					effects++
					return nil, errors.New("unexpected constructor")
				}
				env := &jobEnv{ID: key.Job, OperationID: key.Operation, Job: "db-follow", Cluster: cfg}
				if err := materialiseFollower(context.Background(), env, cfg, source, owner.Spec, followFromStanza, recover); err == nil {
					t.Fatal("retained attempt accepted by build")
				}
				if !recover {
					recs, err := backup.LoadRecords()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := followRetryable(cfg, recs, owner.Spec); err == nil {
						t.Fatal("retained attempt accepted for public retry")
					}
				}
				after, err := os.ReadFile(path) //nolint:gosec // test-owned path
				if err != nil || string(before) != string(after) || effects != 0 {
					t.Fatalf("record or host effects changed: %v effects=%d", err, effects)
				}
			})
		}
	}
}

func TestFollowRetentionCompletionAndLegacy(t *testing.T) {
	owner, key := followRetentionFixture(t, false)
	o, owned, err := readFollowOwnership(owner)
	if err != nil || !owned || o.Attempt != nil || refuseIncompleteFollowAttempt(o) != nil {
		t.Fatalf("legacy: %+v %v %v", o, owned, err)
	}
	if err := beginFollowAttempt(owner, key); err != nil {
		t.Fatal(err)
	}
	foreign := owner
	foreign.Generation = "another-generation"
	if err := completeFollowAttempt(foreign, key); err == nil {
		t.Fatal("different follow generation accepted")
	}
	seedFollowRetentionBindingForParser(t, owner, key)
	if err := completeFollowAttempt(owner, key); err != nil {
		t.Fatal(err)
	}
	o, owned, err = readFollowOwnership(owner)
	if err != nil || !owned || o.Phase != followPhaseCopied || o.Attempt.State != followAttemptCompleted || refuseIncompleteFollowAttempt(o) != nil {
		t.Fatalf("completed: %+v %v %v", o, owned, err)
	}
}

func TestFollowRetentionRejectsForeignIdentityAndFailureWrite(t *testing.T) {
	owner, key := followRetentionFixture(t, true)
	if err := beginFollowAttempt(owner, key); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*followOwner, *followAttemptKey){
		"target":        func(o *followOwner, _ *followAttemptKey) { o.Name = "another" },
		"source":        func(o *followOwner, _ *followAttemptKey) { o.Source = "another" },
		"awaiting":      func(o *followOwner, _ *followAttemptKey) { o.Awaiting.Since = "other" },
		"stanza":        func(o *followOwner, _ *followAttemptKey) { o.Awaiting.Stanza = "other-abcdef" },
		"job":           func(_ *followOwner, k *followAttemptKey) { k.Job = "01ARZ3NDEKTSV4RRFFQ69G5FAX" },
		"operation":     func(_ *followOwner, k *followAttemptKey) { k.Operation = "01ARZ3NDEKTSV4RRFFQ69G5FAX" },
		"source stanza": func(_ *followOwner, k *followAttemptKey) { k.SourceStanza = "other-abcdef" },
		"port":          func(_ *followOwner, k *followAttemptKey) { k.TemporaryPort++ },
	} {
		t.Run(name, func(t *testing.T) {
			foreign, foreignKey := owner, key
			mutate(&foreign, &foreignKey)
			if err := retainFollowAttempt(foreign, foreignKey, "recovery_parameter_mismatch"); err == nil {
				t.Fatal("foreign attempt accepted")
			}
		})
	}
	followHostInstance = func() (string, error) { return "other-install", nil }
	if err := retainFollowAttempt(owner, key, "recovery_parameter_mismatch"); err == nil {
		t.Fatal("foreign install accepted")
	}
	followHostInstance = func() (string, error) { return "install", nil }
	if err := retainFollowAttempt(owner, key, "other"); err == nil {
		t.Fatal("unsafe reason accepted")
	}
	followRetentionWrite = func(string, []byte, os.FileMode) error { return errors.New("injected write failure") }
	if err := retainFollowAttempt(owner, key, "recovery_parameter_mismatch"); err == nil {
		t.Fatal("write failure ignored")
	}
	o, owned, err := readFollowOwnership(owner)
	if err != nil || !owned || o.Attempt.State != followAttemptPending || refuseIncompleteFollowAttempt(o) == nil {
		t.Fatalf("failed write did not retain blocked pending: %+v %v %v", o, owned, err)
	}
}

func TestFollowRetentionInvalidAttemptFailsClosed(t *testing.T) {
	owner, _ := followRetentionFixture(t, false)
	p, err := followOwnershipPath(owner.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"database":"follower","source":"source","instance":"install","operation":"follow","generation":"generation","phase":"copying","attempt":null}`,
		`{"database":"follower","source":"source","instance":"install","operation":"follow","generation":"generation","phase":"copying","attempt":{"state":"pending"}}`,
	} {
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		} //nolint:gosec // temporary test record
		if _, _, err := readFollowOwnership(owner); err == nil {
			t.Fatal("accepted corrupt attempt")
		}
	}
}

func TestFollowRetentionCaseEquivalentAttempt(t *testing.T) {
	for _, state := range []string{"pending", "retained", "completed", "malformed-completed", "null", "duplicate"} {
		for _, name := range []string{"Attempt", "AtTeMpT"} {
			t.Run(state+"-"+name, func(t *testing.T) {
				owner, key := followRetentionFixture(t, false)
				if err := beginFollowAttempt(owner, key); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "retained":
					if err := retainFollowAttempt(owner, key, "recovery_parameter_mismatch"); err != nil {
						t.Fatal(err)
					}
				case "completed", "malformed-completed":
					seedFollowRetentionBindingForParser(t, owner, key)
					if err := completeFollowAttempt(owner, key); err != nil {
						t.Fatal(err)
					}
				}
				path, err := followOwnershipPath(owner.Name)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path) //nolint:gosec // test-owned record
				if err != nil {
					t.Fatal(err)
				}
				raw := strings.Replace(string(data), `"attempt":`, `"`+name+`":`, 1)
				switch state {
				case "malformed-completed":
					raw = strings.Replace(raw, `"job":"`+key.Job+`"`, `"job":""`, 1)
				case "null":
					raw = `{"database":"follower","source":"source","instance":"install","operation":"follow","generation":"generation","phase":"copying","` + name + `":null}`
				case "duplicate":
					raw = strings.TrimSpace(raw)
					raw = strings.TrimSuffix(raw, "}") + `,"attempt":null}`
				}
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil { //nolint:gosec // test-owned record
					t.Fatal(err)
				}
				own, owned, readErr := readFollowOwnership(owner)
				switch state {
				case "completed":
					if readErr != nil || !owned || refuseIncompleteFollowAttempt(own) != nil {
						t.Fatalf("valid completed rejected: %+v %v %v", own, owned, readErr)
					}
					if err := clearFollowOwnership(owner.Name); err != nil {
						t.Fatal(err)
					}
					return
				case "pending", "retained":
					if readErr != nil || !owned || refuseIncompleteFollowAttempt(own) == nil {
						t.Fatalf("incomplete not blocked: %+v %v %v", own, owned, readErr)
					}
				default:
					if readErr == nil {
						t.Fatal("invalid record accepted")
					}
				}
				if clearFollowOwnership(owner.Name) == nil ||
					retireFollowOwnershipAfterDrop(owner.Name, owner.Source, owner.Generation) == nil ||
					writeFollowOwnership(owner, followPhaseCopying) == nil {
					t.Fatal("invalid or incomplete record was cleared, retired or overwritten")
				}
			})
		}
	}
	invalid := followOwnership{Phase: followPhaseCopied, Attempt: &followRetentionAttempt{State: followAttemptCompleted}}
	if refuseIncompleteFollowAttempt(invalid) == nil {
		t.Fatal("malformed completed value bypassed predicate validation")
	}
}
