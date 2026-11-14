package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryLogCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := newRecoveryLogCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: test file.
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(strings.Repeat("x", recoveryDiagnosticPollLimit+100)); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := c.read()
	if err != nil || len(first) != recoveryDiagnosticPollLimit || strings.Contains(first, "stale") {
		t.Fatalf("first read length %d: %v", len(first), err)
	}
	second, err := c.read()
	if err != nil || len(second) != recoveryDiagnosticContextLimit+100 {
		t.Fatalf("second read length %d: %v", len(second), err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.read(); err == nil {
		t.Fatal("rotation must not be treated as current evidence")
	}
}
