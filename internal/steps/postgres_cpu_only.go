package steps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/render"
)

const cpuOverlayName = "20-vaire-cpu.conf"

var cpuOverlayPattern = regexp.MustCompile(`^# managed by vaire - cpu-only overlay\n\[Service\]\nCPUWeight=(idle|[1-9][0-9]{0,4})\nCPUQuota=([1-9][0-9]{0,4}%|)\n$`)

func validCPUOverlay(raw []byte) bool {
	match := cpuOverlayPattern.FindSubmatch(raw)
	if match == nil {
		return false
	}
	if string(match[1]) == "idle" {
		return true
	}
	weight, err := strconv.Atoi(string(match[1]))
	return err == nil && weight <= 10000
}

// CPUOnlyCatalog substitutes the existing cluster template with an explicit
// CPU-only variant. Callers must select the cluster step for the target.
func CPUOnlyCatalog(catalog []engine.Step) ([]engine.Step, error) {
	out := append([]engine.Step(nil), catalog...)
	found := 0
	for i, step := range out {
		if _, ok := step.(postgresCluster); ok {
			out[i] = postgresCPUOnly{}
			found++
		}
	}
	if found != 1 {
		return nil, errors.New("expected exactly one postgres cluster template")
	}
	return out, nil
}

// CPUOnlyOptions selects exactly one CPU-only cluster instance and never
// settles or performs existing restart obligations.
func CPUOnlyOptions(cluster string) engine.Options {
	return engine.Options{
		DB: cluster, Only: []string{engine.InstanceID(postgresClusterID, cluster)},
		LeaveRestartsPending: true,
	}
}

type postgresCPUOnly struct{ clusterStep }

func (s postgresCPUOnly) ID() string { return s.id(postgresClusterID) }
func (postgresCPUOnly) Instance(cluster string) engine.Step {
	return postgresCPUOnly{clusterStep{cluster: cluster}}
}
func (s postgresCPUOnly) Describe(config.Cluster) string {
	return "apply only the CPU policy for " + s.cluster
}
func (postgresCPUOnly) Force(config.Cluster, engine.Runtime) string { return "cpu_only_explicit" }
func (s postgresCPUOnly) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil, err
	}
	res, err := cfg.Resources(spec)
	if err != nil {
		return nil, err
	}
	return canonical(map[string]any{"scope": "cpu-only", "target": spec.Name, "cpu_dropin": string(render.PGCPUOnlyDropin(res))})
}
func (postgresCPUOnly) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return incomplete("explicit CPU policy verification required"), nil
}

// CheckCPUOnlyResources screens a proposed target before saving it. It
// compares systemd's currently loaded non-CPU values with actual cgroup
// controls, independently of the proposed or saved memory configuration.
func CheckCPUOnlyResources(ctx context.Context, cfg config.Cluster, cluster string) error {
	_, err := cpuOnlyAdmission(ctx, cfg, cluster, Probe, hostinfo.ObserveNonCPUService, hostinfo.CPUServiceScreen, checkCPUOverlayTarget)
	return err
}

func cpuOnlyAdmission(ctx context.Context, cfg config.Cluster, cluster string,
	probe func(context.Context, string, ...string) (string, error),
	observe func(context.Context, string) hostinfo.NonCPUObservation,
	screen func(context.Context, string) hostinfo.CPUScreen,
	checkTarget func(string) error,
) (hostinfo.NonCPUObservation, error) {
	refuse := func(reason string) (hostinfo.NonCPUObservation, error) {
		return hostinfo.NonCPUObservation{}, output.NewError(output.CodePrecondition, "CPU-only update cannot safely inspect "+cluster,
			output.ExitPrecondition, "Inspect the managed cluster and its loaded service controls, then retry.").
			WithDetails(map[string]any{"reason": reason, "cluster": cluster})
	}
	if config.CheckDatabaseName(cluster) != nil {
		return refuse("cpu_only_cluster_unverified")
	}
	spec, ok := cfg.ClusterByName(cluster)
	if !ok {
		return refuse("cpu_only_cluster_unverified")
	}
	res, err := cfg.Resources(spec)
	if err != nil {
		return hostinfo.NonCPUObservation{}, err
	}
	out, err := probe(ctx, "pg_lsclusters", "-h")
	if err != nil {
		return refuse("cpu_only_cluster_unverified")
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 7 {
			return refuse("cpu_only_cluster_unverified")
		}
		if f[0] != strconv.Itoa(PGVersion) || f[1] != cluster {
			continue
		}
		status, e := clusterPrimaryStatus(f[3])
		port, e2 := strconv.Atoi(f[2])
		if found || e != nil || e2 != nil || status != "online" || port != spec.Port || f[5] != cfg.Server.DataDirectory(cluster) {
			return refuse("cpu_only_cluster_unverified")
		}
		found = true
	}
	if !found {
		return refuse("cpu_only_cluster_unverified")
	}
	if err := checkTarget(cluster); err != nil {
		return refuse("cpu_only_dropin_unverified")
	}
	if err := checkSpareCPU(ctx, cluster, res.CPUMode, screen); err != nil {
		return hostinfo.NonCPUObservation{}, err
	}
	facts := observe(ctx, PGServiceName(cluster))
	if err := hostinfo.ValidateNonCPUControls(facts); err != nil {
		return refuse(err.Error())
	}
	return facts, nil
}

func (s postgresCPUOnly) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	res, err := cfg.Resources(spec)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	err = runCPUOnly(ctx, cfg, spec, res, cpuOnlyOps{
		admit: func(ctx context.Context) (hostinfo.NonCPUObservation, error) {
			return cpuOnlyAdmission(ctx, cfg, spec.Name, Probe, hostinfo.ObserveNonCPUService, hostinfo.CPUServiceScreen, checkCPUOverlayTarget)
		},
		write: func() error { return writeCPUOverlay(PGServiceDropInDir(spec.Name), render.PGCPUOnlyDropin(res)) },
		set: func(ctx context.Context) error {
			return rt.Shell.Exec(ctx, s.ID(), cpuOnlyCommand(spec, res), report)
		},
		nonCPU: func(ctx context.Context) hostinfo.NonCPUObservation {
			return hostinfo.ObserveNonCPUService(ctx, PGServiceName(spec.Name))
		},
		cpu: func(ctx context.Context) hostinfo.CPUObservation {
			return hostinfo.ObserveCPUService(ctx, PGServiceName(spec.Name))
		},
		unchanged: hostinfo.ValidateNonCPUUnchanged,
	})
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

func cpuOnlyCommand(spec config.ClusterSpec, res config.Resources) engine.Command {
	return engine.Command{Name: "systemctl", Args: []string{
		"set-property", "--runtime", PGServiceName(spec.Name) + ".service",
		"CPUWeight=" + render.PGCPUWeight(res), "CPUQuota=" + res.CPUQuota,
	}}
}

type cpuOnlyOps struct {
	admit     func(context.Context) (hostinfo.NonCPUObservation, error)
	write     func() error
	set       func(context.Context) error
	nonCPU    func(context.Context) hostinfo.NonCPUObservation
	cpu       func(context.Context) hostinfo.CPUObservation
	unchanged func(hostinfo.NonCPUObservation, hostinfo.NonCPUObservation) error
}

func runCPUOnly(ctx context.Context, _ config.Cluster, spec config.ClusterSpec, res config.Resources, ops cpuOnlyOps) error {
	before, err := ops.admit(ctx)
	if err != nil {
		return err
	}
	if err := ops.write(); err != nil {
		return err
	}
	if err := ops.unchanged(before, ops.nonCPU(ctx)); err != nil {
		return output.NewError(output.CodeStepFailed, "non-CPU service controls changed before the CPU update",
			output.ExitStep, "Inspect the service controls and re-run apply.").
			WithDetails(map[string]any{"reason": "cpu_only_non_cpu_changed", "cluster": spec.Name, "non_cpu_reason": err.Error()})
	}
	setErr := ops.set(ctx)
	cpu := ops.cpu(ctx)
	after := ops.nonCPU(ctx)
	nonCPUErr := ops.unchanged(before, after)
	if setErr != nil || nonCPUErr != nil {
		failure := cpuPolicyError(spec.Name, res, cpu, "cpu_only_runtime_failure")
		details := failure.Details.(map[string]any)
		if nonCPUErr != nil {
			details["non_cpu_reason"] = nonCPUErr.Error()
		}
		failure.Cause = setErr
		return failure
	}
	if err := hostinfo.ValidateCPUPolicy(cpuPolicy(res), cpu); err != nil {
		var mismatch *hostinfo.CPUPolicyMismatch
		reason := "effective_policy_unknown"
		if errors.As(err, &mismatch) {
			reason = mismatch.Reason
		}
		return cpuPolicyError(spec.Name, res, cpu, reason)
	}
	return nil
}

// cpuOverlayState rejects any foreign file instead of replacing/removing it.
func cpuOverlayState(root *os.Root) (bool, error) {
	info, err := root.Lstat(cpuOverlayName)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.New("CPU overlay is not an owned regular file")
	}
	raw, err := root.ReadFile(cpuOverlayName)
	if err != nil || !validCPUOverlay(raw) {
		return false, errors.New("CPU overlay is not an owned vaire file")
	}
	return true, nil
}

func openCPUOverlayRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("managed service drop-in directory unavailable")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	info, err = root.Lstat("10-vaire.conf")
	if err != nil || !info.Mode().IsRegular() {
		_ = root.Close()
		return nil, errors.New("managed service drop-in unavailable")
	}
	raw, err := root.ReadFile("10-vaire.conf")
	if err != nil || !strings.HasPrefix(string(raw), "# managed by vaire - do not edit; use cluster.toml\n[Service]\n") {
		_ = root.Close()
		return nil, errors.New("service drop-in is not managed by vaire")
	}
	return root, nil
}

func checkCPUOverlayTarget(cluster string) error {
	root, err := openCPUOverlayRoot(PGServiceDropInDir(cluster))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	_, err = cpuOverlayState(root)
	return err
}

func writeCPUOverlay(dir string, content []byte) error {
	if !validCPUOverlay(content) {
		return errors.New("invalid CPU overlay")
	}
	root, err := openCPUOverlayRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err := cpuOverlayState(root); err != nil {
		return err
	}
	return writeCPUOverlayAtomic(root, content, rand.Reader, (*os.Root).Rename)
}

func writeCPUOverlayAtomic(root *os.Root, content []byte, random io.Reader, rename func(*os.Root, string, string) error) error {
	var suffix [12]byte
	if _, err := io.ReadFull(random, suffix[:]); err != nil {
		return err
	}
	tmp := ".20-vaire-cpu.conf." + hex.EncodeToString(suffix[:])
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	ownedTemp := true
	defer func() {
		if ownedTemp {
			_ = root.Remove(tmp)
		}
	}()
	written, writeErr := file.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Chmod(0o644)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	if err := rename(root, tmp, cpuOverlayName); err != nil {
		return err
	}
	ownedTemp = false
	dirFile, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dirFile.Close() }()
	return dirFile.Sync()
}

func cpuOverlayExists(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.IsDir() {
		return false, errors.New("service drop-in directory is not a directory")
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	return cpuOverlayState(root)
}

func removeCPUOverlay(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() {
		return errors.New("service drop-in directory is not a directory")
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	exists, err := cpuOverlayState(root)
	if err != nil || !exists {
		return err
	}
	if err := root.Remove(cpuOverlayName); err != nil {
		return fmt.Errorf("remove managed CPU overlay: %w", err)
	}
	dirFile, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dirFile.Close() }()
	return dirFile.Sync()
}

// prepareFullCPUDropin checks ownership before writing and removes the
// CPU-only override after the full file is written, before daemon-reload.
func prepareFullCPUDropin(dir string, write func() error) error {
	if _, err := cpuOverlayExists(dir); err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	return removeCPUOverlay(dir)
}
