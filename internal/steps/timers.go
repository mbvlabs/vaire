package steps

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
)

const timersID = "timers-v1"

// CertsTimerName is the hourly certificate timer timers-v1 owns.
const CertsTimerName = "eru-certs.timer"

// DumpServiceTemplate runs one database's long-retention dump; each
// eru-db-dump@<db>.timer starts its instance.
const DumpServiceTemplate = "eru-db-dump@.service"

// ownedService matches the service units timers-v1 writes, so a removed
// schedule takes its service with it. eru-certs.service and eru-thp.service
// belong to other steps.
var ownedService = regexp.MustCompile(`^eru-(backup-(full|diff|incr|verify)|restore-drill|db-dump@)\.service$`)

// unitNamePattern guards the unit names handed to the script.
var unitNamePattern = regexp.MustCompile(`^eru-[a-z0-9@_.-]+\.(timer|service)$`)

// timerJob is one scheduled job (§18).
type timerJob struct {
	Job         string
	Description string
	Schedule    string
	Jitter      string
}

// scheduledJobs are the backup jobs with a schedule in cfg.
func scheduledJobs(cfg config.Cluster) []timerJob {
	if !cfg.BackupConfigured() {
		return nil
	}
	db := cfg.Backup.Database
	all := []timerJob{
		{"backup-full", "eru full database backup", db.FullSchedule, "5m"},
		{"backup-diff", "eru differential database backup", db.DiffSchedule, "5m"},
		{"backup-incr", "eru incremental database backup", db.IncrSchedule, "2m"},
		{"backup-verify", "eru backup verify", db.VerifySchedule, "10m"},
		{"restore-drill", "eru restore drill", db.DrillSchedule, "30m"},
	}
	var out []timerJob
	for _, j := range all {
		if j.Schedule != "" {
			out = append(out, j)
		}
	}
	return out
}

// timerFiles returns every unit file timers-v1 owns, by file name.
func timerFiles(cfg config.Cluster) map[string][]byte {
	files := map[string][]byte{CertsTimerName: render.CertsTimer()}
	for _, j := range scheduledJobs(cfg) {
		files["eru-"+j.Job+".service"] = render.JobService(j.Job, "")
		files["eru-"+j.Job+".timer"] = render.JobTimer(j.Description, j.Schedule, j.Jitter)
	}
	if cfg.BackupConfigured() {
		for _, db := range cfg.Clusters {
			if db.Backup.DumpSchedule == "" {
				continue
			}
			files[DumpServiceTemplate] = render.JobService("db-dump", "--db %i")
			files["eru-db-dump@"+db.Name+".timer"] = render.JobTimer("eru long-retention dump of "+db.Name, db.Backup.DumpSchedule, "10m")
		}
	}
	return files
}

// staleUnits are eru timers and owned services on disk that are not rendered.
func staleUnits(want map[string][]byte) []string {
	var out []string
	timers, _ := filepath.Glob(filepath.Join(SystemdSystemDir, "eru-*.timer"))
	services, _ := filepath.Glob(filepath.Join(SystemdSystemDir, "eru-*.service"))
	for _, path := range append(timers, services...) {
		name := filepath.Base(path)
		if _, ok := want[name]; ok {
			continue
		}
		if strings.HasSuffix(name, ".timer") || ownedService.MatchString(name) {
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
	return "install the systemd timers for certificates, backups, verify, drills and dumps"
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
		if strings.HasSuffix(name, ".timer") {
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
		if strings.HasSuffix(name, ".timer") {
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
