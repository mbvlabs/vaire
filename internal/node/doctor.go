package node

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/doctor"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/promfile"
	"github.com/mbvlabs/eru/internal/steps"
)

// DoctorStatePath keeps the last doctor run: its results, for eru-doctor.prom
// and the next run's memory.events comparison. It is regenerable and not part
// of the master backup.
const DoctorStatePath = "/var/lib/eru/doctor.json"

// doctorState is doctor.json.
type doctorState struct {
	At         string           `json:"at"`
	Results    []doctor.Result  `json:"results"`
	HighEvents map[string]int64 `json:"high_events,omitempty"`
}

func loadDoctorState() (doctorState, bool) {
	raw, err := os.ReadFile(DoctorStatePath)
	if err != nil {
		return doctorState{}, false
	}
	var st doctorState
	if json.Unmarshal(raw, &st) != nil {
		return doctorState{}, false
	}
	return st, true
}

// nearestExisting is path, or its nearest parent that exists: the data root
// is made with the first cluster and removed with the last, and until then
// the filesystem it will live on is its parent's.
func nearestExisting(path string, exists func(string) bool) string {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if exists(p) || p == filepath.Dir(p) {
			return p
		}
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// gatherFacts reads everything doctor's checks need from the node.
func gatherFacts(ctx context.Context, cfg config.Cluster, rt engine.Runtime, init bool) doctor.Facts {
	f := doctor.Facts{Now: time.Now().UTC(), Init: init, DataRoot: cfg.Server.DataRoot}
	if o, err := hostinfo.ReadOS(); err == nil {
		f.OSSupported, f.OSName = o.Supported(), osName(o)
	}
	_, err := os.Stat("/var/run/reboot-required")
	f.RebootRequired = err == nil
	f.DiskPath = nearestExisting(cfg.Server.DataRoot, pathExists)
	f.DiskTotal, f.DiskFree, f.DiskKnown = hostinfo.DiskUsage(f.DiskPath)
	f.UnattendedActive = steps.ServiceActive(ctx, "unattended-upgrades")
	if out, err := steps.Probe(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value"); err == nil {
		f.TimeKnown, f.TimeSynced = true, out == "yes"
	}
	if st, err := engine.LoadState(engine.StatePath); err == nil {
		f.LockedDownAt, f.LockdownPending, f.SecretsAtTime, f.Jobs = st.LockedDownAt, st.LockdownPending, st.SecretsConfirmedAt, st.Jobs
	}
	_, err = os.Stat(steps.BreakGlassPath)
	f.BreakGlassOpen = err == nil
	f.SSHConfigDrift = driftOf(ctx, cfg, rt, steps.SSHDStepID)
	f.FirewallDrift = driftOf(ctx, cfg, rt, steps.FirewallStepID)
	if out, err := steps.Probe(ctx, "ufw", "status"); err == nil {
		f.UFWActive = strings.HasPrefix(out, "Status: active")
	}
	f.OpenAllowlists = openAllowlists(cfg)

	f.TLSMode = cfg.TLS.Mode
	if cert := readCert(steps.TLSCertPath); cert != nil {
		f.CertKnown, f.CertNotAfter, f.CertSelfSign = true, cert.NotAfter, isSelfSignedCert(cert)
	}
	f.Hostname = steps.NodeHostname(cfg)
	if f.Hostname != "" {
		f.DNSChecked, f.DNSMatches, f.DNSDetail = checkDNS(ctx, cfg, f.Hostname)
	}

	f.MetricsEnabled = cfg.Metrics.Enabled
	if f.MetricsEnabled {
		f.Exporters = exporterHealth(ctx, len(stepsView(cfg).Clusters) > 0)
		f.EndpointStatus, f.EndpointError = endpointStatus(ctx, f.Hostname, cfg.Metrics.PathPrefix)
		f.PGProbes, f.PGExporterLog = postgresProbes(ctx, stepsView(cfg))
	}
	f.NginxActive = steps.ServiceActive(ctx, steps.NginxService)
	// Databases awaiting recovery have no cluster and no pooler entry yet
	// (Phase 5), so the poolers run only once one is recovered.
	f.HasDatabases = len(stepsView(cfg).Clusters) > 0
	if f.HasDatabases {
		f.PoolersActive = map[string]bool{
			steps.PgbouncerService:     steps.ServiceActive(ctx, steps.PgbouncerService),
			steps.SessionPoolerService: steps.ServiceActive(ctx, steps.SessionPoolerService),
		}
	}
	f.BackupConfigured = cfg.BackupConfigured()
	for _, w := range oversubscriptionWarnings(cfg, rt.Host) {
		f.BudgetWarnings = append(f.BudgetWarnings, doctor.Warning{Code: w.Code, Message: w.Message})
	}
	f.Records, _ = backup.LoadRecords()
	f.Master = masterFacts(cfg)

	prev, havePrev := loadDoctorState()
	created := clusterCreated()
	for _, s := range readClusters(ctx, cfg, "", false) {
		c := doctor.ClusterFacts{Name: s.Spec.Name, Stanza: s.Spec.Stanza, State: s.State, Error: s.Error, Sample: s.Sample}
		if f.Records.IsAwaiting(s.Spec.Name) {
			c.AwaitingRecovery = true
			f.Clusters = append(f.Clusters, c)
			continue
		}
		c.ConfigDrift = driftOf(ctx, cfg, rt, steps.ConfigStepID(s.Spec.Name))
		c.Overrides = overriddenSettings(filepath.Join(steps.PGConfD(s.Spec.Name), "90-overrides.conf"))
		if s.State == "running" {
			c.HBAWeak = weakHBA(ctx, s.Spec)
		}
		if s.CgroupOK {
			if p, ok := s.Cgroup.Pressure["memory"]; ok {
				c.PressureKnown, c.PressureAvg300 = true, p.Some.Avg300
			}
			c.HighEvents = s.Cgroup.MemoryEvents["high"]
			if havePrev {
				c.PrevHighEvents, c.PrevKnown = prev.HighEvents[s.Spec.Name]
			}
		}
		if s.State == "stopped" {
			if err := checkStarts(cfg, s.Spec, rt.Host, 0); err != nil {
				c.WouldNotStart = err.Error()
			}
		}
		c.CreatedAt = created[s.Spec.Name]
		if cfg.BackupConfigured() {
			c.FullInterval, c.FullIntervalErr = fullInterval(ctx, cfg, s.Spec)
		}
		f.Clusters = append(f.Clusters, c)
	}
	return f
}

// driftOf is a step's check detail, "" when the host matches it.
func driftOf(ctx context.Context, cfg config.Cluster, rt engine.Runtime, id string) string {
	detail, err := steps.CheckStep(ctx, cfg, rt, id)
	if err != nil {
		return "cannot check: " + err.Error()
	}
	return detail
}

// openAllowlists are the allowlist entries open to every address.
func openAllowlists(cfg config.Cluster) []string {
	var out []string
	for _, l := range []struct {
		name string
		list []string
		on   bool
	}{
		{"network.client_allowlist", cfg.Network.ClientAllowlist, cfg.Network.ExposePgbouncer || cfg.Network.ExposePostgres},
		{"metrics.allowlist", cfg.Metrics.Allowlist, true},
	} {
		if !l.on {
			continue
		}
		for _, cidr := range l.list {
			if open, err := openToAll(cidr); err == nil && open {
				out = append(out, l.name+" "+cidr)
			}
		}
	}
	return out
}

// openToAll reports whether cidr is open to every address (0.0.0.0/0, ::/0).
func openToAll(cidr string) (bool, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, err
	}
	ones, _ := n.Mask.Size()
	return ones == 0, nil
}

// checkDNS resolves hostname and compares it with the node's public address.
func checkDNS(ctx context.Context, cfg config.Cluster, hostname string) (checked, matches bool, detail string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, hostname)
	if err != nil {
		return false, false, err.Error()
	}
	want := ""
	if n, ok := cfg.Master(); ok {
		want = n.PublicAddress
	}
	for _, a := range addrs {
		if a == want {
			return true, true, ""
		}
	}
	return true, false, "resolves to " + strings.Join(addrs, ", ") + ", not " + want
}

// exporterHealth GETs every exporter on loopback.
func exporterHealth(ctx context.Context, databases bool) map[string]error {
	targets := map[string]string{
		"node_exporter":     "http://" + steps.NodeExporterAddr + "/metrics",
		"postgres_exporter": "http://" + steps.PostgresExporterAddr + "/metrics",
	}
	if databases {
		targets["pgbouncer_exporter"] = "http://" + steps.PgbouncerTxAddr + "/metrics"
		targets["pgbouncer_exporter-session"] = "http://" + steps.PgbouncerSessAddr + "/metrics"
	}
	out := map[string]error{}
	for name, url := range targets {
		code, err := steps.HTTPStatus(ctx, url)
		if err == nil && code != http.StatusOK {
			err = errHTTP(code)
		}
		out[name] = err
	}
	return out
}

type errHTTP int

func (e errHTTP) Error() string { return "HTTP " + http.StatusText(int(e)) }

// endpointStatus asks nginx for /metrics/node without a credential, through
// 443 on loopback as a scraper would (8443 wants the PROXY protocol header
// from the 443 hop). 401 is the healthy answer.
func endpointStatus(ctx context.Context, hostname, prefix string) (int, string) {
	if prefix == "" {
		prefix = "/metrics"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: hostname, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // G402: loopback probe of the node's own nginx; the certificate is checked by tls.expiry.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:443")
		},
	}}
	host := hostname
	if host == "" {
		host = "localhost"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+prefix+"/node", nil)
	if err != nil {
		return 0, err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	_ = resp.Body.Close()
	return resp.StatusCode, ""
}

// weakHBA lists pg_hba rules with trust, password or md5.
func weakHBA(ctx context.Context, spec config.ClusterSpec) []string {
	rows, err := steps.ClusterPG(spec).Query(ctx, "postgres",
		"SELECT DISTINCT auth_method FROM pg_hba_file_rules WHERE auth_method IN ('trust','password','md5')", nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rows {
		out = append(out, r["auth_method"])
	}
	return out
}

// overriddenSettings are the setting names a postgresql.conf-style file sets.
func overriddenSettings(path string) []string {
	f, err := os.Open(path) //nolint:gosec // G304: the cluster's own override file.
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	return parseSettingNames(f)
}

func parseSettingNames(r interface{ Read([]byte) (int, error) }) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			name = strings.Fields(line)[0]
		}
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// clusterCreated is when each cluster's postgres-cluster-v1 instance first
// completed, from the journal.
func clusterCreated() map[string]time.Time {
	out := map[string]time.Time{}
	j, err := engine.LoadJournal(engine.JournalPath)
	if err != nil {
		return out
	}
	for id, e := range j.Steps {
		tmpl, cluster := engine.SplitID(id)
		if tmpl != "postgres-cluster-v1" || cluster == "" {
			continue
		}
		at := e.StartedAt
		if at == "" {
			at = e.CompletedAt
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			out[cluster] = t
		}
	}
	return out
}

// fullInterval is the time between two full backups of spec, from its
// schedule through systemd-analyze calendar.
func fullInterval(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) (time.Duration, string) {
	schedule := spec.Backup.FullSchedule
	if schedule == "" {
		schedule = cfg.Backup.Database.FullSchedule
	}
	// TZ=UTC: every elapse prints in UTC, whatever the server's zone.
	out, err := steps.ProbeWithEnv(ctx, []string{"TZ=UTC"}, "systemd-analyze", "calendar", "--iterations=3", schedule)
	if err != nil {
		return 0, err.Error()
	}
	d, err := doctor.ScheduleInterval(doctor.ParseCalendarIterations(out))
	if err != nil {
		return 0, err.Error()
	}
	return d, ""
}

type doctorParams struct {
	Only string `json:"only,omitempty"`
	DB   string `json:"db,omitempty"`
}

// doctorData is doctor.run's result.
type doctorData struct {
	Results []doctor.Result       `json:"results"`
	Summary map[doctor.Status]int `json:"summary"`
}

// handleDoctorRun runs the node's checks (Phase 4). Failing checks are not an
// RPC error: the operator adds its own checks and decides the exit code.
func handleDoctorRun(ctx context.Context, env *Env) (Result, error) {
	var p doctorParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if !doctor.ValidOnly(p.Only) {
		return Result{}, output.NewError(output.CodeUsage, "unknown --only "+p.Only, output.ExitUsage,
			"Use one of "+strings.Join(doctor.Categories, ", ")+".")
	}
	if p.DB != "" {
		if _, err := clusterOf(env.Cluster, p.DB); err != nil {
			return Result{}, err
		}
	}
	results := runDoctor(ctx, env.Cluster, runtimeFor(env, true), false, p.Only, p.DB)
	return Result{Data: doctorData{Results: results, Summary: doctor.Summary(results)}}, nil
}

// runDoctor gathers the facts, runs the checks and, for a full run, records
// the results in doctor.json and eru-doctor.prom.
func runDoctor(ctx context.Context, cfg config.Cluster, rt engine.Runtime, init bool, only, db string) []doctor.Result {
	f := gatherFacts(ctx, cfg, rt, init)
	results := doctor.Run(doctor.Filter(doctor.NodeChecks(), only), f, db)
	if only == "" && db == "" && !init {
		recordDoctor(f, results)
	}
	return results
}

// recordDoctor writes doctor.json and eru-doctor.prom.
func recordDoctor(f doctor.Facts, results []doctor.Result) {
	st := doctorState{At: f.Now.Format(time.RFC3339), Results: results, HighEvents: map[string]int64{}}
	for _, c := range f.Clusters {
		st.HighEvents[c.Name] = c.HighEvents
	}
	if raw, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = engine.WriteFileAtomic(DoctorStatePath, append(raw, '\n'), 0o600)
	}
	_ = promfile.Write(promfile.Doctor, doctorFamilies(f.Now, results))
}

// doctorFamilies are eru-doctor.prom's families: one line per result, so an
// external Prometheus can alert on any failing check until Phase 9's alerts.
func doctorFamilies(at time.Time, results []doctor.Result) []*promfile.Family {
	check := promfile.Gauge("eru_doctor_check", "eru doctor results: 1 for the check's status.")
	sorted := append([]doctor.Result(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].DB < sorted[j].DB
	})
	for _, r := range sorted {
		check.Add(1, promfile.L("check", r.ID, "db", r.DB, "status", string(r.Status))...)
	}
	last := promfile.Gauge("eru_doctor_last_run_timestamp_seconds", "When eru doctor last ran on the node.")
	last.Add(float64(at.Unix()))
	failing := promfile.Gauge("eru_doctor_failing_checks", "How many doctor checks fail.")
	failing.Add(float64(len(doctor.Failed(results))))
	return []*promfile.Family{check, last, failing}
}

// doctorGate is step 18 (Phase 4 D10): after apply and server init's
// database phase the node runs its checks. During init a failing check
// stops init with doctor_failed (checks that can't pass yet are left out);
// after apply each failing check is the warning doctor_check_failed.
func doctorGate(ctx context.Context, cfg config.Cluster, rt engine.Runtime, init bool) (*doctorData, []output.Warning, error) {
	results := runDoctor(ctx, cfg, rt, init, "", "")
	data := &doctorData{Results: results, Summary: doctor.Summary(results)}
	failed := doctor.Failed(results)
	if len(failed) == 0 {
		return data, nil, nil
	}
	if init {
		names := make([]string, 0, len(failed))
		for _, r := range failed {
			names = append(names, resultName(r))
		}
		return data, nil, output.NewError(output.CodeDoctorFailed, "doctor checks fail after the database phase: "+strings.Join(names, ", "), output.ExitStep,
			"Run the fix of each failing check (eru doctor shows them), then eru server init --resume.").WithDetails(map[string]any{"checks": failed, "summary": data.Summary})
	}
	var warnings []output.Warning
	for _, r := range failed {
		msg := resultName(r) + ": " + r.Detail
		if r.Fix != "" {
			msg += " (fix: " + r.Fix + ")"
		}
		warnings = append(warnings, output.Warning{Code: "doctor_check_failed", Message: msg})
	}
	return data, warnings, nil
}

func resultName(r doctor.Result) string {
	if r.DB != "" {
		return r.ID + "@" + r.DB
	}
	return r.ID
}

// runDoctorJob is the hourly eru-doctor timer's run.
func runDoctorJob(ctx context.Context, cfg config.Cluster, report engine.Reporter) (any, error) {
	env := &Env{Cluster: cfg, Report: report}
	results := runDoctor(ctx, cfg, runtimeFor(env, true), false, "", "")
	summary := doctor.Summary(results)
	logLine(report, "doctor", formatSummary(summary))
	return map[string]any{"summary": summary}, nil
}

func formatSummary(s map[doctor.Status]int) string {
	return strings.Join([]string{
		itoa(s[doctor.Pass]) + " pass", itoa(s[doctor.Warn]) + " warn", itoa(s[doctor.Fail]) + " fail", itoa(s[doctor.Skip]) + " skip",
	}, ", ")
}

func itoa(n int) string { return strconv.Itoa(n) }
