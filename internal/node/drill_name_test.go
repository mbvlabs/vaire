package node

import (
	"strings"
	"testing"
)

func TestDrillTargetNameBoundary(t *testing.T) {
	if err := checkDrillTargetName(strings.Repeat("a", 56)); err != nil {
		t.Fatalf("56-character source: %v", err)
	}
	if err := checkDrillTargetName(strings.Repeat("a", 57)); err == nil {
		t.Fatal("57-character source produced an addressable drill target")
	}
}
