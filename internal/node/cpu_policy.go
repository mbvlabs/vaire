package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

var checkSpareCPU = steps.CheckSpareCPU
var checkCPUOnlyResources = steps.CheckCPUOnlyResources
var cpuOnlyCatalog = steps.CPUOnlyCatalog
var cpuJournalPath = func() string { return engine.JournalPath }
var invalidateCPUCompletion = engine.InvalidateStepCompletion

func rejectNullCPUParams(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"cpu_mode", "cpu_quota"} {
		if value, ok := fields[key]; ok && strings.TrimSpace(string(value)) == "null" {
			return output.NewError(output.CodeValidation, key+" must be a string", output.ExitConfig, "")
		}
	}
	return nil
}

func validateCPUParams(mode, quota *string) error {
	if mode != nil && *mode != "normal" && *mode != "spare" {
		return output.NewError(output.CodeValidation, "CPU mode must be normal or spare", output.ExitConfig, "")
	}
	if quota != nil && *quota != "none" {
		if *quota == "" {
			return output.NewError(output.CodeValidation, "CPU quota must be a percentage or none", output.ExitConfig, "")
		}
		if _, err := config.ResolveResources(config.ClusterSpec{Size: "small", CPUQuota: *quota}, "small"); err != nil {
			return config.ValidationError(err)
		}
	}
	return nil
}

// cpuProofTargets includes effective changes and explicitly supplied dotted
// CPU keys, including same-value requests that still require verification.
func cpuProofTargets(old, next config.Cluster, dotted map[string]string) []string {
	explicit := map[string]bool{}
	for key := range dotted {
		parts := strings.Split(key, ".")
		if len(parts) == 3 && parts[0] == "clusters" &&
			(parts[2] == "cpu_mode" || parts[2] == "cpu_quota" || parts[2] == "cpu_weight") {
			explicit[parts[1]] = true
		}
	}
	var targets []string
	for _, spec := range next.Clusters {
		prev, exists := old.ClusterByName(spec.Name)
		if !exists {
			continue
		}
		before, e1 := old.Resources(prev)
		after, e2 := next.Resources(spec)
		changed := prev.CPUMode != spec.CPUMode || prev.CPUQuota != spec.CPUQuota || prev.CPUWeight != spec.CPUWeight ||
			e1 != nil || e2 != nil || before.CPUWeight != after.CPUWeight || before.CPUQuota != after.CPUQuota || before.CPUMode != after.CPUMode
		if explicit[spec.Name] || changed {
			targets = append(targets, spec.Name)
		}
	}
	return targets
}

func invalidateCPUProofs(targets []string) error {
	seen := map[string]bool{}
	for _, name := range targets {
		if seen[name] {
			continue
		}
		seen[name] = true
		if err := invalidateCPUCompletion(cpuJournalPath(), engine.InstanceID("postgres-cluster-v1", name)); err != nil {
			return err
		}
	}
	return nil
}

// checkCPUProposal screens changed spare policies before any configuration
// persistence or protection synchronization.
func checkCPUProposal(ctx context.Context, old, next config.Cluster) error {
	return checkCPUProposalWithRequests(ctx, old, next, nil)
}

func checkCPUProposalWithRequests(ctx context.Context, old, next config.Cluster, dotted map[string]string) error {
	explicit := map[string]bool{}
	for key := range dotted {
		parts := strings.Split(key, ".")
		if len(parts) == 3 && parts[0] == "clusters" &&
			(parts[2] == "cpu_mode" || parts[2] == "cpu_quota" || parts[2] == "cpu_weight") {
			explicit[parts[1]] = true
		}
	}
	for _, spec := range next.Clusters {
		if spec.CPUMode != "spare" {
			continue
		}
		previous, exists := old.ClusterByName(spec.Name)
		if exists {
			before, e1 := old.Resources(previous)
			after, e2 := next.Resources(spec)
			if !explicit[spec.Name] && e1 == nil && e2 == nil && previous.CPUMode == spec.CPUMode && previous.CPUQuota == spec.CPUQuota &&
				previous.CPUWeight == spec.CPUWeight && before.CPUWeight == after.CPUWeight {
				continue
			}
		}
		if err := checkSpareCPU(ctx, spec.Name, spec.CPUMode); err != nil {
			return err
		}
	}
	return nil
}

func cpuPolicySaved(err error) error {
	var cli *output.CLIError
	if !errors.As(err, &cli) {
		return err
	}
	details, ok := cli.Details.(map[string]any)
	if !ok || details["reason"] != "cpu_policy_not_enforced" {
		return err
	}
	copied := *cli
	copiedDetails := make(map[string]any, len(details))
	for key, value := range details {
		copiedDetails[key] = value
	}
	copiedDetails["desired_saved"] = true
	copied.Details = copiedDetails
	return &copied
}
