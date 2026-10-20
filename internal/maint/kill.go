package maint

import (
	"strconv"
	"strings"
)

// KillCandidatesSQL lists the client backends of the connected database with
// what vaire maint kill filters on.
const KillCandidatesSQL = `SELECT a.pid, a.usename AS role, coalesce(a.state, '') AS state,
  coalesce(a.application_name, '') AS app,
  coalesce(extract(epoch FROM now() - a.state_change)::bigint, 0) AS state_seconds,
  coalesce(r.rolsuper, false) AS superuser, left(coalesce(a.query, ''), 200) AS query
FROM pg_stat_activity a LEFT JOIN pg_roles r ON r.rolname = a.usename
WHERE a.backend_type = 'client backend' AND a.pid <> pg_backend_pid() AND a.datname = current_database()`

// KillOptions are vaire maint kill's filters.
type KillOptions struct {
	PID             int   `json:"pid,omitempty"`
	IdleInTx        bool  `json:"idle_in_tx,omitempty"`
	OlderThanSecond int64 `json:"older_than_s,omitempty"`
}

// Backend is one candidate for termination.
type Backend struct {
	PID      int    `json:"pid"`
	Role     string `json:"role"`
	State    string `json:"state"`
	App      string `json:"application_name,omitempty"`
	Seconds  int64  `json:"state_seconds"`
	Super    bool   `json:"superuser"`
	Query    string `json:"query"`
	Refusal  string `json:"refusal,omitempty"`
	Selected bool   `json:"-"`
}

// ParseBackends reads KillCandidatesSQL rows.
func ParseBackends(rows []map[string]string) []Backend {
	out := make([]Backend, 0, len(rows))
	for _, r := range rows {
		pid, _ := strconv.Atoi(r["pid"])
		secs, _ := strconv.ParseInt(r["state_seconds"], 10, 64)
		out = append(out, Backend{PID: pid, Role: r["role"], State: r["state"], App: r["app"], Seconds: secs,
			Super: r["superuser"] == "t" || r["superuser"] == "true", Query: r["query"]})
	}
	return out
}

// protectedRole is a role whose sessions vaire maint kill never picks by a
// filter: the superuser and vaire's own and the poolers' service roles.
func protectedRole(role string) bool {
	return role == "postgres" || role == "pgbouncer_auth" || strings.HasPrefix(role, "vaire_")
}

// SelectBackends picks what o terminates. A filter (--idle-in-tx) never
// picks a protected role or vaire top; --pid picks that backend, but a
// superuser's is refused (pg_signal_backend can't signal it anyway).
func SelectBackends(all []Backend, o KillOptions) (selected, refused []Backend) {
	for _, b := range all {
		switch {
		case o.PID != 0:
			if b.PID != o.PID {
				continue
			}
			if b.Super {
				b.Refusal = "a superuser backend"
				refused = append(refused, b)
				continue
			}
			selected = append(selected, b)
		case o.IdleInTx:
			if !strings.HasPrefix(b.State, "idle in transaction") {
				continue
			}
			if o.OlderThanSecond > 0 && b.Seconds < o.OlderThanSecond {
				continue
			}
			if b.Super || protectedRole(b.Role) || b.App == "vaire top" {
				continue
			}
			selected = append(selected, b)
		}
	}
	return selected, refused
}

// TerminateSQL terminates the backends with pids.
func TerminateSQL(pids []int) string {
	parts := make([]string, 0, len(pids))
	for _, p := range pids {
		parts = append(parts, strconv.Itoa(p))
	}
	return "SELECT pid, pg_terminate_backend(pid) AS terminated FROM unnest(ARRAY[" + strings.Join(parts, ",") + "]::int[]) AS pid"
}
