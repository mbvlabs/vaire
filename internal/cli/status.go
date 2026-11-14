package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/health"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/tune"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type statusPools struct {
	ClientsActive  int64 `json:"cl_active"`
	ClientsWaiting int64 `json:"cl_waiting"`
	ServersActive  int64 `json:"sv_active"`
	ServersIdle    int64 `json:"sv_idle"`
	MaxWaitS       int64 `json:"maxwait_s"`
}

type statusCPU struct {
	Requested struct {
		Mode  string `json:"mode"`
		Quota string `json:"quota"`
	} `json:"requested"`
	EffectiveKnown bool `json:"effective_known"`
	Effective      *struct {
		Idle            *bool   `json:"idle,omitempty"`
		QuotaPeriodUsec *uint64 `json:"quota_period_usec,omitempty"`
		QuotaUsec       *uint64 `json:"quota_usec,omitempty"`
		Unlimited       *bool   `json:"unlimited,omitempty"`
		Weight          *uint64 `json:"weight,omitempty"`
	} `json:"effective,omitempty"`
	UsageUsec         *uint64    `json:"usage_usec,omitempty"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	SampleGeneration  string     `json:"sample_generation,omitempty"`
	SampleMonotonicNS *int64     `json:"sample_monotonic_ns,omitempty"`
	UsagePercent      *float64   `json:"usage_percent,omitempty"`
}

type statusDB struct {
	Name             string   `json:"name"`
	State            string   `json:"state"`
	Error            string   `json:"error,omitempty"`
	Size             string   `json:"size"`
	MemoryMiB        int      `json:"memory_mib"`
	MemoryHighMiB    int      `json:"memory_high_mib"`
	MemoryCurrentMiB *int64   `json:"memory_current_mib,omitempty"`
	MemoryPressure10 *float64 `json:"memory_pressure_avg10,omitempty"`
	UptimeS          int64    `json:"uptime_s"`
	Connections      struct {
		Used     int64 `json:"used"`
		Max      int64 `json:"max"`
		Active   int64 `json:"active"`
		Idle     int64 `json:"idle"`
		IdleInTx int64 `json:"idle_in_tx"`
	} `json:"connections"`
	CacheHitRatio *float64 `json:"cache_hit_ratio,omitempty"`
	TPS           *float64 `json:"tps,omitempty"`
	DBSize        int64    `json:"db_size"`
	WALBytes      int64    `json:"wal_bytes"`
	LongestXactS  int64    `json:"longest_xact_s"`
	Backup        struct {
		LastFull         string `json:"last_full,omitempty"`
		LastDiff         string `json:"last_diff,omitempty"`
		LastIncr         string `json:"last_incr,omitempty"`
		WindowStart      string `json:"pitr_window_start,omitempty"`
		ArchiveFailed    int64  `json:"archive_failed_count"`
		LastArchivedAgeS int64  `json:"last_archived_age_s"`
		Sets             int    `json:"sets,omitempty"`
		RepoSize         int64  `json:"repo_size,omitempty"`
		LastFailure      string `json:"last_failure,omitempty"`
	} `json:"backup"`
	Drill             map[string]string `json:"drill,omitempty"`
	RestartPending    bool              `json:"restart_pending"`
	DeferredRestart   string            `json:"deferred_restart,omitempty"`
	RestoreInProgress bool              `json:"restore_in_progress"`
	Protected         bool              `json:"protected,omitempty"`
	// Follows and Follower are a follower's source and replication (Phase 6).
	Follows          string                 `json:"follows,omitempty"`
	Follower         *health.FollowerState  `json:"follower,omitempty"`
	CPU              *statusCPU             `json:"cpu,omitempty"`
	DataFreeBytes    *uint64                `json:"data_free_bytes,omitempty"`
	ApplicationReady *bool                  `json:"application_ready,omitempty"`
	Materialisation  *materialisationReport `json:"materialisation,omitempty"`
}

func (db statusDB) MarshalJSON() ([]byte, error) {
	type plain statusDB
	raw, err := json.Marshal(plain(db))
	if err != nil || db.State != "incomplete" {
		return raw, err
	}
	return omitIncompleteFields(raw, "uptime_s", "connections", "db_size", "wal_bytes", "longest_xact_s", "backup", "restart_pending", "restore_in_progress")
}

type statusData struct {
	Server       string                 `json:"server"`
	Node         string                 `json:"node"`
	Role         string                 `json:"role"`
	VaireVersion string                 `json:"vaire_version"`
	PGVersion    string                 `json:"pg_version,omitempty"`
	Budget       tuneServer             `json:"budget"`
	Poolers      map[string]statusPools `json:"poolers"`
	Disk         *struct {
		Path       string  `json:"path"`
		TotalBytes int64   `json:"total_bytes"`
		FreeBytes  int64   `json:"free_bytes"`
		FreePct    float64 `json:"free_pct"`
	} `json:"disk,omitempty"`
	TLS *struct {
		State    string `json:"state"`
		NotAfter string `json:"not_after,omitempty"`
	} `json:"tls,omitempty"`
	Backups   bool       `json:"backups_configured"`
	Databases []statusDB `json:"databases"`
	Master    *struct {
		Last       string `json:"last"`
		CreatedAt  string `json:"created_at"`
		Uploaded   bool   `json:"uploaded"`
		Bucket     bool   `json:"bucket"`
		Epoch      int64  `json:"epoch"`
		Pending    bool   `json:"pending"`
		Fenced     bool   `json:"fenced"`
		LastFailed string `json:"last_failure"`
	} `json:"master_backup,omitempty"`
}

// statusWatchInterval is how often vaire status --watch refreshes.
const statusWatchInterval = 5 * time.Second

func runStatus(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	params := map[string]any{}
	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}
	fetch := func() (statusData, []output.Warning, error) {
		reply, err := p.call(commandContext(cmd), s.node, "status", params, remote.CallOptions{OnEvent: p.event})
		if err != nil {
			return statusData{}, nil, err
		}
		var data statusData
		if err := reply.Decode(&data); err != nil {
			return statusData{}, nil, err
		}
		return data, reply.Warnings, nil
	}
	data, warnings, err := fetch()
	if err != nil {
		return err
	}
	if !flagTrue(cmd, "watch") || MustNotPrompt(cmd) || !term.IsTerminal(int(os.Stdout.Fd())) { //nolint:gosec // G115: a file descriptor fits in int.
		return output.Success(cmd, output.Result{Data: data, Message: statusMessage(data, warnings), Warnings: append(s.warnings, warnings...),
			Next: statusNext(data)})
	}
	ctx := commandContext(cmd)
	for {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), "\033[H\033[2J"+statusMessage(data, warnings)+
			fmt.Sprintf("\n\nrefreshing every %s · Ctrl-C to stop · %s\n", statusWatchInterval, time.Now().Format("15:04:05")))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(statusWatchInterval):
		}
		next, w, err := fetch()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		statusWatchCPU(data, &next)
		data, warnings = next, w
	}
}

// statusWatchCPU uses consecutive same-generation node monotonic samples.
// Missing or invalid samples are never bridged across a refresh.
func statusWatchCPU(previous statusData, next *statusData) {
	old := make(map[string]*statusCPU, len(previous.Databases))
	for _, db := range previous.Databases {
		old[db.Name] = db.CPU
	}
	for i := range next.Databases {
		cpu := next.Databases[i].CPU
		prior := old[next.Databases[i].Name]
		if cpu == nil {
			continue
		}
		cpu.UsagePercent = nil
		if prior == nil || prior.SampleGeneration == "" || cpu.SampleGeneration != prior.SampleGeneration ||
			prior.SampleMonotonicNS == nil || cpu.SampleMonotonicNS == nil || prior.UsageUsec == nil || cpu.UsageUsec == nil ||
			*cpu.UsageUsec < *prior.UsageUsec || *prior.SampleMonotonicNS <= 0 || *cpu.SampleMonotonicNS <= *prior.SampleMonotonicNS {
			continue
		}
		elapsed := *cpu.SampleMonotonicNS - *prior.SampleMonotonicNS
		rate := float64(*cpu.UsageUsec-*prior.UsageUsec) * 100000 / float64(elapsed)
		cpu.UsagePercent = &rate
	}
}

// statusNext suggests doctor when anything looks wrong.
func statusNext(d statusData) []string {
	for _, db := range d.Databases {
		if db.State != "running" || db.RestartPending {
			return []string{"vaire doctor"}
		}
	}
	return nil
}

// statusMessage renders the server card and a row per database.
func statusMessage(d statusData, warnings []output.Warning) string {
	var lines []string
	head := fmt.Sprintf("server %s · node %s · vaire %s", d.Server, d.Node, d.VaireVersion)
	if d.PGVersion != "" {
		head += " · PostgreSQL " + d.PGVersion
	}
	lines = append(lines, head)
	b := d.Budget
	free := "free " + tune.FormatMiB(b.FreeMiB)
	if b.FreeMiB < 0 {
		free = "overcommitted by " + tune.FormatMiB(-b.FreeMiB)
	}
	lines = append(lines, fmt.Sprintf("  memory  %s RAM · reserve %s · budget %s · allocated %s · %s",
		tune.FormatMiB(b.RAMMiB), tune.FormatMiB(b.ReserveMiB), tune.FormatMiB(b.ClusterBudgetMiB), tune.FormatMiB(b.ClusterMemoryMiB), free))
	if d.Disk != nil {
		lines = append(lines, fmt.Sprintf("  disk    %s free of %s on %s (%.0f%% free)", humanBytes(d.Disk.FreeBytes), humanBytes(d.Disk.TotalBytes), d.Disk.Path, d.Disk.FreePct))
	}
	if d.TLS != nil {
		tls := "  tls     " + d.TLS.State
		if d.TLS.NotAfter != "" {
			tls += " until " + d.TLS.NotAfter
		}
		lines = append(lines, tls)
	}
	for _, name := range []string{"transaction", "session"} {
		if pl, ok := d.Poolers[name]; ok {
			lines = append(lines, fmt.Sprintf("  %-7s clients %d active, %d waiting · servers %d active, %d idle", poolerLabel(name),
				pl.ClientsActive, pl.ClientsWaiting, pl.ServersActive, pl.ServersIdle))
		}
	}
	if !d.Backups {
		lines = append(lines, "  backups none off the server (vaire backup setup)")
	}
	if m := d.Master; m != nil {
		line := "  master  "
		switch {
		case m.Fenced:
			line += "FENCED: a newer master took over this server's bucket"
		case m.Last == "":
			line += "no bundle yet"
		default:
			line += "last bundle " + m.CreatedAt
			switch {
			case m.Uploaded:
				line += fmt.Sprintf(", in the bucket (epoch %d)", m.Epoch)
			case m.Bucket:
				line += ", not uploaded yet"
			default:
				line += ", on the server only (vaire server backup export)"
			}
			if m.Pending {
				line += "; a change is being bundled"
			}
			if m.LastFailed != "" {
				line += "; last failure: " + m.LastFailed
			}
		}
		lines = append(lines, line)
	}
	if len(d.Databases) == 0 {
		lines = append(lines, "", "no databases yet (vaire db create <name>)")
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "", fmt.Sprintf("  %-16s %-11s %-7s %-16s %-9s %-6s %-7s %-20s %s",
		"DATABASE", "STATE", "SIZE", "MEMORY", "CONNS", "CACHE", "TPS", "LAST BACKUP", "UP"))
	for _, db := range d.Databases {
		mem := tune.FormatMiB(db.MemoryMiB)
		if db.MemoryCurrentMiB != nil {
			mem = tune.FormatMiB(int(*db.MemoryCurrentMiB)) + "/" + tune.FormatMiB(db.MemoryMiB)
		}
		if db.State == "incomplete" {
			lines = append(lines, fmt.Sprintf("  %-16s %-11s %-7s %-16s %-9s %-6s %-7s %-20s %s",
				db.Name, db.State, db.Size, mem, "-", "-", "-", "-", "-"))
			continue
		}
		cache, tps := "-", "-"
		if db.CacheHitRatio != nil {
			cache = fmt.Sprintf("%.1f%%", *db.CacheHitRatio*100)
		}
		if db.TPS != nil {
			tps = fmt.Sprintf("%.1f", *db.TPS)
		}
		state := db.State
		if db.RestartPending {
			state += "*"
		}
		last := latestBackup(db)
		lines = append(lines, fmt.Sprintf("  %-16s %-11s %-7s %-16s %-9s %-6s %-7s %-20s %s",
			db.Name, state, db.Size, mem, fmt.Sprintf("%d/%d", db.Connections.Used, db.Connections.Max), cache, tps, last, health.Uptime(db.UptimeS)))
	}
	for _, db := range d.Databases {
		if db.State == "incomplete" {
			lines = append(lines, "  "+db.Name+": "+incompleteReportText(db.Materialisation))
			continue
		}
		if db.Backup.WindowStart != "" {
			lines = append(lines, fmt.Sprintf("  %s: restorable from %s", db.Name, db.Backup.WindowStart))
		}
	}
	for _, db := range d.Databases {
		if db.State == "incomplete" {
			continue
		}
		if line := followerLine(db); line != "" {
			lines = append(lines, line)
		}
		if db.CPU != nil {
			cpu := db.CPU
			quota := cpu.Requested.Quota
			if quota == "" {
				quota = "none"
			}
			line := fmt.Sprintf("  %s: CPU %s, requested quota %s", db.Name, cpu.Requested.Mode, quota)
			if !cpu.EffectiveKnown {
				line += ", effective unknown"
				if cpu.Reason != "" {
					line += " (" + cpu.Reason + ")"
				}
			} else if cpu.Effective != nil {
				if cpu.Effective.Idle != nil && *cpu.Effective.Idle {
					line += ", idle priority"
				} else if cpu.Effective.Weight != nil {
					line += fmt.Sprintf(", weight %d", *cpu.Effective.Weight)
				}
				if cpu.Effective.Unlimited != nil && *cpu.Effective.Unlimited {
					line += ", effective quota unlimited"
				} else if cpu.Effective.QuotaUsec != nil && cpu.Effective.QuotaPeriodUsec != nil {
					line += fmt.Sprintf(", effective quota %d/%d usec", *cpu.Effective.QuotaUsec, *cpu.Effective.QuotaPeriodUsec)
				}
			}
			if cpu.UsagePercent != nil {
				line += fmt.Sprintf(", usage %.1f%% of one CPU", *cpu.UsagePercent)
			} else if cpu.UsageUsec != nil {
				line += fmt.Sprintf(", cumulative usage %d usec", *cpu.UsageUsec)
			}
			lines = append(lines, line)
		}
		if db.Follows != "" && db.DataFreeBytes != nil {
			lines = append(lines, fmt.Sprintf("  %s: data filesystem %d bytes free", db.Name, *db.DataFreeBytes))
		}
	}
	for _, db := range d.Databases {
		if db.RestartPending {
			lines = append(lines, "  * restart pending")
			break
		}
	}
	_ = warnings
	return strings.Join(lines, "\n")
}

func poolerLabel(name string) string {
	if name == "transaction" {
		return "pooler"
	}
	return "session"
}

// latestBackup is the newest of a database's last full, diff and incr.
func latestBackup(db statusDB) string {
	last := ""
	for _, at := range []string{db.Backup.LastFull, db.Backup.LastDiff, db.Backup.LastIncr} {
		if at > last {
			last = at
		}
	}
	if last == "" {
		return "-"
	}
	return last
}

// followerLine is a follower's replication under the table (Phase 6): its
// source, the lag and the slot the source keeps for it.
func followerLine(db statusDB) string {
	f := db.Follower
	if db.Follows == "" {
		return ""
	}
	line := "  " + db.Name + ": follows " + db.Follows
	switch {
	case f == nil || (!f.SourceKnown && !f.SelfKnown):
		return line + " (replication unknown)"
	case f.SelfKnown && !f.InRecovery:
		return line + ", but is out of recovery"
	case f.SelfKnown && f.ReplayPaused:
		line += ", replay PAUSED"
	case !f.SourceKnown:
		return line + " (source state unknown)"
	case !f.SelfKnown:
		line += ", follower state unknown"
	case f.Streaming:
		line += ", streaming, lag " + followerLagText(f)
	default:
		line += ", NOT streaming"
	}
	if !f.SourceKnown {
		return line
	}
	if f.ReceiveBacklogBytes != nil {
		line += fmt.Sprintf(" · receive backlog %d bytes", *f.ReceiveBacklogBytes)
	}
	if f.ReplayBacklogBytes != nil {
		line += fmt.Sprintf(" · replay backlog %d bytes", *f.ReplayBacklogBytes)
	}
	switch {
	case !f.SlotExists:
		line += " · slot " + f.Slot + " missing"
	case f.SlotWALStatus == "lost":
		line += " · slot " + f.Slot + " LOST"
	case f.SlotRetainedBytes == nil:
		line += " · slot " + f.Slot + " holds an unknown amount"
	default:
		line += " · slot " + f.Slot + " holds " + humanBytes(*f.SlotRetainedBytes)
	}
	return line
}
