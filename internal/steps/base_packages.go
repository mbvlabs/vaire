package steps

import (
	"context"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

const basePackagesID = "base-packages-v1"

// basePackageList is step 1's package set (§11). python3-systemd lets the
// fail2ban sshd jail read the journal, since Debian ships no auth.log.
var basePackageList = []string{
	"curl", "ca-certificates", "gnupg", "sudo", "ufw", "fail2ban", "python3-systemd",
	"wireguard-tools", "chrony", "logrotate", "unattended-upgrades", "jq", "lsb-release",
}

// optionalPackageList are transitional packages a newer release may drop.
var optionalPackageList = []string{"apt-transport-https"}

type basePackages struct{}

func (basePackages) ID() string { return basePackagesID }

func (basePackages) Describe(config.Cluster) string {
	return "wait for cloud-init and install the base packages"
}

func (basePackages) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return canonical(map[string]any{"packages": basePackageList, "optional": optionalPackageList})
}

func (basePackages) Check(ctx context.Context, _ config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	var missing []string
	for _, pkg := range basePackageList {
		out, _ := Probe(ctx, "dpkg-query", "-W", "-f=${Status}", pkg)
		if out != "install ok installed" {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		return incomplete("missing " + strings.Join(missing, ", ")), nil
	}
	return complete(), nil
}

func (s basePackages) Apply(ctx context.Context, _ config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	err := rt.Shell.Run(ctx, s.ID(), Script("base-packages"), map[string]string{
		"PACKAGES":          strings.Join(basePackageList, " "),
		"OPTIONAL_PACKAGES": strings.Join(optionalPackageList, " "),
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
