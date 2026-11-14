package hostinfo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testUnit = "postgresql@18-test.service"
const testGroup = "/system.slice/" + testUnit

func cpuFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"system.slice", "system.slice/" + testUnit} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		"cgroup.controllers":                       "cpu memory",
		"cgroup.subtree_control":                   "cpu memory",
		"system.slice/cgroup.subtree_control":      "cpu",
		"system.slice/" + testUnit + "/cpu.idle":   "1",
		"system.slice/" + testUnit + "/cpu.weight": "100",
		"system.slice/" + testUnit + "/cpu.max":    "150000 100000",
		"system.slice/" + testUnit + "/cpu.stat":   "usage_usec 12345\nuser_usec 42\n",
	} {
		putCPU(t, dir, name, value)
	}
	return dir
}

func putCPU(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCPUScreen(t *testing.T) {
	dir := cpuFixture(t)
	if got := screenCPU(dir, testGroup, testUnit); got.Capability != CPUCapable {
		t.Fatal(got)
	}
	if got := screenCPU(dir, "", testUnit); got.Capability != CPUUnknown || got.Reason != "service_unrealized" {
		t.Fatal(got)
	}
	for _, invalid := range []string{"/system.slice/../" + testUnit, "/system.slice/other.service", "system.slice/" + testUnit} {
		if got := screenCPU(dir, invalid, testUnit); got.Capability != CPUUnknown {
			t.Fatalf("%q: %+v", invalid, got)
		}
	}
	putCPU(t, dir, "system.slice/cgroup.subtree_control", "memory")
	if got := screenCPU(dir, testGroup, testUnit); got.Capability != CPUUnknown {
		t.Fatal(got)
	}
	putCPU(t, dir, "system.slice/cgroup.subtree_control", "cpu")
	if err := os.Remove(filepath.Join(dir, "system.slice", testUnit, "cpu.idle")); err != nil {
		t.Fatal(err)
	}
	if got := screenCPU(dir, testGroup, testUnit); got.Capability != CPUUnsupported {
		t.Fatal(got)
	}
	putCPU(t, dir, "cgroup.controllers", "memory")
	if got := screenCPU(dir, testGroup, testUnit); got.Capability != CPUUnsupported {
		t.Fatal(got)
	}
}

func TestCPUScreenRejectsSymlink(t *testing.T) {
	dir := cpuFixture(t)
	group := filepath.Join(dir, "system.slice", testUnit)
	if err := os.Rename(group, group+".other"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(testUnit+".other", group); err != nil {
		t.Fatal(err)
	}
	if got := screenCPU(dir, testGroup, testUnit); got.Capability != CPUUnknown {
		t.Fatal(got)
	}
	if got := observeCPU(dir, testGroup, testUnit); got.Idle != nil {
		t.Fatal(got)
	}
}

func TestUnrealizedServiceScreensActualSystemdSlice(t *testing.T) {
	dir := cpuFixture(t)
	putCPU(t, dir, "system.slice/cgroup.controllers", "cpu memory")
	putCPU(t, dir, "system.slice/cpu.idle", "0")
	properties := map[string]string{
		testUnit + "/ControlGroup":  "",
		testUnit + "/Slice":         "system.slice",
		"system.slice/ControlGroup": "/system.slice",
	}
	property := func(_ context.Context, unit, name string) (string, error) {
		return properties[unit+"/"+name], nil
	}
	check := func() CPUScreen { return screenCPUService(context.Background(), dir, testUnit, property) }
	if got := check(); got.Capability != CPUCapable || got.Reason != "parent_capable_service_unrealized" {
		t.Fatal(got)
	}
	// The parent need not have enabled its child controller before creation.
	putCPU(t, dir, "system.slice/cgroup.subtree_control", "memory")
	if got := check(); got.Capability != CPUCapable {
		t.Fatal(got)
	}
	putCPU(t, dir, "system.slice/cgroup.controllers", "memory")
	if got := check(); got.Capability != CPUUnknown {
		t.Fatal(got)
	}
	putCPU(t, dir, "system.slice/cgroup.controllers", "cpu")
	properties["system.slice/ControlGroup"] = "/elsewhere/../system.slice"
	if got := check(); got.Capability != CPUUnknown {
		t.Fatal(got)
	}
	properties["system.slice/ControlGroup"] = ""
	if got := check(); got.Capability != CPUUnknown || got.Reason != "service_slice_unrealized" {
		t.Fatal(got)
	}
}

func TestCPUObservationAndValidation(t *testing.T) {
	dir := cpuFixture(t)
	got := observeCPU(dir, testGroup, testUnit)
	if got.Idle == nil || !*got.Idle || got.Weight == nil || *got.Weight != 100 ||
		got.UsageUsec == nil || *got.UsageUsec != 12345 || got.ObservedAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	pct := 150.0
	if err := ValidateCPUPolicy(CPUPolicy{Idle: true, QuotaPercent: &pct}, got); err != nil {
		t.Fatal(err)
	}
	pct = 151
	var mismatch *CPUPolicyMismatch
	if err := ValidateCPUPolicy(CPUPolicy{Idle: true, QuotaPercent: &pct}, got); !errors.As(err, &mismatch) || mismatch.Reason != "quota_mismatch" {
		t.Fatal(err)
	}
	putCPU(t, dir, "system.slice/"+testUnit+"/cpu.idle", "0")
	putCPU(t, dir, "system.slice/"+testUnit+"/cpu.max", "max 100000")
	got = observeCPU(dir, testGroup, testUnit)
	if err := ValidateCPUPolicy(CPUPolicy{Weight: 100}, got); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCPUPolicy(CPUPolicy{Weight: 200}, got); !errors.As(err, &mismatch) || mismatch.Reason != "weight_mismatch" {
		t.Fatal(err)
	}
	putCPU(t, dir, "system.slice/"+testUnit+"/cpu.stat", "usage_usec 0\nusage_usec 1")
	got = observeCPU(dir, testGroup, testUnit)
	if got.UsageUsec != nil || got.Reason == "" {
		t.Fatal(got)
	}
}

func TestCPUQuotaRoundingAndMalformed(t *testing.T) {
	for _, raw := range []string{"0 100000", "max 0", "-1 100", "1", "1 2 3"} {
		if _, err := parseServiceCPUMax(raw); err == nil {
			t.Fatal(raw)
		}
	}
	q, err := parseServiceCPUMax("33334 100001")
	if err != nil {
		t.Fatal(err)
	}
	idle := true
	pct := 33.333
	got := CPUObservation{Screen: CPUScreen{Capability: CPUCapable}, Idle: &idle, Quota: &q}
	if err := ValidateCPUPolicy(CPUPolicy{Idle: true, QuotaPercent: &pct}, got); err != nil {
		t.Fatal(err)
	}
	q.QuotaUsec += 2
	if err := ValidateCPUPolicy(CPUPolicy{Idle: true, QuotaPercent: &pct}, got); err == nil {
		t.Fatal("expected mismatch")
	}
}

func TestCPUUsagePercent(t *testing.T) {
	dir := cpuFixture(t)
	a := observeCPU(dir, testGroup, testUnit)
	a.invocation = "one"
	b := a
	b.ObservedAt = a.ObservedAt.Add(time.Second)
	n := *a.UsageUsec + 500000
	b.UsageUsec = &n
	if rate, ok := CPUUsagePercent(a, b); !ok || rate != 50 {
		t.Fatalf("%v %v", rate, ok)
	}
	b.invocation = "two"
	if _, ok := CPUUsagePercent(a, b); ok {
		t.Fatal("restart accepted")
	}
	b.invocation = "one"
	n = 0
	if _, ok := CPUUsagePercent(a, b); ok {
		t.Fatal("counter reset accepted")
	}
	n = *a.UsageUsec + 1
	b.ObservedAt = b.ObservedAt.Round(0)
	if _, ok := CPUUsagePercent(a, b); ok {
		t.Fatal("non-monotonic time accepted")
	}
}
