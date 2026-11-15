package localenv

import (
	"context"
	"errors"
	"testing"
)

func TestUsqlDownloadContext(t *testing.T) {
	if ctx := usqlDownloadContext(nil); ctx == nil || ctx.Err() != nil { //nolint:staticcheck // SA1012: explicitly test the supported nil-context fallback.
		t.Fatalf("nil input produced an unusable download context: %v", ctx)
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := usqlDownloadContext(parent)
	if ctx != parent {
		t.Fatal("the caller's context was replaced")
	}
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("download context error = %v, want cancellation", ctx.Err())
	}
}
