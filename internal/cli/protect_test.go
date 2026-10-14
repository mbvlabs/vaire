package cli

import (
	"errors"
	"testing"

	"github.com/mbvlabs/eru/internal/cli/output"
)

func TestGroupSecret(t *testing.T) {
	if got := groupSecret("JBSWY3DPEHPK3PXPJB"); got != "JBSW Y3DP EHPK 3PXP JB" {
		t.Fatalf("got %q", got)
	}
	if got := groupSecret("ABCD"); got != "ABCD" {
		t.Fatalf("got %q", got)
	}
}

func TestRetryableCode(t *testing.T) {
	rejected := func(reason string) error {
		return output.NewError(output.CodeProtectCodeRejected, "x", output.ExitUnsafe, "").WithDetails(map[string]any{"reason": reason})
	}
	for reason, want := range map[string]bool{"code_invalid": true, "code_reused": true, "locked": false, "code_required": false} {
		if got := retryableCode(rejected(reason)); got != want {
			t.Fatalf("%s: got %v, want %v", reason, got, want)
		}
	}
	if retryableCode(errors.New("boom")) {
		t.Fatal("a plain error is not retryable")
	}
}
