package activity

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// benchStore is an isolated store for local timing. It is not a VPS
// qualification: the numbers include this developer filesystem's fsync cost.
func benchStore(b *testing.B) *Store {
	b.Helper()
	return &Store{
		Dir:          filepath.Join(b.TempDir(), "activity"),
		MaxBytes:     32 << 20,
		SegmentBytes: 1 << 20,
		Retention:    14 * 24 * time.Hour,
	}
}

// BenchmarkAppend measures one checked append: segment write+fsync, retention,
// state write+fsync and directory fsync.
func BenchmarkAppend(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	ev := Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeSucceeded, OperationID: "op-bench"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Append(ctx, ev); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
}

// BenchmarkQuerySmallPage measures a bounded read of a warm store with a small
// page.
func BenchmarkQuerySmallPage(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		if _, err := s.Append(ctx, Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeSucceeded}); err != nil {
			b.Fatalf("setup Append: %v", err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Query(ctx, Query{Limit: 20}); err != nil {
			b.Fatalf("Query: %v", err)
		}
	}
}
