// Package health holds the readings eru status and eru doctor share (Phase
// 4): one query per cluster, the poolers' SHOW POOLS, and the arithmetic on
// top. Collecting them is the node's job; this package is pure.
package health

import (
	"strconv"
	"strings"
	"time"
)

// SampleSQL reads one cluster in one round trip, in its application
// database: uptime, size, connections by state, cache counters, transaction
// and WAL counters, archiver state, pending restarts and the oldest
// transaction ID age.
const SampleSQL = `SELECT
  extract(epoch FROM now() - pg_postmaster_start_time())::bigint AS uptime_s,
  pg_database_size(current_database()) AS db_size,
  current_setting('max_connections')::int AS max_connections,
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend') AS connections,
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND state = 'active') AS active,
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND state = 'idle') AS idle,
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND state LIKE 'idle in transaction%') AS idle_in_tx,
  (SELECT coalesce(max(extract(epoch FROM now() - xact_start)), 0)::bigint FROM pg_stat_activity WHERE backend_type = 'client backend' AND xact_start IS NOT NULL) AS longest_xact_s,
  d.blks_hit, d.blks_read, d.xact_commit + d.xact_rollback AS xacts,
  (SELECT wal_bytes FROM pg_stat_wal) AS wal_bytes,
  a.failed_count AS archive_failed,
  coalesce(extract(epoch FROM now() - a.last_archived_time)::bigint, -1) AS last_archived_age_s,
  coalesce(extract(epoch FROM now() - a.last_failed_time)::bigint, -1) AS last_failed_age_s,
  (SELECT count(*) FROM pg_settings WHERE pending_restart) AS pending_restart,
  (SELECT max(age(datfrozenxid)) FROM pg_database) AS max_xid_age,
  (SELECT max(mxid_age(datminmxid)) FROM pg_database) AS max_mxid_age,
  current_setting('data_checksums') AS data_checksums,
  current_setting('ssl') AS ssl,
  current_setting('allow_alter_system') AS allow_alter_system,
  current_setting('listen_addresses') AS listen_addresses,
  current_setting('server_version') AS server_version
FROM pg_stat_database d, pg_stat_archiver a
WHERE d.datname = current_database()`

// XactsSQL reads only the transaction counter, for the second TPS sample.
const XactsSQL = `SELECT xact_commit + xact_rollback AS xacts FROM pg_stat_database WHERE datname = current_database()`

// Sample is one cluster's SampleSQL row.
type Sample struct {
	UptimeS          int64  `json:"uptime_s"`
	DBSize           int64  `json:"db_size"`
	MaxConnections   int64  `json:"max_connections"`
	Connections      int64  `json:"connections"`
	Active           int64  `json:"active"`
	Idle             int64  `json:"idle"`
	IdleInTx         int64  `json:"idle_in_tx"`
	LongestXactS     int64  `json:"longest_xact_s"`
	BlksHit          int64  `json:"blks_hit"`
	BlksRead         int64  `json:"blks_read"`
	Xacts            int64  `json:"xacts"`
	WALBytes         int64  `json:"wal_bytes"`
	ArchiveFailed    int64  `json:"archive_failed"`
	LastArchivedAgeS int64  `json:"last_archived_age_s"`
	LastFailedAgeS   int64  `json:"last_failed_age_s"`
	PendingRestart   int64  `json:"pending_restart"`
	MaxXIDAge        int64  `json:"max_xid_age"`
	MaxMXIDAge       int64  `json:"max_mxid_age"`
	DataChecksums    string `json:"data_checksums"`
	SSL              string `json:"ssl"`
	AllowAlterSystem string `json:"allow_alter_system"`
	ListenAddresses  string `json:"listen_addresses"`
	ServerVersion    string `json:"server_version"`
}

// ParseSample reads a SampleSQL row; a missing or empty column is zero.
func ParseSample(row map[string]string) Sample {
	n := func(k string) int64 {
		v, err := strconv.ParseInt(strings.TrimSpace(row[k]), 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return Sample{
		UptimeS: n("uptime_s"), DBSize: n("db_size"), MaxConnections: n("max_connections"),
		Connections: n("connections"), Active: n("active"), Idle: n("idle"), IdleInTx: n("idle_in_tx"),
		LongestXactS: n("longest_xact_s"),
		BlksHit:      n("blks_hit"), BlksRead: n("blks_read"), Xacts: n("xacts"), WALBytes: n("wal_bytes"),
		ArchiveFailed: n("archive_failed"), LastArchivedAgeS: n("last_archived_age_s"), LastFailedAgeS: n("last_failed_age_s"),
		PendingRestart: n("pending_restart"), MaxXIDAge: n("max_xid_age"), MaxMXIDAge: n("max_mxid_age"),
		DataChecksums: row["data_checksums"], SSL: row["ssl"], AllowAlterSystem: row["allow_alter_system"],
		ListenAddresses: row["listen_addresses"], ServerVersion: row["server_version"],
	}
}

// CacheHit is the buffer cache hit ratio, 0-1; ok is false before any read.
func CacheHit(hit, read int64) (float64, bool) {
	if hit+read <= 0 {
		return 0, false
	}
	return float64(hit) / float64(hit+read), true
}

// TPS is the transaction rate between two counter readings dt apart. A
// counter that went down (stats reset, restart) is 0.
func TPS(before, after int64, dt time.Duration) float64 {
	if dt <= 0 || after < before {
		return 0
	}
	return float64(after-before) / dt.Seconds()
}

// ConnectionUse is connections as a share of max_connections, 0-1.
func ConnectionUse(s Sample) float64 {
	if s.MaxConnections <= 0 {
		return 0
	}
	return float64(s.Connections) / float64(s.MaxConnections)
}

// Pools is a pooler's SHOW POOLS summed over its databases.
type Pools struct {
	ClientsActive  int64 `json:"cl_active"`
	ClientsWaiting int64 `json:"cl_waiting"`
	ServersActive  int64 `json:"sv_active"`
	ServersIdle    int64 `json:"sv_idle"`
	MaxWaitS       int64 `json:"maxwait_s"`
}

// SumPools sums SHOW POOLS rows, leaving out the admin console's own
// pgbouncer database and the auth aliases.
func SumPools(rows []map[string]string) Pools {
	var p Pools
	for _, r := range rows {
		db := r["database"]
		if db == "pgbouncer" || strings.HasSuffix(db, "__auth") {
			continue
		}
		n := func(k string) int64 {
			v, _ := strconv.ParseInt(strings.TrimSpace(r[k]), 10, 64)
			return v
		}
		p.ClientsActive += n("cl_active")
		p.ClientsWaiting += n("cl_waiting")
		p.ServersActive += n("sv_active")
		p.ServersIdle += n("sv_idle")
		if w := n("maxwait"); w > p.MaxWaitS {
			p.MaxWaitS = w
		}
	}
	return p
}

// Uptime renders seconds as 3d4h, 5h12m or 42m.
func Uptime(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds) * time.Second
	days := int64(d / (24 * time.Hour))
	hours := int64(d/time.Hour) % 24
	minutes := int64(d/time.Minute) % 60
	switch {
	case days > 0:
		return strconv.FormatInt(days, 10) + "d" + strconv.FormatInt(hours, 10) + "h"
	case hours > 0:
		return strconv.FormatInt(hours, 10) + "h" + strconv.FormatInt(minutes, 10) + "m"
	default:
		return strconv.FormatInt(minutes, 10) + "m"
	}
}
