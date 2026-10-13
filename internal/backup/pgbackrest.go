package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
)

// ConfPath is pgBackRest's configuration (postgres:postgres 0600).
const ConfPath = "/etc/pgbackrest/pgbackrest.conf"

// PgBackRest is the Driver over the pgbackrest binary. Every command runs as
// the postgres OS user through the engine shell, so its output is redacted
// and lands in the run log; no argument carries a secret, because the keys
// and cipher pass live in ConfPath.
type PgBackRest struct {
	Stanza string
	// Port is the cluster's pg1-port, passed to the commands that connect
	// to it. pgbackrest.conf's stanza section names it too.
	Port  int
	Shell engine.Shell
	// StepID labels the log events, for example the job name.
	StepID string
	Report engine.Reporter
	// PG reads pg_stat_archiver on the live cluster.
	PG pg.Local
}

var _ Driver = PgBackRest{}

func (p PgBackRest) run(ctx context.Context, stdout *bytes.Buffer, onLine func(string), args ...string) error {
	full := []string{"-u", "postgres", "--", "pgbackrest"}
	if p.Stanza != "" {
		full = append(full, "--stanza="+p.Stanza)
	}
	full = append(full, args...)
	c := engine.Command{Name: "runuser", Args: full, OnLine: onLine}
	if stdout != nil {
		c.Stdout = stdout
	}
	return p.Shell.Exec(ctx, p.StepID, c, p.Report)
}

// StanzaCreate runs stanza-create, which also checks an existing stanza
// against this cluster's system identifier.
func (p PgBackRest) StanzaCreate(ctx context.Context) error {
	return wrapTool("pgbackrest stanza-create", p.run(ctx, nil, nil, p.withPort("stanza-create")...))
}

// withPort puts --pg1-port before the commands that connect to the cluster.
func (p PgBackRest) withPort(args ...string) []string {
	if p.Port == 0 {
		return args
	}
	return append([]string{"--pg1-port=" + strconv.Itoa(p.Port)}, args...)
}

// Check runs pgbackrest check: it forces a WAL switch and waits for the
// segment to reach the repository.
func (p PgBackRest) Check(ctx context.Context) error {
	return wrapTool("pgbackrest check", p.run(ctx, nil, nil, p.withPort("check")...))
}

var newLabel = regexp.MustCompile(`new backup label = (\S+)`)

// Backup takes one backup, streaming progress events.
func (p PgBackRest) Backup(ctx context.Context, kind string) (Set, error) {
	switch kind {
	case TypeFull, TypeDiff, TypeIncr:
	default:
		return Set{}, fmt.Errorf("backup type %q must be full, diff or incr", kind)
	}
	prog := newProgress("backup", p.Report)
	label := ""
	onLine := func(line string) {
		prog.line(line)
		if m := newLabel.FindStringSubmatch(line); m != nil {
			label = m[1]
		}
	}
	if err := p.run(ctx, nil, onLine, p.withPort("--type="+kind, "--log-level-console=detail", "backup")...); err != nil {
		return Set{}, wrapTool("pgbackrest backup", err)
	}
	info, err := p.Info(ctx, "")
	if err != nil {
		return Set{}, err
	}
	if label != "" {
		if set, ok := info.Find(label); ok {
			return set, nil
		}
	}
	latest, ok := info.Latest()
	if !ok {
		return Set{}, errors.New("pgbackrest reported success but the repository lists no backup")
	}
	return latest, nil
}

// infoJSON is the part of pgbackrest info --output=json eru reads.
type infoJSON struct {
	Name   string `json:"name"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Lock    struct {
			Backup struct {
				Held bool `json:"held"`
			} `json:"backup"`
		} `json:"lock"`
	} `json:"status"`
	Archive []struct {
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"archive"`
	Backup []struct {
		Label   string  `json:"label"`
		Type    string  `json:"type"`
		Prior   *string `json:"prior"`
		Error   bool    `json:"error"`
		Archive struct {
			Start string `json:"start"`
			Stop  string `json:"stop"`
		} `json:"archive"`
		Info struct {
			Size       int64 `json:"size"`
			Delta      int64 `json:"delta"`
			Repository struct {
				Size  int64 `json:"size"`
				Delta int64 `json:"delta"`
			} `json:"repository"`
		} `json:"info"`
		Timestamp struct {
			Start int64 `json:"start"`
			Stop  int64 `json:"stop"`
		} `json:"timestamp"`
		DatabaseRef []struct {
			Name string `json:"name"`
		} `json:"database-ref"`
	} `json:"backup"`
}

// Info runs pgbackrest info --output=json for the driver's stanza.
func (p PgBackRest) Info(ctx context.Context, label string) (Info, error) {
	args := []string{"--output=json"}
	if label != "" {
		args = append(args, "--set="+label)
	}
	stanzas, err := p.info(ctx, args...)
	if err != nil {
		return Info{}, err
	}
	for _, st := range stanzas {
		if st.Name == p.Stanza {
			return parseInfo(st), nil
		}
	}
	return Info{Stanza: p.Stanza}, nil
}

// InfoAll runs pgbackrest info --output=json once for every stanza in the
// repository, keyed by stanza. The driver's own stanza is ignored.
func (p PgBackRest) InfoAll(ctx context.Context) (map[string]Info, error) {
	all := p
	all.Stanza = ""
	stanzas, err := all.info(ctx, "--output=json")
	if err != nil {
		return nil, err
	}
	out := make(map[string]Info, len(stanzas))
	for _, st := range stanzas {
		out[st.Name] = parseInfo(st)
	}
	return out, nil
}

func (p PgBackRest) info(ctx context.Context, args ...string) ([]infoJSON, error) {
	var stdout bytes.Buffer
	quiet := p
	quiet.Report = nil
	if err := quiet.run(ctx, &stdout, nil, append(args, "info")...); err != nil {
		return nil, wrapTool("pgbackrest info", err)
	}
	var stanzas []infoJSON
	if err := json.Unmarshal(stdout.Bytes(), &stanzas); err != nil {
		return nil, fmt.Errorf("pgbackrest info output does not parse: %w", err)
	}
	return stanzas, nil
}

// parseInfo turns one stanza of pgbackrest info into an Info.
func parseInfo(st infoJSON) Info {
	out := Info{Stanza: st.Name}
	out.StatusCode, out.Status = st.Status.Code, st.Status.Message
	out.BackupLockHeld = st.Status.Lock.Backup.Held
	for _, a := range st.Archive {
		if out.ArchiveMin == "" || a.Min < out.ArchiveMin {
			out.ArchiveMin = a.Min
		}
		if a.Max > out.ArchiveMax {
			out.ArchiveMax = a.Max
		}
	}
	for _, b := range st.Backup {
		// pgBackRest leaves repository.size out of bundled backups; what
		// the set stored (its delta) is then the best figure.
		repoSize := b.Info.Repository.Size
		if repoSize == 0 {
			repoSize = b.Info.Repository.Delta
		}
		set := Set{
			Label:     b.Label,
			Type:      b.Type,
			Start:     unixTime(b.Timestamp.Start),
			Stop:      unixTime(b.Timestamp.Stop),
			Size:      b.Info.Size,
			Delta:     b.Info.Delta,
			RepoSize:  repoSize,
			RepoDelta: b.Info.Repository.Delta,
			WALStart:  b.Archive.Start,
			WALStop:   b.Archive.Stop,
			Error:     b.Error,
		}
		if b.Prior != nil {
			set.Prior = *b.Prior
		}
		for _, d := range b.DatabaseRef {
			set.Databases = append(set.Databases, d.Name)
		}
		out.Sets = append(out.Sets, set)
	}
	return out
}

// networkProcessMax is process-max for verify and restore. Both mostly wait
// on the bucket, so they gain from more processes than the vCPU-based
// default in pgbackrest.conf.
const networkProcessMax = "--process-max=4"

// Verify runs pgbackrest verify on the driver's stanza (RFC 9.4), so a big
// repository is checked one stanza at a time.
func (p PgBackRest) Verify(ctx context.Context) (VerifyResult, error) {
	res := VerifyResult{StartedAt: now()}
	var lines []string
	err := p.run(ctx, nil, func(line string) {
		lines = append(lines, line)
		if len(lines) > 40 {
			lines = lines[1:]
		}
	}, networkProcessMax, "verify")
	res.FinishedAt = now()
	res.Output = lines
	var scriptErr *engine.ScriptError
	switch {
	case err == nil:
		res.OK = true
	case errors.As(err, &scriptErr):
		// verify exits non-zero when it finds a problem: a result, not an
		// error of eru's.
		res.OK = false
	default:
		return res, err
	}
	return res, nil
}

// StanzaDelete runs stop, then stanza-delete --force, then start, which
// removes the stop file again. --force is needed because pgBackRest otherwise
// checks that PostgreSQL is down, and the dropped cluster is gone. The
// options come from the command line: a dropped stanza has no section in
// pgbackrest.conf once pgbackrest-v1 re-renders it.
func (p PgBackRest) StanzaDelete(ctx context.Context, pgPath string) error {
	if p.Stanza == "" {
		return errors.New("stanza-delete needs a stanza")
	}
	if err := wrapTool("pgbackrest stop", p.run(ctx, nil, nil, "stop")); err != nil {
		return err
	}
	if err := wrapTool("pgbackrest stanza-delete", p.run(ctx, nil, nil, "--pg1-path="+pgPath, "--force", "stanza-delete")); err != nil {
		return err
	}
	return wrapTool("pgbackrest start", p.run(ctx, nil, nil, "start"))
}

// RestoreScratch restores into opts.Path with archive_mode off, so the
// restored instance never pushes WAL to the repository. The path is always
// empty, so the delta=y meant for the live cluster is turned off.
func (p PgBackRest) RestoreScratch(ctx context.Context, opts RestoreOptions) error {
	args := []string{"--pg1-path=" + opts.Path, "--archive-mode=off", "--log-level-console=detail", "--no-delta", networkProcessMax}
	if opts.Database != "" {
		args = append(args, "--db-include="+opts.Database)
	}
	switch opts.Kind {
	case TargetLatest, "":
		args = append(args, "--type=default")
	case TargetTime:
		args = append(args, "--type=time", "--target="+PgTime(opts.Time), "--target-action=promote")
	case TargetSet:
		args = append(args, "--set="+opts.Set, "--type=immediate", "--target-action=promote")
	default:
		return fmt.Errorf("restore target %q is unknown", opts.Kind)
	}
	prog := newProgress("restore", p.Report)
	return wrapTool("pgbackrest restore", p.run(ctx, nil, prog.line, append(args, "restore")...))
}

// ArchiveStatus reads pg_stat_archiver.
func (p PgBackRest) ArchiveStatus(ctx context.Context) (ArchiveStatus, error) {
	rows, err := p.PG.Query(ctx, "postgres", `SELECT archived_count, coalesce(last_archived_wal, '') AS last_archived_wal,
  coalesce(to_char(last_archived_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS last_archived_time,
  failed_count, coalesce(last_failed_wal, '') AS last_failed_wal,
  coalesce(to_char(last_failed_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS last_failed_time
FROM pg_stat_archiver`, nil)
	if err != nil {
		return ArchiveStatus{}, err
	}
	if len(rows) == 0 {
		return ArchiveStatus{}, nil
	}
	row := rows[0]
	st := ArchiveStatus{
		LastArchivedWAL:  row["last_archived_wal"],
		LastArchivedTime: row["last_archived_time"],
		LastFailedWAL:    row["last_failed_wal"],
		LastFailedTime:   row["last_failed_time"],
	}
	st.ArchivedCount, _ = strconv.ParseInt(row["archived_count"], 10, 64)
	st.FailedCount, _ = strconv.ParseInt(row["failed_count"], 10, 64)
	if t, err := time.Parse(time.RFC3339, st.LastArchivedTime); err == nil {
		st.LagSeconds = int64(time.Since(t).Seconds())
	}
	return st, nil
}

// PgTime formats t as pgBackRest's recovery target.
func PgTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000") + "+00"
}

func unixTime(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// wrapTool turns a failed backup tool into external_command_failed with its
// last output lines.
func wrapTool(what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, engine.ErrInterrupted) {
		return err
	}
	details := map[string]any{"command": what}
	var scriptErr *engine.ScriptError
	if errors.As(err, &scriptErr) {
		details["exit_status"] = scriptErr.Status
		details["output_tail"] = scriptErr.Tail
	}
	return output.WrapError(output.CodeExternalCommand, fmt.Errorf("%s failed: %w", what, err), output.ExitExternal,
		"Read details.output_tail, or the run log under /var/log/eru and /var/log/pgbackrest.").WithDetails(details)
}
