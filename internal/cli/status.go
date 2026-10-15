package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/health"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/tune"
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
}

type statusData struct {
	Server     string                 `json:"server"`
	Node       string                 `json:"node"`
	Role       string                 `json:"role"`
	EruVersion string                 `json:"eru_version"`
	PGVersion  string                 `json:"pg_version,omitempty"`
	Budget     tuneServer             `json:"budget"`
	Poolers    map[string]statusPools `json:"poolers"`
	Disk       *struct {
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
}

// statusWatchInterval is how often eru status --watch refreshes.
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
		data, warnings = next, w
	}
}

// statusNext suggests doctor when anything looks wrong.
func statusNext(d statusData) []string {
	for _, db := range d.Databases {
		if db.State != "running" || db.RestartPending {
			return []string{"eru doctor"}
		}
	}
	return nil
}

// statusMessage renders the server card and a row per database.
func statusMessage(d statusData, warnings []output.Warning) string {
	var lines []string
	head := fmt.Sprintf("server %s · node %s · eru %s", d.Server, d.Node, d.EruVersion)
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
		lines = append(lines, "  backups none off the server (eru backup setup)")
	}
	if len(d.Databases) == 0 {
		lines = append(lines, "", "no databases yet (eru db create <name>)")
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "", fmt.Sprintf("  %-16s %-11s %-7s %-16s %-9s %-6s %-7s %-20s %s",
		"DATABASE", "STATE", "SIZE", "MEMORY", "CONNS", "CACHE", "TPS", "LAST BACKUP", "UP"))
	for _, db := range d.Databases {
		mem := tune.FormatMiB(db.MemoryMiB)
		if db.MemoryCurrentMiB != nil {
			mem = tune.FormatMiB(int(*db.MemoryCurrentMiB)) + "/" + tune.FormatMiB(db.MemoryMiB)
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
		if db.Backup.WindowStart != "" {
			lines = append(lines, fmt.Sprintf("  %s: restorable from %s", db.Name, db.Backup.WindowStart))
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
