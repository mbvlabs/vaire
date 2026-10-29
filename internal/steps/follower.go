package steps

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

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

// floorValue parses one of FloorSettings' values as PostgreSQL 18's parse_int
// reads a setting without a unit: strtol base 0 first (leading whitespace, a
// sign, 0x hexadecimal, leading-0 octal or decimal), the whole value again
// with strtod when that stops at '.', 'e' or 'E' or overflows, then rint (ties
// to even), trailing whitespace only and a 32-bit range. None of FloorSettings
// takes a unit, so a suffix is an error: a floor is never silently dropped for
// a value the source runs.
func floorValue(name, value string) (int, error) {
	n, end, ok, overflow := strtolBase0(value)
	val := float64(n)
	retry := overflow
	if end < len(value) {
		switch value[end] {
		case '.', 'e', 'E':
			retry = true
		}
	}
	if retry {
		val, end, ok, overflow = strtodWhole(value)
	}
	if !ok || overflow {
		return 0, floorValueError(name, value)
	}
	if end = skipCSpace(value, end); end != len(value) {
		return 0, floorValueError(name, value)
	}
	if math.IsNaN(val) {
		return 0, floorValueError(name, value)
	}
	rounded := math.RoundToEven(val)
	if rounded < math.MinInt32 || rounded > math.MaxInt32 {
		return 0, fmt.Errorf("%s %q is out of range for a PostgreSQL integer", name, value)
	}
	return int(rounded), nil
}

// floorValueError says why PostgreSQL would not read a floor's value.
func floorValueError(name, value string) error {
	return fmt.Errorf("%s %q is not a PostgreSQL integer: PostgreSQL reads a decimal, 0x hexadecimal or leading-0 octal number, rounds a fraction to the nearest integer, and takes no unit for %s", name, value, name)
}

// strtolBase0 reads value as C strtol with base 0: leading space, an optional
// sign, then 0x hexadecimal, leading-0 octal or decimal digits. end is the
// first byte after the number (0 when there is no number), and overflow is
// errno ERANGE.
func strtolBase0(value string) (n int64, end int, ok, overflow bool) {
	i := skipCSpace(value, 0)
	if i == len(value) {
		return 0, 0, false, false
	}
	negative := false
	if value[i] == '+' || value[i] == '-' {
		negative, i = value[i] == '-', i+1
	}
	base, digits := 10, i
	if i < len(value) && value[i] == '0' {
		base, digits = 8, i
		if i+2 < len(value) && (value[i+1] == 'x' || value[i+1] == 'X') && hexDigit(value[i+2]) {
			base, digits = 16, i+2
		}
	}
	for i = digits; i < len(value) && digitInBase(value[i], base); i++ {
	}
	if i == digits {
		return 0, 0, false, false
	}
	mag, err := strconv.ParseUint(value[digits:i], base, 64)
	if err != nil {
		return 0, i, true, true
	}
	if negative {
		switch {
		case mag > 1<<63:
			return 0, i, true, true
		case mag == 1<<63:
			return math.MinInt64, i, true, false
		}
		return -int64(mag), i, true, false //nolint:gosec // G115: mag < 2^63 here, so the conversion is exact.
	}
	if mag > math.MaxInt64 {
		return 0, i, true, true
	}
	return int64(mag), i, true, false
}

// strtodWhole reads value as C strtod: leading space, an optional sign, then a
// decimal or 0x hexadecimal number with an optional exponent, inf or nan. The
// whole remainder must parse (parse_int allows trailing space only).
func strtodWhole(value string) (f float64, end int, ok, overflow bool) {
	i := skipCSpace(value, 0)
	start := i
	negative := false
	if i < len(value) && (value[i] == '+' || value[i] == '-') {
		negative, i = value[i] == '-', i+1
	}
	lower := strings.ToLower(value[i:])
	for _, special := range []struct {
		text string
		val  float64
	}{{"infinity", math.Inf(1)}, {"inf", math.Inf(1)}, {"nan", math.NaN()}} {
		if strings.HasPrefix(lower, special.text) {
			f = special.val
			if negative {
				f = -f
			}
			return f, i + len(special.text), true, false
		}
	}
	if i+1 < len(value) && value[i] == '0' && (value[i+1] == 'x' || value[i+1] == 'X') {
		j := i + 2
		digits := 0
		for j < len(value) && hexDigit(value[j]) {
			j, digits = j+1, digits+1
		}
		if j < len(value) && value[j] == '.' {
			j++
			for j < len(value) && hexDigit(value[j]) {
				j, digits = j+1, digits+1
			}
		}
		if digits > 0 {
			end = j
			if j < len(value) && (value[j] == 'p' || value[j] == 'P') {
				if k, ok := exponentEnd(value, j+1); ok {
					end = k
				}
			}
			literal := value[start:end]
			if !strings.ContainsAny(literal, "pP") {
				literal += "p0"
			}
			return parseFloat(literal, end)
		}
	}
	j, digits := i, 0
	for j < len(value) && decimalDigit(value[j]) {
		j, digits = j+1, digits+1
	}
	if j < len(value) && value[j] == '.' {
		j++
		for j < len(value) && decimalDigit(value[j]) {
			j, digits = j+1, digits+1
		}
	}
	if digits == 0 {
		return 0, 0, false, false
	}
	end = j
	if end < len(value) && (value[end] == 'e' || value[end] == 'E') {
		if k, ok := exponentEnd(value, end+1); ok {
			end = k
		}
	}
	return parseFloat(value[start:end], end)
}

// exponentEnd is the first byte after a decimal exponent's digits, beginning
// at i (after e, E, p or P and its optional sign). ok is false when there are
// no digits, so the exponent is not part of the number.
func exponentEnd(value string, i int) (end int, ok bool) {
	if i < len(value) && (value[i] == '+' || value[i] == '-') {
		i++
	}
	start := i
	for i < len(value) && decimalDigit(value[i]) {
		i++
	}
	if i == start {
		return 0, false
	}
	return i, true
}

// parseFloat reads literal, rejecting a value that overflows a double or
// underflows it: strtod reports ERANGE for a nonzero literal that comes out
// zero or subnormal, which PostgreSQL rejects, where Go returns no error.
func parseFloat(literal string, end int) (float64, int, bool, bool) {
	f, err := strconv.ParseFloat(literal, 64)
	if err == nil {
		if math.Abs(f) < 0x1p-1022 && nonzeroMantissa(literal) {
			return 0, end, true, true
		}
		return f, end, true, false
	}
	var numErr *strconv.NumError
	if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
		return 0, end, true, true
	}
	return 0, 0, false, false
}

// nonzeroMantissa reports whether a decimal or 0x hexadecimal float literal
// has a nonzero digit before its exponent.
func nonzeroMantissa(literal string) bool {
	s := strings.TrimLeft(literal, "+-")
	exp := "eE"
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s, exp = s[2:], "pP"
	}
	if i := strings.IndexAny(s, exp); i >= 0 {
		s = s[:i]
	}
	return strings.ContainsAny(s, "123456789abcdefABCDEF")
}

// decimalDigit reports an ASCII decimal digit.
func decimalDigit(c byte) bool { return c >= '0' && c <= '9' }

// hexDigit reports an ASCII hexadecimal digit.
func hexDigit(c byte) bool {
	return decimalDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// digitInBase reports whether c is a digit in base.
func digitInBase(c byte, base int) bool {
	switch base {
	case 8:
		return c >= '0' && c <= '7'
	case 16:
		return hexDigit(c)
	}
	return decimalDigit(c)
}

// skipCSpace skips the bytes C isspace accepts.
func skipCSpace(value string, i int) int {
	for i < len(value) {
		switch value[i] {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// effectiveSetting is the value cluster spec runs name with, as far as
// cluster.toml says: [clusters.settings], then [postgres.settings] (both in
// 50-vaire-settings.conf, after the tuning), then the tuning, then
// PostgreSQL's default. owner names spec in the error of a value PostgreSQL
// cannot read.
func effectiveSetting(owner string, spec config.ClusterSpec, pgSettings map[string]string, res tune.Result, name string) (int, error) {
	for _, settings := range []map[string]string{spec.Settings, pgSettings} {
		if v, ok := settings[name]; ok {
			n, err := floorValue(name, v)
			if err != nil {
				return 0, fmt.Errorf("%s: %w", owner, err)
			}
			return n, nil
		}
	}
	for _, s := range res.Settings {
		if s.Name == name {
			return floorValue(name, s.Value)
		}
	}
	n, ok := floorDefaults[name]
	if !ok {
		return 0, fmt.Errorf("%s: no PostgreSQL default is known for %s", owner, name)
	}
	return n, nil
}

// floorPair is one hot-standby floor's effective value on the follower and on
// its source.
type floorPair struct {
	name         string
	mine, source int
}

// followerFloorPairs are the effective floor values of spec, a follower of
// src, as PostgreSQL runs them: each setting at the larger of the follower's
// own value and its source's (RFC 10.4).
func followerFloorPairs(cfg config.Cluster, spec, src config.ClusterSpec, host hostinfo.Info) ([]floorPair, error) {
	own, theirs := tuningFor(cfg, spec, host), tuningFor(cfg, src, host)
	out := make([]floorPair, 0, len(FloorSettings))
	for _, name := range FloorSettings {
		mine, err := effectiveSetting(spec.Name, spec, cfg.Postgres.Settings, own, name)
		if err != nil {
			return nil, fmt.Errorf("%s's hot-standby floors: %w", spec.Name, err)
		}
		source, err := effectiveSetting(src.Name, src, cfg.Postgres.Settings, theirs, name)
		if err != nil {
			return nil, fmt.Errorf("%s's hot-standby floors: %w", spec.Name, err)
		}
		out = append(out, floorPair{name: name, mine: mine, source: source})
	}
	return out, nil
}

// FollowerFloors are a follower's hot-standby floors: each setting at the
// larger of the follower's own value and its source's (RFC 10.4).
func FollowerFloors(cfg config.Cluster, spec, src config.ClusterSpec, host hostinfo.Info) ([]render.Floor, error) {
	pairs, err := followerFloorPairs(cfg, spec, src, host)
	if err != nil {
		return nil, err
	}
	out := make([]render.Floor, 0, len(pairs))
	for _, p := range pairs {
		v, reason := p.mine, fmt.Sprintf("its own; %s runs %d", src.Name, p.source)
		if p.source > p.mine {
			v, reason = p.source, fmt.Sprintf("%s's, above its own %d", src.Name, p.mine)
		}
		out = append(out, render.Floor{Name: p.name, Value: strconv.Itoa(v), Reason: reason})
	}
	return out, nil
}

// FollowerFloorDrop is one hot-standby floor that a source change would lower:
// the follower, the setting, and its effective value before and after.
type FollowerFloorDrop struct {
	Follower string `json:"follower"`
	Setting  string `json:"setting"`
	From     int    `json:"from"`
	To       int    `json:"to"`
}

// FollowerFloorDrops lists the hot-standby floors that would fall on any of
// db's followers when db's entry changes from was to now (Phase 6, Q1a). A
// resize that lowers one is refused: until the follower has restarted above
// the old values its replay can pause, and a retry cannot order that restart
// around the source.
func FollowerFloorDrops(cfg config.Cluster, db string, was, now config.ClusterSpec, host hostinfo.Info) ([]FollowerFloorDrop, error) {
	var out []FollowerFloorDrop
	for _, f := range cfg.FollowersOf(db) {
		before, err := followerFloorPairs(cfg, f, was, host)
		if err != nil {
			return nil, err
		}
		after, err := followerFloorPairs(cfg, f, now, host)
		if err != nil {
			return nil, err
		}
		for i := range after {
			if floorOf(after[i]) >= floorOf(before[i]) {
				continue
			}
			out = append(out, FollowerFloorDrop{Follower: f.Name, Setting: after[i].name, From: floorOf(before[i]), To: floorOf(after[i])})
		}
	}
	return out, nil
}

// floorOf is the larger of a pair's two values: the floor PostgreSQL runs.
func floorOf(p floorPair) int { return max(p.mine, p.source) }

// FollowerFloorsChange reports whether spec's floors change when its source
// changes from was to now. Every floor setting is a postmaster setting, so a
// changed floor needs the follower's restart.
func FollowerFloorsChange(cfg config.Cluster, spec, was, now config.ClusterSpec, host hostinfo.Info) (bool, error) {
	before, err := followerFloorPairs(cfg, spec, was, host)
	if err != nil {
		return false, err
	}
	after, err := followerFloorPairs(cfg, spec, now, host)
	if err != nil {
		return false, err
	}
	for i := range after {
		if floorOf(after[i]) != floorOf(before[i]) {
			return true, nil
		}
	}
	return false, nil
}

// FloorsDiffer reports whether diff, a unified diff of a follower's
// 60-vaire-follower.conf from the file on disk to the rendered one, changes a
// hot-standby floor's value. Every floor is a postmaster setting, so such a
// change needs the follower's restart; a changed reason alone does not.
func FloorsDiffer(diff string) bool {
	removed, added := map[string]string{}, map[string]string{}
	for line := range strings.SplitSeq(diff, "\n") {
		if line == "" || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		into := added
		switch line[0] {
		case '-':
			into = removed
		case '+':
		default:
			continue
		}
		setting, _, _ := strings.Cut(line[1:], "#")
		name, value, ok := strings.Cut(setting, "=")
		if ok {
			into[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	for _, name := range FloorSettings {
		if removed[name] != added[name] {
			return true
		}
	}
	return false
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
func followerSettings(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info, recs backup.Records) (render.FollowerSettings, bool, error) {
	if !spec.IsFollower() {
		return render.FollowerSettings{}, false, nil
	}
	src, ok := cfg.ClusterByName(spec.Follows)
	if !ok {
		return render.FollowerSettings{}, false, nil
	}
	floors, err := FollowerFloors(cfg, spec, src, host)
	if err != nil {
		return render.FollowerSettings{}, false, err
	}
	return render.FollowerSettings{
		Name: spec.Name, Source: src.Name,
		Floors:         floors,
		Conninfo:       FollowerConninfo(spec.Name, src),
		Slot:           config.FollowerSlot(spec.Name),
		RestoreCommand: FollowerRestoreCommand(cfg, spec, src),
		Timeline:       recs.TargetTimeline(src.Name, src.Stanza),
	}, true, nil
}

// followerFiles is spec's 60-vaire-follower.conf as a managed file, or none
// for a database that follows nothing. db unfollow removes the file itself.
func followerFiles(cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime) ([]managedFile, error) {
	if !spec.IsFollower() {
		return nil, nil
	}
	recs, _ := backup.LoadRecords()
	f, ok, err := followerSettings(cfg, spec, rt.Host, recs)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return []managedFile{{Path: FollowerConfPath(spec.Name), Content: string(render.FollowerConf(f)), Mode: 0o644, Owner: pgFileOwner, Group: pgFileGroup}}, nil
}

// WriteFollowerConf writes spec's whole managed configuration, as
// postgres-config-v1 renders it, before the follower's first start under
// systemd (Phase 6, D2): a standby that started with defaults below its
// source's floors would pause replay at once, and one without
// primary_conninfo would never stream. postgres-config-v1 then finds every
// file in place and only reloads.
func WriteFollowerConf(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) error {
	files, err := confFiles(cfg, spec, engine.Runtime{Host: host})
	if err != nil {
		return err
	}
	for _, f := range files {
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
