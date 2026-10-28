package diagnostics

import (
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/activity"
)

func TestOfflineQueryValidationUsesCollectionTime(t *testing.T) {
	bundle := missingHistoryBundle()
	bundle.CreatedAt = "2020-01-02T12:00:00Z"
	bundle.Metadata.CollectedAt = bundle.CreatedAt
	bundle.Query = &activity.Query{Since: "24h", Until: "2020-01-02T11:00:00Z", Limit: 100}
	path := filepath.Join(t.TempDir(), "incident.json")
	if err := Write(path, bundle); err != nil {
		t.Fatalf("saved activity filters were re-evaluated against today's clock: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Query == nil || *got.Query != *bundle.Query {
		t.Fatal("offline inspection changed the original activity filters")
	}
}
