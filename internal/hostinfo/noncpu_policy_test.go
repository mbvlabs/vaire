package hostinfo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNonCPUObservationLoadedActual(t *testing.T) {
	dir := cpuFixture(t)
	base := "system.slice/" + testUnit + "/"
	putCPU(t, dir, base+"memory.high", "1048576")
	putCPU(t, dir, base+"memory.max", "max")
	putCPU(t, dir, base+"io.weight", "default 100\n")
	values := map[string]string{
		"InvocationID": "0123456789abcdef0123456789abcdef", "ActiveState": "active",
		"ControlGroup": testGroup, "MemoryHigh": "1048576", "MemoryMax": "infinity", "IOWeight": "100",
	}
	property := func(_ context.Context, _, key string) (string, error) { return values[key], nil }
	first := observeNonCPU(context.Background(), dir, testUnit, property)
	if err := ValidateNonCPUControls(first); err != nil {
		t.Fatalf("%+v %v", first, err)
	}
	second := observeNonCPU(context.Background(), dir, testUnit, property)
	if err := ValidateNonCPUUnchanged(first, second); err != nil {
		t.Fatal(err)
	}
	values["MemoryHigh"] = "2097152"
	drift := observeNonCPU(context.Background(), dir, testUnit, property)
	if ValidateNonCPUControls(drift) == nil {
		t.Fatal("loaded/actual mismatch accepted")
	}
	values["MemoryHigh"] = "1048576"
	values["InvocationID"] = "1123456789abcdef0123456789abcdef"
	if ValidateNonCPUUnchanged(first, observeNonCPU(context.Background(), dir, testUnit, property)) == nil {
		t.Fatal("restart accepted")
	}
	if err := os.Remove(filepath.Join(dir, base, "io.weight")); err != nil {
		t.Fatal(err)
	}
	if ValidateNonCPUControls(observeNonCPU(context.Background(), dir, testUnit, property)) == nil {
		t.Fatal("missing control accepted")
	}
}

func TestPartialCPUDisagreementBeforeUnknown(t *testing.T) {
	idle := false
	err := ValidateCPUPolicy(CPUPolicy{Idle: true}, CPUObservation{Idle: &idle})
	var mismatch *CPUPolicyMismatch
	if !errors.As(err, &mismatch) || mismatch.Reason != "idle_mismatch" {
		t.Fatal(err)
	}
	err = ValidateCPUPolicy(CPUPolicy{Idle: true}, CPUObservation{Idle: &idle, Reason: "service_identity_unverified"})
	if !errors.As(err, &mismatch) || mismatch.Reason != "effective_policy_unknown" {
		t.Fatal(err)
	}
}
