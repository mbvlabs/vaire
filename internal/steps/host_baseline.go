package steps

import (
	"context"
	"os"
	"sort"
	"strconv"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

const hostBaselineID = "host-baseline-v1"

// hostFiles are the fixed files step 6 renders. They carry no secrets.
var hostFiles = map[string]string{
	"/etc/systemd/journald.conf.d/eru.conf": `# managed by eru
[Journal]
Storage=persistent
Compress=yes
SystemMaxUse=1G
SystemKeepFree=1G
RuntimeMaxUse=256M
RuntimeKeepFree=256M
MaxRetentionSec=14day
`,
	"/etc/apt/apt.conf.d/20auto-upgrades": `// managed by eru
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
`,
	// #clear drops the distribution's default origins, so only security
	// updates install. PostgreSQL and its tools move only with eru pg update.
	"/etc/apt/apt.conf.d/52eru-unattended-upgrades": `// managed by eru: security updates only
#clear Unattended-Upgrade::Origins-Pattern;
#clear Unattended-Upgrade::Allowed-Origins;
#clear Unattended-Upgrade::Package-Blacklist;
Unattended-Upgrade::Origins-Pattern {
	"origin=Debian,codename=${distro_codename}-security,label=Debian-Security";
	"origin=Debian,codename=${distro_codename},label=Debian-Security";
	"origin=Ubuntu,archive=${distro_codename}-security,label=Ubuntu";
};
Unattended-Upgrade::Package-Blacklist {
	"postgresql-";
	"libpq";
	"pgbouncer";
	"pgbackrest";
	"caddy";
};
Unattended-Upgrade::Automatic-Reboot "false";
`,
	// iptables actions keep bans in fail2ban's own chains, out of the ufw rule
	// set that firewall-v1 converges.
	"/etc/fail2ban/jail.d/eru.conf": `# managed by eru
[DEFAULT]
banaction = iptables-multiport
banaction_allports = iptables-allports

[sshd]
enabled = true
backend = systemd
maxretry = 5
findtime = 10m
bantime = 1h
`,
	"/etc/logrotate.d/eru": `# managed by eru
/var/log/eru/*.log {
	daily
	rotate 30
	maxage 30
	missingok
	notifempty
	compress
	delaycompress
	nocreate
}
`,
}

type hostBaseline struct{}

func (hostBaseline) ID() string { return hostBaselineID }

func (hostBaseline) Describe(cfg config.Cluster) string {
	return "journald limits, swap, timezone " + cfg.Timezone + ", chrony, security-only unattended-upgrades, fail2ban, logrotate"
}

func hostFilePaths() []string {
	paths := make([]string, 0, len(hostFiles))
	for path := range hostFiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (hostBaseline) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	return canonical(map[string]any{
		"files":    hostFiles,
		"swap_mb":  cfg.Server.SwapMB,
		"timezone": cfg.Timezone,
	})
}

func (hostBaseline) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	for _, path := range hostFilePaths() {
		if !fileIs(path, hostFiles[path], 0o644) {
			return incomplete(path + " differs"), nil
		}
	}
	for _, unit := range []string{"chrony", "fail2ban"} {
		if !serviceActive(ctx, unit) {
			return incomplete(unit + " is not active"), nil
		}
	}
	if tz, _ := Probe(ctx, "timedatectl", "show", "-p", "Timezone", "--value"); tz != cfg.Timezone {
		return incomplete("timezone is " + tz), nil
	}
	if cfg.Server.SwapMB > 0 {
		if swaps, _ := Probe(ctx, "swapon", "--noheadings", "--raw"); swaps == "" {
			return incomplete("no swap is active"), nil
		}
	}
	return complete(), nil
}

func (s hostBaseline) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	for _, path := range hostFilePaths() {
		if err := writeFile(path, hostFiles[path], 0o644); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if err := os.MkdirAll(engine.LogDir, 0o750); err != nil {
		return engine.ApplyResult{}, err
	}
	err := rt.Shell.Run(ctx, s.ID(), Script("host-baseline"), map[string]string{
		"SWAP_MB":  strconv.Itoa(cfg.Server.SwapMB),
		"TIMEZONE": cfg.Timezone,
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
