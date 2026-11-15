package backup

import (
	"encoding/json"
	"testing"
)

func TestMaterialisationAttemptRecordRoundTrip(t *testing.T) {
	attempt := MaterialisationAttempt{
		JobID: "job", OperationID: "operation", InstanceID: "install", Kind: "drill",
		Source: "app", Target: "app__drill", Stanza: "app-stanza", SourcePort: 5432,
		TemporaryPort: 6001, DataPath: "/data/app__drill", ConfigPath: "/etc/postgresql/18/app__drill",
		Reason: "recovery_parameter_mismatch",
	}
	original := Records{
		Drilling: map[string]string{"app": attempt.Target}, DrillAttempts: map[string]MaterialisationAttempt{"app": attempt},
		Restores: map[string]Restore{"app": {ID: "job", Phase: PhaseFailed, Attempt: &attempt, MaterialisationPending: true}},
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decode(raw)
	if err != nil || got.DrillAttempts["app"] != attempt || got.Restores["app"].Attempt == nil ||
		*got.Restores["app"].Attempt != attempt || !got.Restores["app"].MaterialisationPending {
		t.Fatalf("attempt round trip: %+v %v", got, err)
	}
}
