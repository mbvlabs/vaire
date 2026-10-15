package doctor

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/engine"
)

// NodeChecks are the checks the node runs, in report order. The operator
// adds its own (eru.version_skew, ctx.file_modes, metrics.auth).
func NodeChecks() []Check {
	return []Check{
		{ID: "host.os_supported", Category: CatHost, AtInit: true, Run: osSupported},
		{ID: "host.reboot_required", Category: CatHost, AtInit: true, Run: rebootRequired},
		{ID: "host.disk_data", Category: CatHost, AtInit: true, Run: diskData},
		{ID: "host.unattended_upgrades", Category: CatHost, Security: true, AtInit: true, Run: unattended},
		{ID: "host.time_sync", Category: CatHost, AtInit: true, Run: timeSync},
		{ID: "ssh.public_closed", Category: CatHost, Security: true, AtInit: true, Run: publicSSH},
		{ID: "ssh.config", Category: CatHost, Security: true, AtInit: true, Run: sshConfig},
		{ID: "fw.active", Category: CatHost, Security: true, AtInit: true, Run: fwActive},
		{ID: "fw.rules", Category: CatHost, Security: true, AtInit: true, Run: fwRules},
		{ID: "net.allowlist_open", Category: CatHost, Security: true, AtInit: true, Run: allowlistOpen},
		{ID: "tls.expiry", Category: CatTLS, AtInit: true, Run: tlsExpiry},
		{ID: "tls.dns", Category: CatTLS, AtInit: true, Run: tlsDNS},
		{ID: "tls.mode", Category: CatTLS, Security: true, AtInit: true, Run: tlsMode},
		{ID: "secrets.confirmed", Category: CatHost, Security: true, Run: secretsConfirmed},
		{ID: "metrics.endpoint", Category: CatMetrics, Security: true, AtInit: true, Run: metricsEndpoint},
		{ID: "nginx.running", Category: CatHost, AtInit: true, Run: nginxRunning},
		{ID: "pooler.running", Category: CatPostgres, AtInit: true, Run: poolerRunning},
		{ID: "pg.running", Category: CatPostgres, AtInit: true, Run: pgRunning},
		{ID: "pg.config_drift", Category: CatPostgres, AtInit: true, Run: pgConfigDrift},
		{ID: "pg.restart_pending", Category: CatPostgres, AtInit: true, Run: pgRestartPending},
		{ID: "pg.alter_system_off", Category: CatPostgres, Security: true, AtInit: true, Run: pgAlterSystem},
		{ID: "pg.hba_no_trust", Category: CatPostgres, Security: true, AtInit: true, Run: pgHBA},
		{ID: "pg.ssl", Category: CatPostgres, Security: true, AtInit: true, Run: pgSSL},
		{ID: "pg.listen_socket_only", Category: CatPostgres, Security: true, AtInit: true, Run: pgSocketOnly},
		{ID: "pg.checksums", Category: CatPostgres, AtInit: true, Run: pgChecksums},
		{ID: "pg.wraparound", Category: CatPostgres, AtInit: true, Run: pgWraparound},
		{ID: "pg.connections", Category: CatPostgres, AtInit: true, Run: pgConnections},
		{ID: "pg.memory_pressure", Category: CatPostgres, AtInit: true, Run: pgMemoryPressure},
		{ID: "pg.would_not_start", Category: CatPostgres, AtInit: true, Run: pgWouldNotStart},
		{ID: "pg.memory_budget", Category: CatPostgres, AtInit: true, Run: memoryBudget},
		{ID: "jobs.skipped", Category: CatBackups, Run: jobsSkipped},
		{ID: "jobs.failed", Category: CatBackups, Run: jobsFailed},
		{ID: "backup.offsite", Category: CatBackups, Run: backupOffsite},
		{ID: "backup.archiving", Category: CatBackups, Run: backupArchiving},
		{ID: "backup.full_age", Category: CatBackups, Run: backupFullAge},
		{ID: "backup.verify", Category: CatBackups, Run: backupVerify},
		{ID: "backup.drill", Category: CatBackups, Run: backupDrill},
	}
}

func osSupported(f Facts) []Result {
	if f.OSSupported {
		return one(Pass, f.OSName, "")
	}
	return one(Fail, f.OSName+" is not supported (Debian 13, Ubuntu 26.04)", "rebuild on a supported release")
}

func rebootRequired(f Facts) []Result {
	if f.RebootRequired {
		return one(Warn, "/var/run/reboot-required exists: a security update waits for a reboot", "reboot the server in a quiet hour (eru ssh --command 'sudo reboot')")
	}
	return one(Pass, "no reboot pending", "")
}

// FreePct is the free share of a filesystem in percent.
func FreePct(total, free uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(free) * 100 / float64(total)
}

func diskData(f Facts) []Result {
	if !f.DiskKnown {
		return one(Warn, "cannot read the data root's filesystem", "")
	}
	pct := FreePct(f.DiskTotal, f.DiskFree)
	detail := fmt.Sprintf("%.0f%% free on %s", pct, f.DataRoot)
	switch {
	case pct < DiskFailFreePct:
		return one(Fail, detail+" (fails under 10%: a full disk stops every database)", "free space or grow the volume; eru backup list shows dropped stanzas, eru restore cleanup leftovers")
	case pct < DiskWarnFreePct:
		return one(Warn, detail, "free space or grow the volume before it reaches 10%")
	}
	return one(Pass, detail, "")
}

func unattended(f Facts) []Result {
	if f.UnattendedActive {
		return one(Pass, "unattended-upgrades is active (security updates only)", "")
	}
	return one(Warn, "unattended-upgrades is not active", "eru apply")
}

func timeSync(f Facts) []Result {
	if !f.TimeKnown {
		return one(Warn, "cannot read the clock's sync state", "")
	}
	if f.TimeSynced {
		return one(Pass, "the clock is synchronised", "")
	}
	if f.Init {
		return one(Warn, "the clock is not synchronised yet", "eru apply")
	}
	return one(Fail, "the clock is not synchronised (chrony): TLS, TOTP and PITR targets depend on it", "eru apply")
}

func publicSSH(f Facts) []Result {
	switch {
	case f.BreakGlassOpen:
		return one(Warn, "break-glass public SSH is open", "close it: eru ssh --command 'sudo eru node ssh-revert --lockdown-only'")
	case f.LockdownPending:
		return one(Warn, "the SSH lockdown is pending confirmation", "eru server init --resume")
	case f.LockedDownAt == "" && f.Init:
		// The lockdown is init's own phase with its own failure handling
		// (--local leaves it to the first operator command), so it doesn't
		// block the gate.
		return one(Warn, "public SSH (22/tcp) is still open", "confirm the lockdown over WireGuard (any eru command from your machine)")
	case f.LockedDownAt == "":
		return one(Fail, "public SSH (22/tcp) is still open", "eru server init --resume")
	}
	return one(Pass, "SSH listens on WireGuard only since "+f.LockedDownAt, "")
}

func sshConfig(f Facts) []Result {
	if f.SSHConfigDrift != "" {
		return one(Fail, "the sshd drop-in differs: "+f.SSHConfigDrift, "eru apply")
	}
	return one(Pass, "the sshd drop-in matches", "")
}

func fwActive(f Facts) []Result {
	if f.UFWActive {
		return one(Pass, "ufw is active", "")
	}
	return one(Fail, "ufw is not active", "eru apply")
}

func fwRules(f Facts) []Result {
	if f.FirewallDrift != "" {
		return one(Fail, "the firewall rules differ: "+f.FirewallDrift, "eru apply")
	}
	return one(Pass, "the firewall rules match", "")
}

func allowlistOpen(f Facts) []Result {
	if len(f.OpenAllowlists) > 0 {
		return one(Warn, "open to every address: "+joinSorted(f.OpenAllowlists), "narrow the CIDRs with eru config set")
	}
	return one(Pass, "no allowlist is open to every address", "")
}

func tlsExpiry(f Facts) []Result {
	if !f.CertKnown {
		return one(Fail, "cannot read the node's certificate", "eru apply")
	}
	days := f.CertNotAfter.Sub(f.Now).Hours() / 24
	detail := fmt.Sprintf("the certificate expires %s (%.0f days)", f.CertNotAfter.UTC().Format(time.RFC3339), math.Floor(days))
	switch {
	case days < TLSFailDays:
		return one(Fail, detail, "eru ssh --command 'sudo eru node run certs-renew'")
	case days < TLSWarnDays:
		return one(Warn, detail+"; renewal starts 30 days ahead, so it is failing", "eru logs eru-certs")
	}
	return one(Pass, detail, "")
}

func tlsDNS(f Facts) []Result {
	if f.Hostname == "" {
		return one(Skip, "no domain set", "")
	}
	if !f.DNSChecked {
		return one(Warn, "cannot resolve "+f.Hostname+": "+f.DNSDetail, "check the DNS record (DNS only, not proxied)")
	}
	if !f.DNSMatches {
		return one(Warn, f.Hostname+" "+f.DNSDetail, "point the record at the node's public address")
	}
	return one(Pass, f.Hostname+" resolves to this node", "")
}

func tlsMode(f Facts) []Result {
	switch {
	case f.TLSMode == "self-signed":
		return one(Warn, "self-signed mode: clients can't verify the server (sslmode=require)", "eru config set tls.mode acme")
	case f.CertSelfSign:
		return one(Warn, "the ACME certificate is still pending; the placeholder is served", "eru logs eru-certs")
	}
	return one(Pass, "an ACME certificate is served", "")
}

func secretsConfirmed(f Facts) []Result {
	if f.SecretsAtTime == "" {
		return one(Fail, "the secrets handoff was never confirmed as stored", "eru secrets export")
	}
	return one(Pass, "the secrets handoff was stored "+f.SecretsAtTime, "")
}

func metricsEndpoint(f Facts) []Result {
	if !f.MetricsEnabled {
		return one(Skip, "[metrics] enabled is false", "")
	}
	var down []string
	for name, err := range f.Exporters {
		if err != nil {
			down = append(down, name+" ("+err.Error()+")")
		}
	}
	if len(down) > 0 {
		return one(Warn, "not answering: "+joinSorted(down), "eru apply")
	}
	if f.EndpointStatus != 401 {
		detail := fmt.Sprintf("/metrics answered %d without a credential, want 401", f.EndpointStatus)
		if f.EndpointStatus == 0 {
			detail = "/metrics did not answer: " + f.EndpointError
		}
		return one(Warn, detail, "eru apply")
	}
	return one(Pass, "every exporter answers and /metrics refuses a request without the credential", "")
}

func nginxRunning(f Facts) []Result {
	if f.NginxActive {
		return one(Pass, "nginx is active", "")
	}
	return one(Fail, "nginx is not active: 443 and /metrics are down", "eru apply")
}

func poolerRunning(f Facts) []Result {
	if !f.HasDatabases {
		return one(Skip, "no databases (the poolers stop with the last one)", "")
	}
	var down []string
	for name, ok := range f.PoolersActive {
		if !ok {
			down = append(down, name)
		}
	}
	if len(down) > 0 {
		return one(Fail, "not active: "+joinSorted(down), "eru apply")
	}
	return one(Pass, "both poolers are active", "")
}

// perCluster runs fn for every cluster, skipping those a rebuilt master
// hasn't recovered and those not running when needsRunning.
func perCluster(f Facts, needsRunning bool, fn func(c ClusterFacts) (Status, string, string)) []Result {
	var out []Result
	for _, c := range f.Clusters {
		r := Result{DB: c.Name}
		switch {
		case c.AwaitingRecovery:
			r.Status, r.Detail = Skip, "awaiting recovery (eru db recover "+c.Name+")"
		case needsRunning && c.State != "running":
			r.Status, r.Detail = Skip, "the cluster is "+c.State
		default:
			r.Status, r.Detail, r.Fix = fn(c)
		}
		out = append(out, r)
	}
	return out
}

func pgRunning(f Facts) []Result {
	return perCluster(f, false, func(c ClusterFacts) (Status, string, string) {
		switch c.State {
		case "running":
			return Pass, "running", ""
		case "unreachable":
			return Fail, "the cluster does not answer: " + c.Error, "eru logs postgres --db " + c.Name
		}
		return Fail, "the cluster is not running", "eru logs postgres --db " + c.Name
	})
}

func pgConfigDrift(f Facts) []Result {
	return perCluster(f, false, func(c ClusterFacts) (Status, string, string) {
		if c.ConfigDrift != "" {
			return Warn, "the managed configuration differs: " + c.ConfigDrift, "eru apply --db " + c.Name
		}
		if len(c.Overrides) > 0 {
			return Warn, "90-overrides.conf sets " + joinSorted(c.Overrides), ""
		}
		return Pass, "the managed configuration is on disk, no overrides", ""
	})
}

func pgRestartPending(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		if c.Sample.PendingRestart > 0 {
			return Warn, fmt.Sprintf("%d settings wait for a restart", c.Sample.PendingRestart), "eru apply --db " + c.Name + " --confirm " + c.Name + "/restart"
		}
		return Pass, "no setting waits for a restart", ""
	})
}

func pgAlterSystem(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		if c.Sample.AllowAlterSystem != "off" {
			return Fail, "allow_alter_system is " + c.Sample.AllowAlterSystem, "eru apply --db " + c.Name
		}
		return Pass, "allow_alter_system is off", ""
	})
}

func pgHBA(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		if len(c.HBAWeak) > 0 {
			return Fail, "pg_hba allows " + joinSorted(c.HBAWeak), "eru apply --db " + c.Name
		}
		return Pass, "no trust, password or md5 line", ""
	})
}

func pgSSL(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		if c.Sample.SSL != "on" {
			return Fail, "ssl is " + c.Sample.SSL, "eru apply --db " + c.Name
		}
		return Pass, "ssl is on", ""
	})
}

func pgSocketOnly(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		if strings.TrimSpace(c.Sample.ListenAddresses) != "" {
			return Fail, "the cluster listens on TCP (" + c.Sample.ListenAddresses + "); only the poolers should", "eru apply --db " + c.Name
		}
		return Pass, "socket only", ""
	})
}

func pgChecksums(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		if c.Sample.DataChecksums != "on" {
			return Warn, "data checksums are off", ""
		}
		return Pass, "data checksums are on", ""
	})
}

func pgWraparound(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		age := c.Sample.MaxXIDAge
		if c.Sample.MaxMXIDAge > age {
			age = c.Sample.MaxMXIDAge
		}
		detail := fmt.Sprintf("oldest transaction ID age %d", age)
		switch {
		case age > WraparoundFail:
			return Fail, detail + " (fails above 1.6 billion)", "eru maint vacuum --db " + c.Name + " --freeze"
		case age > WraparoundWarn:
			return Warn, detail, "eru maint report wraparound --db " + c.Name
		}
		return Pass, detail, ""
	})
}

func pgConnections(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		detail := fmt.Sprintf("%d of %d connections", c.Sample.Connections, c.Sample.MaxConnections)
		if c.Sample.MaxConnections > 0 && float64(c.Sample.Connections) > ConnectionsWarn*float64(c.Sample.MaxConnections) {
			return Warn, detail, "eru top --db " + c.Name
		}
		return Pass, detail, ""
	})
}

func pgMemoryPressure(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		if !c.PressureKnown {
			return Skip, "no memory pressure reading", ""
		}
		throttled := c.PrevKnown && c.HighEvents > c.PrevHighEvents
		switch {
		case c.PressureAvg300 > PressureWarnPct:
			return Warn, fmt.Sprintf("memory pressure %.1f%% over 5 minutes: the cluster stalls at its MemoryHigh", c.PressureAvg300), "eru db resize " + c.Name + " --size <larger>"
		case throttled:
			return Warn, fmt.Sprintf("throttled at MemoryHigh %d times since the last check", c.HighEvents-c.PrevHighEvents), "eru db resize " + c.Name + " --size <larger>"
		}
		return Pass, fmt.Sprintf("memory pressure %.1f%%", c.PressureAvg300), ""
	})
}

func pgWouldNotStart(f Facts) []Result {
	return perCluster(f, false, func(c ClusterFacts) (Status, string, string) {
		if c.State == "running" {
			return Pass, "running", ""
		}
		if c.WouldNotStart != "" {
			return Fail, c.WouldNotStart, "eru db resize " + c.Name + " --size <smaller>"
		}
		return Pass, "it would start", ""
	})
}

func memoryBudget(f Facts) []Result {
	if len(f.BudgetWarnings) == 0 {
		return one(Pass, "the databases fit the server's budget", "")
	}
	var msgs []string
	for _, w := range f.BudgetWarnings {
		msgs = append(msgs, w.Code+": "+w.Message)
	}
	return one(Warn, strings.Join(msgs, "; "), "eru tune show")
}

// timerJobs are the timer jobs doctor watches for skips and failures.
var timerJobs = map[string]bool{
	"backup-full": true, "backup-diff": true, "backup-incr": true, "backup-verify": true, "restore-drill": true,
	"db-dump": true, "stanza-expiry": true, "maint-daily": true, "maint-weekly": true,
}

// lastRuns groups the timer runs (newest last) by job and database.
func lastRuns(jobs []engine.JobRecord) map[string][]engine.JobRecord {
	out := map[string][]engine.JobRecord{}
	for _, j := range jobs {
		if !timerJobs[j.Job] || j.State == engine.JobRunning {
			continue
		}
		key := j.Job
		if j.DB != "" {
			key += "@" + j.DB
		}
		out[key] = append(out[key], j)
	}
	return out
}

func jobsSkipped(f Facts) []Result {
	var bad []string
	for key, runs := range lastRuns(f.Jobs) {
		if len(runs) < SkipsInARowWarns {
			continue
		}
		all := true
		for _, j := range runs[len(runs)-SkipsInARowWarns:] {
			if j.State != engine.JobSkipped {
				all = false
			}
		}
		if all {
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		return one(Warn, "skipped three runs in a row (a lock was busy): "+joinSorted(bad), "eru job list")
	}
	return one(Pass, "no timer job keeps being skipped", "")
}

func jobsFailed(f Facts) []Result {
	var bad []string
	for key, runs := range lastRuns(f.Jobs) {
		if runs[len(runs)-1].State == engine.JobFailed {
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		return one(Warn, "the last run failed: "+joinSorted(bad), "eru job list")
	}
	return one(Pass, "no timer job's last run failed", "")
}

func backupOffsite(f Facts) []Result {
	if !f.BackupConfigured {
		return one(Warn, "no backups leave the server (no_offsite_backup)", "eru backup setup")
	}
	return one(Pass, "backups go to the bucket", "")
}

// backupCluster runs fn for clusters when backups are on.
func backupCluster(f Facts, fn func(c ClusterFacts) (Status, string, string)) []Result {
	if !f.BackupConfigured {
		var out []Result
		for _, c := range f.Clusters {
			out = append(out, Result{DB: c.Name, Status: Skip, Detail: "backups are not configured"})
		}
		return out
	}
	return perCluster(f, false, fn)
}

func backupArchiving(f Facts) []Result {
	return backupCluster(f, func(c ClusterFacts) (Status, string, string) {
		if c.State != "running" {
			return Skip, "the cluster is " + c.State, ""
		}
		s := c.Sample
		if s.ArchiveFailed > 0 && s.LastFailedAgeS >= 0 && s.LastFailedAgeS < 3600 &&
			(s.LastArchivedAgeS < 0 || s.LastFailedAgeS < s.LastArchivedAgeS) {
			return Fail, fmt.Sprintf("WAL archiving failed %d s ago and has not succeeded since", s.LastFailedAgeS), "eru logs postgres --db " + c.Name
		}
		// An idle database archives nothing (archive_timeout only switches a
		// segment with WAL in it), so a long gap alone is not a problem; a
		// database that never archived anything once it should have is.
		if s.LastArchivedAgeS < 0 && !young(f, c) {
			return Warn, "nothing was ever archived", "eru backup list --db " + c.Name
		}
		return Pass, "archiving works", ""
	})
}

// young reports whether a database is younger than its first full backup's
// schedule, so a missing backup is not yet a problem.
func young(f Facts, c ClusterFacts) bool {
	return !c.CreatedAt.IsZero() && c.FullInterval > 0 && f.Now.Sub(c.CreatedAt) < c.FullInterval+24*time.Hour
}

func backupFullAge(f Facts) []Result {
	return backupCluster(f, func(c ClusterFacts) (Status, string, string) {
		last, ok := f.Records.LastBackup[c.Name][backup.TypeFull]
		if !ok {
			if young(f, c) {
				return Skip, "no full backup yet; the first is due on schedule", ""
			}
			return Fail, "no full backup", "eru backup now --db " + c.Name + " --type full"
		}
		at, err := time.Parse(time.RFC3339, last.FinishedAt)
		if err != nil {
			return Warn, "the last full's time does not parse", ""
		}
		if c.FullInterval <= 0 {
			return Warn, "cannot read the full backup schedule: " + c.FullIntervalErr, ""
		}
		age := f.Now.Sub(at)
		detail := fmt.Sprintf("last full %s ago (schedule every %s)", age.Round(time.Minute), c.FullInterval)
		switch {
		case age > 2*c.FullInterval:
			return Fail, detail, "eru backup now --db " + c.Name + " --type full"
		case age > c.FullInterval+24*time.Hour:
			return Warn, detail, "eru job list"
		}
		return Pass, detail, ""
	})
}

func backupVerify(f Facts) []Result {
	return backupCluster(f, func(c ClusterFacts) (Status, string, string) {
		v, ok := f.Records.VerifyOf(c.Name, c.Stanza)
		if !ok {
			if young(f, c) {
				return Skip, "no verify yet", ""
			}
			return Warn, "the stanza was never verified", "eru backup verify --db " + c.Name
		}
		if !v.OK {
			return Warn, "the last verify found problems", "eru backup verify --db " + c.Name
		}
		at, err := time.Parse(time.RFC3339, v.FinishedAt)
		if err == nil && f.Now.Sub(at) > VerifyMaxAge {
			return Warn, "the last verify is older than 14 days", "eru backup verify --db " + c.Name
		}
		return Pass, "verified " + v.FinishedAt, ""
	})
}

func backupDrill(f Facts) []Result {
	return backupCluster(f, func(c ClusterFacts) (Status, string, string) {
		last, ok := f.Records.LastDrill(c.Name)
		if !ok {
			if c.CreatedAt.IsZero() || f.Now.Sub(c.CreatedAt) < DrillMaxAge {
				return Skip, "the first drill is not due yet", ""
			}
			return Warn, "never drilled", "eru restore drill --db " + c.Name
		}
		if last.Result == backup.DrillFail {
			return Fail, "the last restore drill failed: " + last.Reason, "eru restore drill --db " + c.Name
		}
		var lastPass time.Time
		for _, d := range f.Records.Drills {
			if d.Database == c.Name && d.Result == backup.DrillPass {
				if t, err := time.Parse(time.RFC3339, d.FinishedAt); err == nil && t.After(lastPass) {
					lastPass = t
				}
			}
		}
		if lastPass.IsZero() || f.Now.Sub(lastPass) > DrillMaxAge {
			return Warn, "no passing drill in 40 days", "eru restore drill --db " + c.Name
		}
		return Pass, "last drill passed " + lastPass.UTC().Format(time.RFC3339), ""
	})
}
