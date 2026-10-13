package steps

import (
	"context"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/render"
)

const kernelTuningID = "kernel-tuning-v1"

// kernelTuningInputs is everything step 7 writes.
type kernelTuningInputs struct {
	Sysctl  string `json:"sysctl"`
	THPUnit string `json:"thp_unit"`
}

// renderKernelTuning reserves the sum of every cluster's huge-page estimate
// (RFC 7.3).
func renderKernelTuning(cfg config.Cluster, host hostinfo.Info) kernelTuningInputs {
	return kernelTuningInputs{
		Sysctl:  string(render.SysctlConf(host, hugePagesFor(cfg, host))),
		THPUnit: string(render.THPUnit()),
	}
}

type kernelTuning struct{}

func (kernelTuning) ID() string { return kernelTuningID }

func (kernelTuning) Describe(config.Cluster) string {
	return "write sysctl settings, with huge pages for every cluster, and disable transparent huge pages"
}

func (kernelTuning) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	in := renderKernelTuning(cfg, rt.Host)
	return []engine.RenderedFile{{Path: SysctlPath, Content: []byte(in.Sysctl)}, {Path: THPUnitPath, Content: []byte(in.THPUnit)}}
}

func (kernelTuning) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	return canonical(renderKernelTuning(cfg, rt.Host))
}

func (kernelTuning) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	in := renderKernelTuning(cfg, rt.Host)
	if !fileIs(SysctlPath, in.Sysctl, 0o644) {
		return incomplete(SysctlPath + " differs"), nil
	}
	if !fileIs(THPUnitPath, in.THPUnit, 0o644) {
		return incomplete(THPUnitPath + " differs"), nil
	}
	for _, kv := range parseSysctl(in.Sysctl) {
		got, err := Probe(ctx, "sysctl", "-n", kv.key)
		if err != nil || strings.TrimSpace(got) != kv.value {
			return incomplete("sysctl " + kv.key + " is not " + kv.value), nil
		}
	}
	if thp, err := Probe(ctx, "cat", "/sys/kernel/mm/transparent_hugepage/enabled"); err != nil || !strings.Contains(thp, "[never]") {
		return incomplete("transparent huge pages are not disabled"), nil
	}
	return complete(), nil
}

func (s kernelTuning) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	in := renderKernelTuning(cfg, rt.Host)
	if err := writeFile(SysctlPath, in.Sysctl, 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFile(THPUnitPath, in.THPUnit, 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	err := rt.Shell.Run(ctx, s.ID(), Script("kernel-tuning"), nil, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

type sysctlKV struct{ key, value string }

// parseSysctl reads "key = value" lines from a rendered sysctl file.
func parseSysctl(content string) []sysctlKV {
	var out []sysctlKV
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out = append(out, sysctlKV{key: strings.TrimSpace(key), value: strings.TrimSpace(value)})
	}
	return out
}

// hugePagesFromKernel reads a stopped cluster's
// shared_memory_size_in_huge_pages by starting its postgres with -C. It is
// used by postgres-config-v1 during a restart.
func hugePagesFromKernel(ctx context.Context, cluster, dataDir string) (int, error) {
	out, err := Probe(ctx, "runuser", "-u", "postgres", "--",
		"/usr/lib/postgresql/"+strconv.Itoa(PGVersion)+"/bin/postgres",
		"-D", dataDir, "-c", "config_file="+PGConfDir(cluster)+"/postgresql.conf", "-C", "shared_memory_size_in_huge_pages")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}
