package node

import "testing"

func TestForkJobRetainsTypedRecoveryFailure(t *testing.T) {
	for _, reason := range []string{"recovery_requirements_unmet", "recovery_parameter_mismatch"} {
		t.Run(reason, func(t *testing.T) { runOwnedForkFixture(t, reason) })
	}
}
