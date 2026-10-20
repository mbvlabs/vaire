// Package top is vaire top's model (Phase 4): the SQL it reads each database
// with, the deltas between two samples, the blocking chains, and the
// rendering. The terminal program and the connections live in package cli.
package top

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/pg"
)

// ActivitySQL lists the client sessions of the connected database, with
// what blocks each.
const ActivitySQL = `SELECT pid::text AS pid, usename AS role, coalesce(state, '') AS state,
  coalesce(wait_event_type || ':' || wait_event, '') AS wait,
  coalesce(extract(epoch FROM now() - coalesce(xact_start, query_start, backend_start))::bigint, 0)::text AS secs,
  coalesce(application_name, '') AS app,
  left(regexp_replace(coalesce(query, ''), '\s+', ' ', 'g'), 200) AS query,
  array_to_string(pg_blocking_pids(pid), ',') AS blocked_by
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() AND datname = current_database()
ORDER BY state = 'active' DESC, secs DESC`

// DatabaseSQL reads the connected database's counters.
const DatabaseSQL = `SELECT (xact_commit + xact_rollback)::text AS xacts, blks_hit::text AS blks_hit, blks_read::text AS blks_read,
  current_setting('max_connections') AS max_connections
FROM pg_stat_database WHERE datname = current_database()`

// StatementsSQL reads statement deltas, excluding internal roles before the
// limit unless allSessions is set, just like vaire queries.
func StatementsSQL(allSessions bool) string {
	filter := ""
	if !allSessions {
		filter = "\n  AND NOT " + pg.VaireRoleSQL("r.rolname")
	}
	return `SELECT s.queryid::text AS queryid, s.calls::text AS calls, s.total_exec_time::text AS total_ms,
  left(regexp_replace(s.query, '\s+', ' ', 'g'), 200) AS query
FROM pg_stat_statements s JOIN pg_roles r ON r.oid = s.userid
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())` + filter + `
ORDER BY s.total_exec_time DESC LIMIT 300`
}

// HasStatementsSQL tells whether pg_stat_statements is installed.
const HasStatementsSQL = `SELECT count(*)::text AS n FROM pg_extension WHERE extname = 'pg_stat_statements'`

// Session is one client backend.
type Session struct {
	DB        string `json:"db"`
	PID       int    `json:"pid"`
	Role      string `json:"role"`
	State     string `json:"state"`
	Wait      string `json:"wait,omitempty"`
	Seconds   int64  `json:"seconds"`
	App       string `json:"application_name,omitempty"`
	Query     string `json:"query"`
	BlockedBy []int  `json:"blocked_by,omitempty"`
}

// Counters is one database's pg_stat_database reading.
type Counters struct {
	Xacts, BlksHit, BlksRead, MaxConnections int64
}

// Statement is one pg_stat_statements row.
type Statement struct {
	QueryID string
	Calls   int64
	TotalMS float64
	Query   string
}

// Sample is one database at one moment.
type Sample struct {
	DB         string
	At         time.Time
	Counters   Counters
	Sessions   []Session
	Statements map[string]Statement
	Error      string
}

// DBRow is one database's line in vaire top.
type DBRow struct {
	DB          string   `json:"db"`
	Connections int      `json:"connections"`
	Active      int      `json:"active"`
	IdleInTx    int      `json:"idle_in_tx"`
	Max         int64    `json:"max_connections"`
	TPS         *float64 `json:"tps,omitempty"`
	CacheHit    *float64 `json:"cache_hit_ratio,omitempty"`
	LongestXact int64    `json:"longest_xact_s"`
	Error       string   `json:"error,omitempty"`
}

// Busy is a statement's work between two samples.
type Busy struct {
	DB      string  `json:"db"`
	QueryID string  `json:"queryid"`
	Calls   int64   `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	Query   string  `json:"query"`
}

// Pool is one pooler database's SHOW POOLS line.
type Pool struct {
	Pooler    string `json:"pooler"`
	Database  string `json:"database"`
	ClActive  int64  `json:"cl_active"`
	ClWaiting int64  `json:"cl_waiting"`
	SvActive  int64  `json:"sv_active"`
	SvIdle    int64  `json:"sv_idle"`
	MaxWait   int64  `json:"maxwait_s"`
}

// Snapshot is what vaire top shows and --json prints.
type Snapshot struct {
	At         time.Time `json:"at"`
	Databases  []DBRow   `json:"databases"`
	Sessions   []Session `json:"sessions"`
	Blocking   []string  `json:"blocking"`
	Statements []Busy    `json:"statements"`
	Pools      []Pool    `json:"pools"`
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// ParseSessions reads ActivitySQL rows of db.
func ParseSessions(db string, rows []map[string]string) []Session {
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		s := Session{DB: db, PID: int(atoi(r["pid"])), Role: r["role"], State: r["state"], Wait: r["wait"],
			Seconds: atoi(r["secs"]), App: r["app"], Query: r["query"]}
		for _, p := range strings.Split(r["blocked_by"], ",") {
			if n := atoi(p); n > 0 {
				s.BlockedBy = append(s.BlockedBy, int(n))
			}
		}
		out = append(out, s)
	}
	return out
}

// ParseCounters reads a DatabaseSQL row.
func ParseCounters(row map[string]string) Counters {
	return Counters{Xacts: atoi(row["xacts"]), BlksHit: atoi(row["blks_hit"]), BlksRead: atoi(row["blks_read"]), MaxConnections: atoi(row["max_connections"])}
}

// ParseStatements reads StatementsSQL rows by queryid.
func ParseStatements(rows []map[string]string) map[string]Statement {
	out := map[string]Statement{}
	for _, r := range rows {
		ms, _ := strconv.ParseFloat(r["total_ms"], 64)
		out[r["queryid"]] = Statement{QueryID: r["queryid"], Calls: atoi(r["calls"]), TotalMS: ms, Query: r["query"]}
	}
	return out
}

// ParsePools reads SHOW POOLS rows, leaving out the admin console and the
// auth aliases.
func ParsePools(pooler string, rows []map[string]string) []Pool {
	var out []Pool
	for _, r := range rows {
		db := r["database"]
		if db == "pgbouncer" || strings.HasSuffix(db, "__auth") {
			continue
		}
		out = append(out, Pool{Pooler: pooler, Database: db, ClActive: atoi(r["cl_active"]), ClWaiting: atoi(r["cl_waiting"]),
			SvActive: atoi(r["sv_active"]), SvIdle: atoi(r["sv_idle"]), MaxWait: atoi(r["maxwait"])})
	}
	return out
}

// StatementDeltas is the work each statement did between prev and cur,
// busiest first. A statement whose counters went down (a reset) counts from
// zero; one with no new calls is left out.
func StatementDeltas(db string, prev, cur map[string]Statement, limit int) []Busy {
	var out []Busy
	for id, c := range cur {
		p, ok := prev[id]
		if !ok || c.Calls < p.Calls {
			p = Statement{}
		}
		calls := c.Calls - p.Calls
		if calls <= 0 {
			continue
		}
		out = append(out, Busy{DB: db, QueryID: id, Calls: calls, TotalMS: c.TotalMS - p.TotalMS, Query: c.Query})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalMS != out[j].TotalMS {
			return out[i].TotalMS > out[j].TotalMS
		}
		return out[i].QueryID < out[j].QueryID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// BlockingChains renders who blocks whom: one line per blocked session,
// "db: 123 (app_app, 42s) waits for 456 (app_migrate)".
func BlockingChains(sessions []Session) []string {
	byPID := map[int]Session{}
	for _, s := range sessions {
		byPID[s.PID] = s
	}
	var out []string
	for _, s := range sessions {
		if len(s.BlockedBy) == 0 {
			continue
		}
		var blockers []string
		for _, pid := range s.BlockedBy {
			b, ok := byPID[pid]
			if ok {
				blockers = append(blockers, fmt.Sprintf("%d (%s, %s)", pid, b.Role, b.State))
			} else {
				blockers = append(blockers, strconv.Itoa(pid))
			}
		}
		out = append(out, fmt.Sprintf("%s: %d (%s, %ds) waits for %s", s.DB, s.PID, s.Role, s.Seconds, strings.Join(blockers, ", ")))
	}
	return out
}

// Build turns this round's samples (and the previous round's, for rates)
// into a snapshot. dbFilter narrows sessions and statements to a database.
func Build(prev, cur map[string]Sample, pools []Pool, dbFilter string, at time.Time) Snapshot {
	snap := Snapshot{At: at, Databases: []DBRow{}, Sessions: []Session{}, Blocking: []string{}, Statements: []Busy{}, Pools: pools}
	names := make([]string, 0, len(cur))
	for name := range cur {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := cur[name]
		row := DBRow{DB: name, Max: s.Counters.MaxConnections, Error: s.Error}
		for _, se := range s.Sessions {
			row.Connections++
			switch {
			case se.State == "active":
				row.Active++
			case strings.HasPrefix(se.State, "idle in transaction"):
				row.IdleInTx++
			}
			if se.State != "idle" && se.Seconds > row.LongestXact {
				row.LongestXact = se.Seconds
			}
		}
		if p, ok := prev[name]; ok && s.Error == "" && p.Error == "" {
			if dt := s.At.Sub(p.At).Seconds(); dt > 0 && s.Counters.Xacts >= p.Counters.Xacts {
				tps := float64(s.Counters.Xacts-p.Counters.Xacts) / dt
				row.TPS = &tps
			}
			hit, read := s.Counters.BlksHit-p.Counters.BlksHit, s.Counters.BlksRead-p.Counters.BlksRead
			if hit+read > 0 && hit >= 0 && read >= 0 {
				ratio := float64(hit) / float64(hit+read)
				row.CacheHit = &ratio
			}
			if dbFilter == "" || dbFilter == name {
				snap.Statements = append(snap.Statements, StatementDeltas(name, p.Statements, s.Statements, 10)...)
			}
		}
		snap.Databases = append(snap.Databases, row)
		if dbFilter == "" || dbFilter == name {
			snap.Sessions = append(snap.Sessions, s.Sessions...)
		}
	}
	snap.Blocking = BlockingChains(snap.Sessions)
	sort.SliceStable(snap.Statements, func(i, j int) bool { return snap.Statements[i].TotalMS > snap.Statements[j].TotalMS })
	if len(snap.Statements) > 10 {
		snap.Statements = snap.Statements[:10]
	}
	return snap
}

// SortSessions orders sessions by key: "time" (longest first), "state" or
// "db".
func SortSessions(sessions []Session, key string) {
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		switch key {
		case "state":
			if a.State != b.State {
				return a.State < b.State
			}
		case "db":
			if a.DB != b.DB {
				return a.DB < b.DB
			}
		}
		return a.Seconds > b.Seconds
	})
}

// Render draws a snapshot in width columns, with the sessions limited to
// fit height rows.
func Render(s Snapshot, filter, sortKey string, width, height int) string {
	var b strings.Builder
	title := "vaire top · " + s.At.Format("15:04:05")
	if filter != "" {
		title += " · " + filter
	}
	b.WriteString(title + " · q quit · d database · s sort (" + sortKey + ")\n\n")
	fmt.Fprintf(&b, "%-16s %7s %7s %7s %8s %7s %8s\n", "DATABASE", "CONNS", "ACTIVE", "IDLE-TX", "TPS", "CACHE", "LONGEST")
	for _, d := range s.Databases {
		tps, cache := "-", "-"
		if d.TPS != nil {
			tps = fmt.Sprintf("%.1f", *d.TPS)
		}
		if d.CacheHit != nil {
			cache = fmt.Sprintf("%.1f%%", *d.CacheHit*100)
		}
		line := fmt.Sprintf("%-16s %7s %7d %7d %8s %7s %7ds", d.DB, fmt.Sprintf("%d/%d", d.Connections, d.Max), d.Active, d.IdleInTx, tps, cache, d.LongestXact)
		if d.Error != "" {
			line = fmt.Sprintf("%-16s %s", d.DB, d.Error)
		}
		b.WriteString(clip(line, width) + "\n")
	}
	if len(s.Pools) > 0 {
		b.WriteString("\nPOOLS\n")
		for _, p := range s.Pools {
			b.WriteString(clip(fmt.Sprintf("  %-11s %-16s clients %d active %d waiting · servers %d active %d idle · maxwait %ds",
				p.Pooler, p.Database, p.ClActive, p.ClWaiting, p.SvActive, p.SvIdle, p.MaxWait), width) + "\n")
		}
	}
	if len(s.Blocking) > 0 {
		b.WriteString("\nBLOCKING\n")
		for _, l := range s.Blocking {
			b.WriteString(clip("  "+l, width) + "\n")
		}
	}
	if len(s.Statements) > 0 {
		b.WriteString("\nBUSIEST STATEMENTS SINCE THE LAST REFRESH\n")
		for _, st := range s.Statements {
			b.WriteString(clip(fmt.Sprintf("  %-12s %6d calls %10.1f ms  %s", st.DB, st.Calls, st.TotalMS, st.Query), width) + "\n")
		}
	}
	sessions := append([]Session(nil), s.Sessions...)
	SortSessions(sessions, sortKey)
	b.WriteString("\nSESSIONS\n")
	used := strings.Count(b.String(), "\n")
	room := height - used - 1
	if room < 3 {
		room = 3
	}
	for i, se := range sessions {
		if i >= room {
			fmt.Fprintf(&b, "  … %d more\n", len(sessions)-i)
			break
		}
		b.WriteString(clip(fmt.Sprintf("  %-12s %7d %-16s %-20s %-18s %6ds  %s", se.DB, se.PID, se.Role, se.State, se.Wait, se.Seconds, se.Query), width) + "\n")
	}
	if len(sessions) == 0 {
		b.WriteString("  no client sessions\n")
	}
	return b.String()
}

func clip(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	if width < 2 {
		return s[:width]
	}
	return s[:width-1] + "…"
}
