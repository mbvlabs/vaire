package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The prestart hook runs before the temporary cluster's first start, so a
// missing extension prerequisite stops the copy instead of letting PostgreSQL
// load a copied configuration without it.
func TestMaterialisedBeforeStartRunsBeforeStarting(t *testing.T) {
	called := false
	m := &Materialised{BeforeStart: func(context.Context) error {
		called = true
		return errors.New("missing extension prerequisite")
	}}
	err := m.start(context.Background(), UntilOpen)
	if !called {
		t.Fatal("BeforeStart was not called")
	}
	if err == nil || !strings.Contains(err.Error(), "missing extension prerequisite") {
		t.Fatalf("start error = %v", err)
	}
}
