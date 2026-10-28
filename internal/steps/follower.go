package steps

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/tune"
)

// Followers (Phase 6): a follower is a cluster of its own that streams from
// its source's cluster over the source's socket. postgres-config-v1 gives it
// one more file, 60-vaire-follower.conf, with the hot-standby floors and the
// replication settings; postgres-roles-v1 skips it, as its roles and
// database are the source's, replicated.

// FloorSettings are the settings a hot standby must have at least as high
// as its primary, or its replay pauses.
var FloorSettings = []string{"max_connections", "max_worker_processes", "max_wal_senders", "max_prepared_transactions", "max_locks_per_transaction"}

// floorDefaults are PostgreSQL 18's defaults of FloorSettings, for a value
// neither the tuning nor the settings give.
var floorDefaults = map[string]int{
	"max_connections": 100, "max_worker_processes": 8, "max_wal_senders": 10,
	"max_prepared_transactions": 0, "max_locks_per_transaction": 64,
}

// FollowerConfPath is a follower's 60-vaire-follower.conf.
func FollowerConfPath(cluster string) string { return PGConfD(cluster) + "/" + render.FollowerConfName }

// effectiveSetting is the value cluster spec runs name with, as far as
// cluster.toml says: [clusters.settings], then [postgres.settings] (both in
// 50-vaire-settings.conf, after the tuning), then the tuning, then
// PostgreSQL's default. ok is false for a value that isn't an integer.
func effectiveSetting(spec config.ClusterSpec, pgSettings map[string]string, res tune.Result, name string) (int, bool) {
	for _, settings := range []map[string]string{spec.Settings, pgSettings} {
		if v, ok := settings[name]; ok {
			n, err := strconv.Atoi(v)
			return n, err == nil
		}
	}
	for _, s := range res.Settings {
		if s.Name == name {
			n, err := strconv.Atoi(s.Value)
			return n, err == nil
		}
	}
	n, ok := floorDefaults[name]
	return n, ok
}

// FollowerFloors are a follower's hot-standby floors: each setting at the
// larger of the follower's own value and its source's (RFC 10.4).
func FollowerFloors(cfg config.Cluster, spec, src config.ClusterSpec, host hostinfo.Info) []render.Floor {
	own, theirs := tuningFor(cfg, spec, host), tuningFor(cfg, src, host)
	out := make([]render.Floor, 0, len(FloorSettings))
	for _, name := range FloorSettings {
		mine, okMine := effectiveSetting(spec, cfg.Postgres.Settings, own, name)
		source, okSource := effectiveSetting(src, cfg.Postgres.Settings, theirs, name)
		if !okMine && !okSource {
			continue
		}
		v, reason := mine, fmt.Sprintf("its own; %s runs %d", src.Name, source)
		if !okMine || (okSource && source > mine) {
			v, reason = source, fmt.Sprintf("%s's, above its own %d", src.Name, mine)
		}
		out = append(out, render.Floor{Name: name, Value: strconv.Itoa(v), Reason: reason})
	}
	return out
}

// FollowerConninfo is a follower's primary_conninfo: the source's socket,
// as postgres with peer authentication (pg_hba's local replication line).
func FollowerConninfo(follower string, src config.ClusterSpec) string {
	return "host=" + PGSocketDir + " port=" + strconv.Itoa(src.Port) + " user=postgres application_name=" + follower
}

// FollowerRestoreCommand fetches WAL from the source's stanza, so a follower
// behind its slot catches up from the archive. Empty without backups or
// without a source stanza.
func FollowerRestoreCommand(cfg config.Cluster, spec, src config.ClusterSpec) string {
	if !cfg.BackupConfigured() || src.Stanza == "" {
		return ""
	}
	return render.PgBackRestBin + " --pg1-path=" + cfg.Server.DataDirectory(spec.Name) + " --stanza=" + src.Stanza + ` archive-get %f "%p"`
}

// followerSettings is what spec's 60-vaire-follower.conf says; ok is false
// for a database that follows nothing (or whose source is gone).
func followerSettings(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info, recs backup.Records) (render.FollowerSettings, bool) {
	if !spec.IsFollower() {
		return render.FollowerSettings{}, false
	}
	src, ok := cfg.ClusterByName(spec.Follows)
	if !ok {
		return render.FollowerSettings{}, false
	}
	return render.FollowerSettings{
		Name: spec.Name, Source: src.Name,
		Floors:         FollowerFloors(cfg, spec, src, host),
		Conninfo:       FollowerConninfo(spec.Name, src),
		Slot:           config.FollowerSlot(spec.Name),
		RestoreCommand: FollowerRestoreCommand(cfg, spec, src),
		Timeline:       recs.TargetTimeline(src.Name, src.Stanza),
	}, true
}

// followerFiles is spec's 60-vaire-follower.conf as a managed file, or none
// for a database that follows nothing. db unfollow removes the file itself.
func followerFiles(cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime) []managedFile {
	if !spec.IsFollower() {
		return nil
	}
	recs, _ := backup.LoadRecords()
	f, ok := followerSettings(cfg, spec, rt.Host, recs)
	if !ok {
		return nil
	}
	return []managedFile{{Path: FollowerConfPath(spec.Name), Content: string(render.FollowerConf(f)), Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup}}
}

// WriteFollowerConf writes spec's whole managed configuration, as
// postgres-config-v1 renders it, before the follower's first start under
// systemd (Phase 6, D2): a standby that started with defaults below its
// source's floors would pause replay at once, and one without
// primary_conninfo would never stream. postgres-config-v1 then finds every
// file in place and only reloads.
func WriteFollowerConf(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) error {
	for _, f := range confFiles(cfg, spec, engine.Runtime{Host: host}) {
		if err := writeFileOwned(f.Path, f.Content, f.Mode, f.Owner, f.Group); err != nil {
			return err
		}
	}
	overrides := PGConfD(spec.Name) + "/90-overrides.conf"
	if _, err := os.Stat(overrides); err != nil {
		if err := writeFileOwned(overrides, string(render.OverridesHeader()), 0o644, pgFileOwner, pgFileGroup); err != nil {
			return err
		}
	}
	return ensureIncludeDir(spec.Name)
}

// RemoveFollowerConf removes a follower's 60-vaire-follower.conf: db unfollow
// does it before the database's own steps run without it.
func RemoveFollowerConf(cluster string) error {
	if err := os.Remove(FollowerConfPath(cluster)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// KillPoolerDatabase drops every client and server connection of db on
// both poolers (KILL), and returns the RESUME that lets new clients in
// again: db unfollow changes the roles a follower's clients logged in as.
// A pooler that doesn't answer is skipped.
func KillPoolerDatabase(ctx context.Context, db string) (resume func()) {
	poolers := []struct {
		dir  string
		port int
	}{{PGSocketDir, PgbouncerPort}, {SessionSocketDir, SessionPoolerPort}}
	for _, p := range poolers {
		_ = poolerAdminAt(ctx, p.dir, p.port, "KILL "+db)
	}
	return func() {
		for _, p := range poolers {
			_ = poolerAdminAt(context.WithoutCancel(ctx), p.dir, p.port, "RESUME "+db)
		}
	}
}

// poolerAdminAt runs one admin console command on the pooler whose socket
// is in dir, as poolerAdmin does for the transaction pooler.
func poolerAdminAt(ctx context.Context, dir string, port int, command string) error {
	ctx, cancel := context.WithTimeout(ctx, poolerAdminTimeout)
	defer cancel()
	return runCmd(ctx, "runuser", "-u", "postgres", "--", "psql", "-X", "-q",
		"-h", dir, "-p", strconv.Itoa(port), "-U", "pgbouncer", "-d", "pgbouncer", "-c", command)
}
