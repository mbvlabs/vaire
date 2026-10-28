package activity

import (
	"testing"
	"time"
)

func TestValidateQueryUsesReferenceTime(t *testing.T) {
	collected := time.Date(2020, 1, 2, 12, 0, 0, 0, time.UTC)
	query := Query{Since: "24h", Until: "2020-01-02T11:00:00Z", Limit: 100}
	original := query
	if err := ValidateQueryAt(query, collected); err != nil {
		t.Fatalf("query valid at collection was rejected: %v", err)
	}
	if err := ValidateQueryAt(query, collected.Add(48*time.Hour)); err == nil {
		t.Fatal("test did not demonstrate that re-evaluating the window changes its meaning")
	}
	if query != original {
		t.Fatal("validation rewrote the saved relative filters")
	}
	if err := ValidateQueryAt(query, time.Time{}); err == nil {
		t.Fatal("accepted a missing collection-time reference")
	}
}
