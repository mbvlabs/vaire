package hostinfo

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Capability describes what a read-only screen can establish, not a promise
// that a future systemd apply will enforce the policy.
type Capability string

const (
	CPUCapable     Capability = "supported"
	CPUUnsupported Capability = "unsupported"
	CPUUnknown     Capability = "unknown"
)

type CPUScreen struct {
	Capability Capability
	Reason     string
}

// CPUQuota represents cpu.max. Unlimited is explicit; otherwise both values
// are positive microseconds.
type CPUQuota struct {
	Unlimited  bool
	QuotaUsec  uint64
	PeriodUsec uint64
}

// CPUObservation contains only observed values. Nil fields are unknown.
// Identity is used only to detect cgroup replacement between rate samples.
type CPUObservation struct {
	Screen     CPUScreen
	Idle       *bool
	Weight     *uint64
	Quota      *CPUQuota
	UsageUsec  *uint64
	ObservedAt time.Time
	Identity   os.FileInfo
	Reason     string
	invocation string
}

// CPUPolicy contains the desired policy; QuotaPercent nil means unlimited.
// Weight is the numeric normal-mode weight, never the derived idle value.
type CPUPolicy struct {
	Idle         bool
	Weight       uint64
	QuotaPercent *float64
}

// CPUPolicyMismatch indicates an unknown or disagreeing effective policy.
// Facts remain available in Observation; no raw systemd output is exposed.
type CPUPolicyMismatch struct {
	Reason      string
	Observation CPUObservation
}

func (e *CPUPolicyMismatch) Error() string { return "cpu policy not enforced: " + e.Reason }

var pgUnitPattern = regexp.MustCompile(`^postgresql@18-[a-zA-Z0-9_]+(?:\.service)?$`)
var sliceUnitPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+\.slice$`)
var invocationPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

// CPUServiceScreen resolves the actual systemd ControlGroup for a managed
// PostgreSQL service. For an unrealized service it screens the configured
// systemd slice's realized hierarchy; this is not future enforcement proof.
func CPUServiceScreen(ctx context.Context, unit string) CPUScreen {
	return screenCPUService(ctx, "/sys/fs/cgroup", unit, systemctlProperty)
}

func screenCPUService(ctx context.Context, dir, unit string, property func(context.Context, string, string) (string, error)) CPUScreen {
	if !pgUnitPattern.MatchString(unit) {
		return CPUScreen{CPUUnknown, "invalid_service"}
	}
	unit = canonicalUnit(unit)
	group, err := property(ctx, unit, "ControlGroup")
	if err != nil {
		return CPUScreen{CPUUnknown, "control_group_unavailable"}
	}
	if group == "" {
		slice, e := property(ctx, unit, "Slice")
		if e != nil || !sliceUnitPattern.MatchString(slice) {
			return CPUScreen{CPUUnknown, "service_slice_unavailable"}
		}
		parent, e := property(ctx, slice, "ControlGroup")
		if e != nil || parent == "" {
			return CPUScreen{CPUUnknown, "service_slice_unrealized"}
		}
		return screenCPUParent(dir, parent, slice)
	}
	return screenCPU(dir, group, unit)
}

// ObserveCPUService measures the realized service cgroup. It never accepts a
// caller-supplied cgroup path. Failures leave individual facts unknown.
func ObserveCPUService(ctx context.Context, unit string) CPUObservation {
	before, e := serviceInvocation(ctx, unit)
	group, err := serviceControlGroup(ctx, unit)
	if err != nil {
		return CPUObservation{Reason: "control_group_unavailable"}
	}
	obs := observeCPU("/sys/fs/cgroup", group, canonicalUnit(unit))
	after, e2 := serviceInvocation(ctx, unit)
	if e == nil && e2 == nil && before != "" && before == after {
		obs.invocation = before
	} else {
		obs.Screen = CPUScreen{CPUUnknown, "service_identity_unverified"}
		obs.Reason = "service_identity_unverified"
	}
	return obs
}

func canonicalUnit(unit string) string {
	if !strings.HasSuffix(unit, ".service") {
		return unit + ".service"
	}
	return unit
}

func serviceControlGroup(ctx context.Context, unit string) (string, error) {
	return serviceProperty(ctx, unit, "ControlGroup")
}

func serviceInvocation(ctx context.Context, unit string) (string, error) {
	value, err := serviceProperty(ctx, unit, "InvocationID")
	if err != nil || !invocationPattern.MatchString(value) {
		return "", errors.New("invocation unavailable")
	}
	return value, nil
}

func serviceProperty(ctx context.Context, unit, property string) (string, error) {
	if !pgUnitPattern.MatchString(unit) {
		return "", errors.New("invalid postgres service")
	}
	return systemctlProperty(ctx, canonicalUnit(unit), property)
}

func systemctlProperty(ctx context.Context, unit, property string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property="+property, "--value", "--", unit).Output() //nolint:gosec // systemd-derived validated unit and fixed property, no shell
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func groupPath(group, unit string) (string, error) {
	if !pgUnitPattern.MatchString(unit) || !strings.HasSuffix(unit, ".service") {
		return "", errors.New("invalid service")
	}
	return namedGroupPath(group, unit)
}

func namedGroupPath(group, unit string) (string, error) {
	if group == "" || !strings.HasPrefix(group, "/") || path.Clean(group) != group ||
		strings.ContainsAny(group, "\x00\r\n") || path.Base(group) != unit {
		return "", errors.New("invalid control group")
	}
	return strings.TrimPrefix(group, "/"), nil
}

// screenCPUParent verifies the actual configured slice, not a predicted leaf.
// CPU need not yet be enabled in that slice's subtree: systemd enables it
// when it creates a service requesting CPU control.
func screenCPUParent(dir, group, slice string) CPUScreen {
	if !sliceUnitPattern.MatchString(slice) {
		return CPUScreen{CPUUnknown, "invalid_service_slice"}
	}
	rel, err := namedGroupPath(group, slice)
	if err != nil {
		return CPUScreen{CPUUnknown, "invalid_slice_control_group"}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return CPUScreen{CPUUnknown, "cgroup_unavailable"}
	}
	defer func() { _ = root.Close() }()
	controllers, err := readCPUFile(root, "cgroup.controllers")
	if err != nil {
		return CPUScreen{CPUUnknown, "cgroup_v2_unavailable"}
	}
	if !containsCPU(controllers) {
		return CPUScreen{CPUUnsupported, "cpu_controller_unavailable"}
	}
	if !validGroupDirectories(root, rel) {
		return CPUScreen{CPUUnknown, "service_slice_unrealized"}
	}
	parent := "."
	for _, component := range strings.Split(rel, "/") {
		delegated, e := readCPUFile(root, path.Join(parent, "cgroup.subtree_control"))
		if e != nil || !containsCPU(delegated) {
			return CPUScreen{CPUUnknown, "cpu_hierarchy_unverified"}
		}
		parent = path.Join(parent, component)
	}
	controllers, err = readCPUFile(root, path.Join(rel, "cgroup.controllers"))
	if err != nil || !containsCPU(controllers) {
		return CPUScreen{CPUUnknown, "slice_cpu_controller_unverified"}
	}
	idle, err := readCPUFile(root, path.Join(rel, "cpu.idle"))
	if errors.Is(err, os.ErrNotExist) {
		return CPUScreen{CPUUnsupported, "cpu_idle_unavailable"}
	}
	if err != nil || (idle != "0" && idle != "1") {
		return CPUScreen{CPUUnknown, "cpu_idle_unverified"}
	}
	return CPUScreen{CPUCapable, "parent_capable_service_unrealized"}
}

func readCPUFile(root *os.Root, name string) (string, error) {
	b, err := root.ReadFile(name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func containsCPU(s string) bool {
	for _, f := range strings.Fields(s) {
		if f == "cpu" {
			return true
		}
	}
	return false
}

// cgroupfs directories are not symlinks. Reject them even when they would
// resolve inside the root, rather than treating another cgroup as the service.
func validGroupDirectories(root *os.Root, rel string) bool {
	current := "."
	for _, component := range strings.Split(rel, "/") {
		current = path.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func screenCPU(dir, group, unit string) CPUScreen {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return CPUScreen{CPUUnknown, "cgroup_unavailable"}
	}
	defer func() { _ = root.Close() }()
	controllers, err := readCPUFile(root, "cgroup.controllers")
	if err != nil {
		return CPUScreen{CPUUnknown, "cgroup_v2_unavailable"}
	}
	if !containsCPU(controllers) {
		return CPUScreen{CPUUnsupported, "cpu_controller_unavailable"}
	}
	if group == "" {
		return CPUScreen{CPUUnknown, "service_unrealized"}
	}
	rel, err := groupPath(group, unit)
	if err != nil {
		return CPUScreen{CPUUnknown, "invalid_control_group"}
	}
	if !validGroupDirectories(root, rel) {
		return CPUScreen{CPUUnknown, "service_cgroup_unavailable"}
	}
	// Every ancestor must currently delegate CPU to the next level. Systemd
	// may enable it on apply, so missing delegation is unknown, not unsupported.
	parent := "."
	for _, component := range strings.Split(rel, "/") {
		delegated, e := readCPUFile(root, path.Join(parent, "cgroup.subtree_control"))
		if e != nil || !containsCPU(delegated) {
			return CPUScreen{CPUUnknown, "cpu_hierarchy_unverified"}
		}
		parent = path.Join(parent, component)
	}
	idle, err := readCPUFile(root, path.Join(rel, "cpu.idle"))
	if errors.Is(err, os.ErrNotExist) {
		if _, e := root.Stat(rel); e == nil {
			return CPUScreen{CPUUnsupported, "cpu_idle_unavailable"}
		}
	}
	if err != nil || (idle != "0" && idle != "1") {
		return CPUScreen{CPUUnknown, "cpu_idle_unverified"}
	}
	return CPUScreen{CPUCapable, ""}
}

func observeCPU(dir, group, unit string) CPUObservation {
	obs := CPUObservation{Screen: screenCPU(dir, group, unit)}
	rel, err := groupPath(group, unit)
	if err != nil {
		obs.Reason = "invalid_control_group"
		return obs
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		obs.Reason = "cgroup_unavailable"
		return obs
	}
	defer func() { _ = root.Close() }()
	if !validGroupDirectories(root, rel) {
		obs.Reason = "service_cgroup_unavailable"
		return obs
	}
	obs.Identity, err = root.Stat(rel)
	if err != nil || !obs.Identity.IsDir() {
		obs.Identity = nil
		obs.Reason = "service_cgroup_unavailable"
		return obs
	}
	if s, e := readCPUFile(root, path.Join(rel, "cpu.idle")); e == nil && (s == "0" || s == "1") {
		v := s == "1"
		obs.Idle = &v
	}
	if s, e := readCPUFile(root, path.Join(rel, "cpu.weight")); e == nil {
		if n, e := strconv.ParseUint(s, 10, 64); e == nil && n >= 1 && n <= 10000 {
			obs.Weight = &n
		}
	}
	if s, e := readCPUFile(root, path.Join(rel, "cpu.max")); e == nil {
		if q, e := parseServiceCPUMax(s); e == nil {
			obs.Quota = &q
		}
	}
	if s, e := readCPUFile(root, path.Join(rel, "cpu.stat")); e == nil {
		if n, ok := cpuUsage(s); ok {
			obs.UsageUsec = &n
			obs.ObservedAt = time.Now()
		}
	}
	if obs.Idle == nil || obs.Weight == nil || obs.Quota == nil || obs.UsageUsec == nil {
		obs.Reason = "cpu_facts_unavailable"
	}
	return obs
}

func parseServiceCPUMax(s string) (CPUQuota, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return CPUQuota{}, errors.New("invalid cpu.max")
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || period == 0 {
		return CPUQuota{}, errors.New("invalid cpu.max period")
	}
	q := CPUQuota{PeriodUsec: period}
	if fields[0] == "max" {
		q.Unlimited = true
		return q, nil
	}
	q.QuotaUsec, err = strconv.ParseUint(fields[0], 10, 64)
	if err != nil || q.QuotaUsec == 0 {
		return CPUQuota{}, errors.New("invalid cpu.max quota")
	}
	return q, nil
}

func cpuUsage(s string) (uint64, bool) {
	var value uint64
	found := false
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "usage_usec" {
			if found || len(fields) != 2 {
				return 0, false
			}
			n, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			value, found = n, true
		}
	}
	return value, found
}

// ValidateCPUPolicy checks measured service settings, not ancestor caps or
// contention behavior. Quota comparison allows one microsecond rounding.
func ValidateCPUPolicy(want CPUPolicy, got CPUObservation) error {
	fail := func(reason string) error { return &CPUPolicyMismatch{reason, got} }
	if got.Reason == "service_identity_unverified" {
		return fail("effective_policy_unknown")
	}
	if got.Idle != nil && *got.Idle != want.Idle {
		return fail("idle_mismatch")
	}
	if !want.Idle && (want.Weight < 1 || want.Weight > 10000 || (got.Weight != nil && *got.Weight != want.Weight)) {
		return fail("weight_mismatch")
	}
	if got.Quota != nil && want.QuotaPercent == nil {
		if !got.Quota.Unlimited {
			return fail("quota_mismatch")
		}
	}
	if want.QuotaPercent != nil {
		pct := *want.QuotaPercent
		if math.IsNaN(pct) || math.IsInf(pct, 0) || pct <= 0 {
			return fail("invalid_requested_quota")
		}
		if got.Quota != nil && (got.Quota.Unlimited || got.Quota.PeriodUsec == 0 ||
			math.Abs(float64(got.Quota.QuotaUsec)-pct*float64(got.Quota.PeriodUsec)/100) > 1) {
			return fail("quota_mismatch")
		}
	}
	if got.Screen.Capability != CPUCapable || got.Idle == nil || got.Quota == nil || (!want.Idle && got.Weight == nil) {
		return fail("effective_policy_unknown")
	}
	return nil
}

// CPUUsagePercent returns the average percentage of one CPU between two
// observations. Both timestamps must carry monotonic clock readings.
func CPUUsagePercent(first, second CPUObservation) (float64, bool) {
	if first.UsageUsec == nil || second.UsageUsec == nil || first.Identity == nil || second.Identity == nil ||
		first.invocation == "" || first.invocation != second.invocation ||
		!os.SameFile(first.Identity, second.Identity) || *second.UsageUsec < *first.UsageUsec ||
		first.ObservedAt == first.ObservedAt.Round(0) || second.ObservedAt == second.ObservedAt.Round(0) { //nolint:staticcheck // Equality deliberately checks for the monotonic clock component.
		return 0, false
	}
	elapsed := second.ObservedAt.Sub(first.ObservedAt)
	if elapsed <= 0 {
		return 0, false
	}
	return float64(*second.UsageUsec-*first.UsageUsec) * float64(time.Microsecond) * 100 / float64(elapsed), true
}
