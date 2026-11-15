package hostinfo

import (
	"context"
	"errors"
	"os"
	"path"
	"strconv"
	"strings"
)

// ControlLimit distinguishes a measured unlimited value from a numeric limit.
type ControlLimit struct {
	Unlimited bool
	Bytes     uint64
}

// NonCPUControls are systemd's loaded or the service cgroup's actual values.
type NonCPUControls struct {
	MemoryHigh ControlLimit
	MemoryMax  ControlLimit
	IOWeight   uint64
}

// NonCPUObservation is an identity-bound read of the service's loaded
// properties and actual cgroup controls. Unknown facts fail closed.
type NonCPUObservation struct {
	Known  bool
	Reason string
	Loaded NonCPUControls
	Actual NonCPUControls

	invocation string
	identity   os.FileInfo
}

// ObserveNonCPUService reads only the actual systemd-resolved service cgroup.
// It never accepts a caller-supplied path or changes any host property.
func ObserveNonCPUService(ctx context.Context, unit string) NonCPUObservation {
	return observeNonCPU(ctx, "/sys/fs/cgroup", unit, serviceProperty)
}

func parseControlLimit(s string) (ControlLimit, error) {
	s = strings.TrimSpace(s)
	if s == "max" || s == "infinity" || s == "18446744073709551615" {
		return ControlLimit{Unlimited: true}, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return ControlLimit{}, err
	}
	return ControlLimit{Bytes: n}, nil
}

func parseIOWeight(s string, loaded bool) (uint64, error) {
	s = strings.TrimSpace(s)
	if loaded && (s == "18446744073709551615" || s == "[not set]") {
		return 100, nil
	}
	if !loaded {
		found := false
		for _, line := range strings.Split(s, "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "default" {
				if found {
					return 0, errors.New("duplicate default weight")
				}
				s, found = f[1], true
			}
		}
		if !found {
			return 0, errors.New("missing default weight")
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n < 1 || n > 10000 {
		return 0, errors.New("invalid io weight")
	}
	return n, nil
}

// propertyFunc reads one systemd unit property.
type propertyFunc func(context.Context, string, string) (string, error)

func observeNonCPU(ctx context.Context, dir, unit string, property propertyFunc) NonCPUObservation {
	unknown := func(reason string) NonCPUObservation { return NonCPUObservation{Reason: reason} }
	if !pgUnitPattern.MatchString(unit) {
		return unknown("invalid_service")
	}
	unit = canonicalUnit(unit)
	before, group, reason := nonCPUIdentity(ctx, unit, property)
	if reason != "" {
		return unknown(reason)
	}
	rel, err := groupPath(group, unit)
	if err != nil {
		return unknown("invalid_control_group")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return unknown("cgroup_unavailable")
	}
	defer func() { _ = root.Close() }()
	identity, reason := realizedServiceCgroup(root, rel)
	if reason != "" {
		return unknown(reason)
	}
	loaded, actual, ok := readNonCPUControls(ctx, root, rel, unit, before, group, identity, property)
	if !ok {
		return unknown("non_cpu_facts_unavailable")
	}
	return NonCPUObservation{Known: true, Loaded: loaded, Actual: actual, invocation: before, identity: identity}
}

// nonCPUIdentity reads the service's invocation and control group. reason is
// "" only for an active service with a stable, realized invocation.
func nonCPUIdentity(ctx context.Context, unit string, property propertyFunc) (invocation, group, reason string) {
	before, e := property(ctx, unit, "InvocationID")
	active, e2 := property(ctx, unit, "ActiveState")
	group, e3 := property(ctx, unit, "ControlGroup")
	if e != nil || e2 != nil || e3 != nil || !invocationPattern.MatchString(before) || before == strings.Repeat("0", 32) || active != "active" {
		return "", "", "service_identity_unavailable"
	}
	return before, group, ""
}

// realizedServiceCgroup checks the service cgroup is realized under root and
// returns its identity, or the reason it is unavailable.
func realizedServiceCgroup(root *os.Root, rel string) (os.FileInfo, string) {
	if _, err := readCPUFile(root, "cgroup.controllers"); err != nil {
		return nil, "cgroup_v2_unavailable"
	}
	if !validGroupDirectories(root, rel) {
		return nil, "service_cgroup_unavailable"
	}
	identity, err := root.Stat(rel)
	if err != nil || !identity.IsDir() {
		return nil, "service_cgroup_unavailable"
	}
	return identity, ""
}

// readNonCPUControls reads the loaded and actual controls and verifies the
// service and its cgroup stayed the same. ok is false when any read failed or
// the identity changed.
func readNonCPUControls(ctx context.Context, root *os.Root, rel, unit, before, group string,
	identity os.FileInfo, property propertyFunc) (loaded, actual NonCPUControls, ok bool) {
	high, e1 := property(ctx, unit, "MemoryHigh")
	maximum, e2 := property(ctx, unit, "MemoryMax")
	weight, e3 := property(ctx, unit, "IOWeight")
	actualHigh, e4 := readCPUFile(root, path.Join(rel, "memory.high"))
	actualMax, e5 := readCPUFile(root, path.Join(rel, "memory.max"))
	actualWeight, e6 := readCPUFile(root, path.Join(rel, "io.weight"))
	after, e7 := property(ctx, unit, "InvocationID")
	identityAfter, e8 := root.Stat(rel)
	activeAfter, e9 := property(ctx, unit, "ActiveState")
	groupAfter, e10 := property(ctx, unit, "ControlGroup")
	finalID, e11 := property(ctx, unit, "InvocationID")
	finalIdentity, e12 := root.Stat(rel)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || e7 != nil || e8 != nil || e9 != nil || e10 != nil || e11 != nil || e12 != nil ||
		before != after || before != finalID || group != groupAfter || activeAfter != "active" ||
		!os.SameFile(identity, identityAfter) || !os.SameFile(identity, finalIdentity) {
		return NonCPUControls{}, NonCPUControls{}, false
	}
	var errs [6]error
	loaded.MemoryHigh, errs[0] = parseControlLimit(high)
	loaded.MemoryMax, errs[1] = parseControlLimit(maximum)
	loaded.IOWeight, errs[2] = parseIOWeight(weight, true)
	actual.MemoryHigh, errs[3] = parseControlLimit(actualHigh)
	actual.MemoryMax, errs[4] = parseControlLimit(actualMax)
	actual.IOWeight, errs[5] = parseIOWeight(actualWeight, false)
	for _, e := range errs {
		if e != nil {
			return NonCPUControls{}, NonCPUControls{}, false
		}
	}
	return loaded, actual, true
}

// ValidateNonCPUControls refuses unknown or differing loaded/actual controls.
func ValidateNonCPUControls(obs NonCPUObservation) error {
	if !obs.Known {
		return errors.New("non_cpu_controls_unknown")
	}
	if obs.Loaded != obs.Actual {
		return errors.New("non_cpu_controls_mismatch")
	}
	return nil
}

// ValidateNonCPUUnchanged also detects service/cgroup replacement and any
// loaded or actual non-CPU control changes between two observations.
func ValidateNonCPUUnchanged(before, after NonCPUObservation) error {
	if err := ValidateNonCPUControls(after); err != nil {
		return err
	}
	if err := ValidateNonCPUControls(before); err != nil {
		return err
	}
	if before.invocation == "" || before.invocation != after.invocation || before.identity == nil || after.identity == nil ||
		!os.SameFile(before.identity, after.identity) || before.Loaded != after.Loaded || before.Actual != after.Actual {
		return errors.New("non_cpu_controls_changed")
	}
	return nil
}
