package steps

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"github.com/mbvlabs/eru/internal/render"
)

const timersID = "timers-v1"

// CertsTimerName is the hourly certificate timer timers-v1 owns.
const CertsTimerName = "eru-certs.timer"

// ownedService matches the template services timers-v1 writes, so a removed
// schedule takes its service with it. eru-certs.service and eru-thp.service
// belong to other steps.
var ownedService = regexp.MustCompile(`^eru-((backup-(full|diff|incr|verify)|restore-drill|db-dump)@|stanza-expiry|maint-daily|maint-weekly|metrics-refresh|doctor|master-backup|fence)\.service$`)

// Master backup units (Phase 5): the job, the path unit that starts it after
// a change, its daily safety-net timer, and with a bucket the epoch fence
// check at boot and every five minutes.
const (
	MasterBackupService = "eru-master-backup.service"
	MasterBackupPath    = "eru-master-backup.path"
	MasterBackupTimer   = "eru-master-backup.timer"
	FenceService        = "eru-fence.service"
	FenceTimer          = "eru-fence.timer"
)

// masterBackupUnits are the master backup's units.
func masterBackupUnits(cfg config.Cluster, start string) map[string][]byte {
	files := map[string][]byte{
		MasterBackupService: render.JobService("master-backup", ""),
		MasterBackupPath:    render.MasterBackupPath(masterbackup.DirtyPath),
		MasterBackupTimer:   render.JobTimer("eru master backup safety net (daily)", "*-*-* "+start+":00", "30m"),
	}
	if cfg.BackupConfigured() {
		files[FenceService] = render.FenceService()
		files[FenceTimer] = render.JobTimer("eru epoch fence check", "*:0/5", "30s")
	}
	return files
}

// enabledUnit reports whether timers-v1 enables name: every timer and path
// unit, and the fence check, which runs at boot.
func enabledUnit(name string) bool {
	return strings.HasSuffix(name, ".timer") || strings.HasSuffix(name, ".path") || name == FenceService
}

// serverTimers are the server-wide timers of Phase 4 (D13), which run with or
// without backups: maintenance at the start of the window, the textfile
// metrics every minute, doctor every hour.
func serverTimers(cfg config.Cluster) map[string][]byte {
	start := "02:00"
	if s, _, err := config.ParseWindow(cfg.Maintenance.Window); err == nil {
		start = fmt.Sprintf("%02d:%02d", int(s.Hours()), int(s.Minutes())%60)
	}
	files := map[string][]byte{}
	for _, t := range []struct{ job, description, schedule, jitter string }{
		{"maint-daily", "eru daily maintenance (wraparound, ANALYZE of stale tables, deferred restarts)", "*-*-* " + start + ":00", "10m"},
		{"maint-weekly", "eru weekly maintenance (bloat estimate)", "Sat *-*-* " + start + ":00", "10m"},
		{"metrics-refresh", "eru textfile metrics refresh", "minutely", "0"},
		{"doctor", "eru doctor checks for the metrics", "hourly", "10m"},
	} {
		files["eru-"+t.job+".service"] = render.JobService(t.job, "")
		files["eru-"+t.job+".timer"] = render.JobTimer(t.description, t.schedule, t.jitter)
	}
	for name, content := range masterBackupUnits(cfg, start) {
		files[name] = content
	}
	return files
}

// StanzaExpiryTimerName is the daily timer that deletes the stanzas of
// dropped databases once their window has passed (RFC 9.1).
const StanzaExpiryTimerName = "eru-stanza-expiry.timer"

// stanzaExpirySchedule runs after the default maintenance window, when the
// night's backups are done.
const stanzaExpirySchedule = "*-*-* 06:00:00"

// unitNamePattern guards the unit names handed to the script.
var unitNamePattern = regexp.MustCompile(`^eru-[a-z0-9@_.-]+\.(timer|service|path)$`)

// timerJob is one scheduled job of one cluster (RFC 9.2).
type timerJob struct {
	Job         string
	Description string
	Schedule    string
	Jitter      string
}

// scheduledJobs are spec's jobs with a schedule: its own, else
// [backup.database]'s for the backups, verify and drill. Dumps have no
// server default.
func scheduledJobs(cfg config.Cluster, spec config.ClusterSpec) []timerJob {
	db, own := cfg.Backup.Database, spec.Backup
	pick := func(own, def string) string {
		if own != "" {
			return own
		}
		return def
	}
	all := []timerJob{
		{"backup-full", "eru full backup of " + spec.Name, pick(own.FullSchedule, db.FullSchedule), "5m"},
		{"backup-diff", "eru differential backup of " + spec.Name, pick(own.DiffSchedule, db.DiffSchedule), "5m"},
		{"backup-incr", "eru incremental backup of " + spec.Name, pick(own.IncrSchedule, db.IncrSchedule), "2m"},
		{"backup-verify", "eru backup verify of " + spec.Name, pick(own.VerifySchedule, db.VerifySchedule), "10m"},
		{"restore-drill", "eru restore drill of " + spec.Name, pick(own.DrillSchedule, db.DrillSchedule), "30m"},
		{"db-dump", "eru long-retention dump of " + spec.Name, own.DumpSchedule, "10m"},
	}
	var out []timerJob
	for _, j := range all {
		if j.Schedule != "" {
			out = append(out, j)
		}
	}
	return out
}

// timerFiles returns every unit file timers-v1 owns, by file name: the
// certificate timer, and with backups on the stanza-expiry timer, a template
// service per job (eru-<job>@.service runs eru node run <job> --db %i) and a
// timer per cluster and job (eru-<job>@<cluster>.timer), staggered by the
// cluster's stanza (RFC 9.3): within the day, and the monthly drills over
// the month too (RFC 9.4).
func timerFiles(cfg config.Cluster) map[string][]byte {
	files := serverTimers(cfg)
	files[CertsTimerName] = render.CertsTimer()
	if !cfg.BackupConfigured() {
		return files
	}
	files["eru-stanza-expiry.service"] = render.JobService("stanza-expiry", "")
	files[StanzaExpiryTimerName] = render.JobTimer("eru expiry of dropped databases' stanzas", stanzaExpirySchedule, "30m")
	spread := StaggerSpread(cfg)
	for _, spec := range cfg.Clusters {
		offset := StaggerOffset(spec.Stanza, spread)
		for _, j := range scheduledJobs(cfg, spec) {
			schedule := Stagger(j.Schedule, offset)
			if j.Job == "restore-drill" {
				schedule = StaggerDay(schedule, spec.Stanza)
			}
			files["eru-"+j.Job+"@.service"] = render.JobService(j.Job, "--db %i")
			files["eru-"+j.Job+"@"+spec.Name+".timer"] = render.JobTimer(j.Description, schedule, j.Jitter)
		}
	}
	return files
}

// defaultStaggerSpread is the spread when [maintenance] window doesn't parse.
const defaultStaggerSpread = 3 * time.Hour

// StaggerSpread is how far apart the clusters' backups are spread: the length
// of [maintenance] window (02:00-05:00 is three hours).
func StaggerSpread(cfg config.Cluster) time.Duration {
	from, to, ok := strings.Cut(cfg.Maintenance.Window, "-")
	if !ok {
		return defaultStaggerSpread
	}
	start, err1 := time.Parse("15:04", strings.TrimSpace(from))
	end, err2 := time.Parse("15:04", strings.TrimSpace(to))
	if err1 != nil || err2 != nil || start.Equal(end) {
		return defaultStaggerSpread
	}
	if end.Before(start) {
		end = end.Add(24 * time.Hour)
	}
	return end.Sub(start)
}

// StaggerOffset is a cluster's fixed offset in whole minutes within spread,
// derived from its stanza id. The stanza never changes while the cluster
// exists, so neither does the offset.
func StaggerOffset(stanza string, spread time.Duration) time.Duration {
	minutes := uint32(spread / time.Minute) //nolint:gosec // G115: the spread is at most a day.
	if minutes == 0 || stanza == "" {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(stanza))
	return time.Duration(h.Sum32()%minutes) * time.Minute
}

// calendarTime is the time part of an OnCalendar expression with a fixed
// minute: HH:MM[:SS] or *:MM[:SS].
var calendarTime = regexp.MustCompile(`^(\*|[0-9]{1,2}):([0-9]{2})(:[0-9]{2})?$`)

// Stagger moves schedule's time of day later by offset. systemd has no
// offset for OnCalendar, so the time is rewritten, and never past the end of
// its day (HH:MM) or hour (*:MM), where the day or hour fields would have to
// move too: the offset wraps within what is left. A schedule without a
// fixed minute ("daily", "*:0/15") is returned as it is, and the heavy-I/O
// lock keeps its jobs apart.
func Stagger(schedule string, offset time.Duration) string {
	fields := strings.Fields(schedule)
	if len(fields) == 0 || offset <= 0 {
		return schedule
	}
	m := calendarTime.FindStringSubmatch(fields[len(fields)-1])
	if m == nil {
		return schedule
	}
	minute, _ := strconv.Atoi(m[2])
	shift := int(offset / time.Minute)
	if m[1] == "*" {
		if minute >= 60 {
			return schedule
		}
		minute += shift % (60 - minute)
		fields[len(fields)-1] = fmt.Sprintf("*:%02d%s", minute, m[3])
		return strings.Join(fields, " ")
	}
	hour, _ := strconv.Atoi(m[1])
	start := hour*60 + minute
	if hour >= 24 || minute >= 60 {
		return schedule
	}
	start += shift % (24*60 - start)
	fields[len(fields)-1] = fmt.Sprintf("%02d:%02d%s", start/60, start%60, m[3])
	return strings.Join(fields, " ")
}

// staggerDays is how many days of the month StaggerDay spreads over: every
// month has them.
const staggerDays = 28

// calendarDate is the date part of an OnCalendar expression with a fixed
// day of the month: [YYYY|*]-[MM|*]-DD.
var calendarDate = regexp.MustCompile(`^(\*|[0-9]{4})-(\*|[0-9]{1,2})-([0-9]{1,2})$`)

// StaggerDay moves a monthly schedule's day of the month later by a number
// of days derived from stanza, wrapping within the first 28 days, so each
// cluster's drill falls on a day of its own. A schedule without a fixed day
// up to the 28th is returned as it is.
func StaggerDay(schedule, stanza string) string {
	fields := strings.Fields(schedule)
	if stanza == "" {
		return schedule
	}
	for i, f := range fields {
		m := calendarDate.FindStringSubmatch(f)
		if m == nil {
			continue
		}
		day, _ := strconv.Atoi(m[3])
		if day < 1 || day > staggerDays {
			return schedule
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(stanza))
		day = (day-1+int(h.Sum32()%staggerDays))%staggerDays + 1
		fields[i] = fmt.Sprintf("%s-%s-%02d", m[1], m[2], day)
		return strings.Join(fields, " ")
	}
	return schedule
}

// staleUnits are eru timers and owned services on disk that are not rendered.
func staleUnits(want map[string][]byte) []string {
	var out []string
	timers, _ := filepath.Glob(filepath.Join(SystemdSystemDir, "eru-*.timer"))
	paths, _ := filepath.Glob(filepath.Join(SystemdSystemDir, "eru-*.path"))
	services, _ := filepath.Glob(filepath.Join(SystemdSystemDir, "eru-*.service"))
	for _, path := range append(append(timers, paths...), services...) {
		name := filepath.Base(path)
		if _, ok := want[name]; ok {
			continue
		}
		if strings.HasSuffix(name, ".timer") || strings.HasSuffix(name, ".path") || ownedService.MatchString(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func sortedNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type timersStep struct{}

func (timersStep) ID() string { return timersID }

func (timersStep) Describe(config.Cluster) string {
	return "install the systemd timers for certificates, maintenance, the master backup and each database's backups, verify, drill and dumps"
}

func (timersStep) RenderedFiles(cfg config.Cluster, _ engine.Runtime) []engine.RenderedFile {
	files := timerFiles(cfg)
	out := make([]engine.RenderedFile, 0, len(files))
	for _, name := range sortedNames(files) {
		out = append(out, engine.RenderedFile{Path: SystemdSystemDir + "/" + name, Content: files[name]})
	}
	return out
}

func (timersStep) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	files := timerFiles(cfg)
	units := make(map[string]string, len(files))
	for name, content := range files {
		units[name] = string(content)
	}
	return canonical(map[string]any{"units": units, "script": Script("timers")})
}

func (timersStep) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	files := timerFiles(cfg)
	for _, name := range sortedNames(files) {
		if !fileIs(SystemdSystemDir+"/"+name, string(files[name]), 0o644) {
			return incomplete(name + " differs"), nil
		}
		if enabledUnit(name) {
			if out, _ := Probe(ctx, "systemctl", "is-enabled", name); out != "enabled" {
				return incomplete(name + " is not enabled"), nil
			}
		}
	}
	if stale := staleUnits(files); len(stale) > 0 {
		return incomplete("stale units: " + strings.Join(stale, ", ")), nil
	}
	return complete(), nil
}

func (s timersStep) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	files := timerFiles(cfg)
	var enable []string
	for _, name := range sortedNames(files) {
		if err := writeFile(SystemdSystemDir+"/"+name, string(files[name]), 0o644); err != nil {
			return engine.ApplyResult{}, err
		}
		if enabledUnit(name) {
			enable = append(enable, name)
		}
	}
	stale := staleUnits(files)
	for _, name := range append(append([]string(nil), enable...), stale...) {
		if !unitNamePattern.MatchString(name) {
			return engine.ApplyResult{}, os.ErrInvalid
		}
	}
	err := rt.Shell.Run(ctx, s.ID(), Script("timers"), map[string]string{
		"SYSTEMD_DIR": SystemdSystemDir,
		"ENABLE":      strings.Join(enable, " "),
		"STALE":       strings.Join(stale, " "),
	}, report)
	notes := []string{"timers: " + strings.Join(enable, ", ")}
	if len(stale) > 0 {
		notes = append(notes, "removed: "+strings.Join(stale, ", "))
	}
	return engine.ApplyResult{Restart: engine.RestartNone, Notes: notes}, err
}
