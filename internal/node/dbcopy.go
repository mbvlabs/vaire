package node

// External PostgreSQL database copy (C03): the node-side preflight, the
// detachable db-copy job and its private workspace lifecycle. The secure
// source model, export, strict archive selection and strict loader live in
// internal/backup/external.go (C02); this file owns the destination lifecycle.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

// The source is carried as one secret: a PostgreSQL URL from stdin, or the
// individual connection fields with their password and optional CA. Neither
// appears in params, results, activity or ordinary JSON telemetry.
const (
	// CopySourceURLSecret is the raw postgres:// or postgresql:// URL.
	CopySourceURLSecret = "copy.source_url" //nolint:gosec // G101: a secret key name, not a value.
	// CopySourceSpecSecret is a JSON CopySourceSecret.
	CopySourceSpecSecret = "copy.source_spec" //nolint:gosec // G101: a secret key name, not a value.
	// CopySourceCASecret is a separately supplied PEM CA certificate, used
	// by a URL source (whose URL has no CA form) or alongside individual
	// fields. It is certificate content, never a path.
	CopySourceCASecret = "copy.source_ca" //nolint:gosec // G101: a secret key name, not a value.
)

// CopySourceSecret is the individual-field source representation carried in
// CopySourceSpecSecret. TLSMode is one of verify-full, verify-ca or require;
// empty is verify-full. CACertPEM is optional PEM content, never a path.
type CopySourceSecret struct {
	Host      string `json:"host"`
	Port      int    `json:"port,omitempty"`
	Database  string `json:"database"`
	User      string `json:"user"`
	Password  string `json:"password"`
	TLSMode   string `json:"tls_mode,omitempty"`
	CACertPEM string `json:"ca_cert_pem,omitempty"`
}

// Copy phases, in order. A failure reports the phase it reached.
const (
	copyPhasePreflight    = "preflight"
	copyPhaseExporting    = "exporting"
	copyPhaseProvisioning = "provisioning"
	copyPhaseImporting    = "importing"
	copyPhaseFinalizing   = "finalizing"
	copyPhaseSucceeded    = "succeeded"
)

type dbCopyParams struct {
	Name    string `json:"name"`
	Size    string `json:"size,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Profile string `json:"profile,omitempty"`
	// AllowOvercommit creates the destination even when its size pushes the
	// databases' sizes past the budget, with the memory_overcommitted warning.
	AllowOvercommit bool `json:"allow_overcommit,omitempty"`
}

// copyTargetState is the destination as it actually is, not as a failed job
// intended it. Known is false when a database catalog could not be read.
type copyTargetState struct {
	Name            string `json:"name"`
	InConfig        bool   `json:"in_config"`
	Cluster         bool   `json:"cluster"`
	Database        bool   `json:"database"`
	DataDirectory   bool   `json:"data_directory"`
	ConfigDirectory bool   `json:"config_directory"`
	Known           bool   `json:"known"`
}

// Present reports whether any destination artefact exists. A partial target
// is never deleted or replayed by vaire.
func (s copyTargetState) Present() bool {
	return s.InConfig || s.Cluster || s.Database || s.DataDirectory || s.ConfigDirectory
}

type copyProgress struct {
	phase         string
	database      string
	importRan     bool
	imported      bool
	backup        copyBackupState
	cleanupFailed bool
}

func (p *copyProgress) enter(report engine.Reporter, phase string) {
	if p == nil {
		return
	}
	p.phase = phase
	if report != nil {
		report(output.Event{Type: "progress", Phase: "copy", Detail: phase,
			Message: "copy " + p.database + ": " + phase})
	}
}

type copyBackupState struct {
	Configured bool            `json:"configured"`
	Ready      bool            `json:"ready"`
	Warning    *output.Warning `json:"warning,omitempty"`
}

// copyJobStartAmbiguousError means the systemd launch was attempted but its
// result could not be confirmed. It preserves the ID and never becomes a
// terminal record by itself.
type copyJobStartAmbiguousError struct {
	ID      string
	Cause   error
	SpecErr error
}

func (e *copyJobStartAmbiguousError) Error() string {
	return "the copy job's launch could not be confirmed"
}

func (e *copyJobStartAmbiguousError) Unwrap() error { return e.Cause }

// Copy-side seams. Production values are the node's real implementations;
// rootless tests replace them to model one boundary at a time.
var (
	copyOpenWorkspace    = backup.OpenCopyWorkspace
	copyWorkspaceOwner   = backup.PostgresWorkspaceOwner
	copyRootDir          = backup.CopyRoot
	copyPreflightSource  = backup.PreflightSource
	copyExportSource     = backup.ExportSource
	copySelectArchiveTOC = backup.SelectArchiveTOC
	copyRestoreStrict    = backup.RestoreStrict
	copyCheckCopySpace   = backup.CheckCopySpace
	copyCheckTools       = func() error {
		if err := backup.CheckPG18Tools(); err != nil {
			return err
		}
		return checkCopyLeaseTools()
	}
	copyExtensionPlan    = defaultCopyExtensionPlan
	copyAddDatabase      = addDatabaseFrom
	copyProvision        = defaultCopyProvision
	copyFirstBackup      = firstBackup
	copyClusterInventory = steps.AllClusterNames
	copyConfDir          = steps.PGConfDir
	copyLoadRecords      = backup.LoadRecords
	copyReadTargetState  = copyTargetStateOf
	copyRestorePolicy    = defaultCopyRestorePolicy
	copyRunGrants        = defaultCopyRunGrants
	copyVerifyObjects    = defaultCopyVerifyObjects
	copyAnalyze          = defaultCopyAnalyze
	copyTargetExtensions = defaultCopyTargetExtensions
	copyBeforeProvision  = func() {}
	copyBeforeSave       = func() {}
	copyState            = func() state.Store { return followerState() }
	copyLstat            = os.Lstat
	copyReadDir          = os.ReadDir
	copyStartJob         = startCopyJob
	copyJobDir           = JobDir
	copyLaunchJob        = launchJobUnit
	copyUpdateState      = engine.UpdateState
	copyRemoveJobSpec    = removeCopyJobSpec
	copyLeaseTool        = "/usr/bin/flock"
	copyStatTool         = "/usr/bin/stat"
	copyCatTool          = "/bin/cat"
	copyShellTool        = "/bin/sh"
)

// copyWorkspaceInactive reports whether a copy job's own record and lease
// prove the workspace can no longer be in use. It never treats a terminal
// record as sufficient evidence by itself. It is a seam so lifecycle tests
// can model positive inactivity without a live systemd.
var copyWorkspaceInactive = func(ctx context.Context, rec engine.JobRecord) bool {
	return copyJobLiveness(ctx, rec) == copyLivenessInactive
}

type copyLiveness int

const (
	copyLivenessUnknown copyLiveness = iota
	copyLivenessActive
	copyLivenessInactive
)

type copyUnitFactsValue struct {
	LoadState    string
	ActiveState  string
	SubState     string
	MainPID      int
	ControlPID   int
	InvocationID string
}

var (
	copyReadUnitFacts   = readCopyUnitFacts
	copyScanWorker      = scanCopyWorkerProcesses
	copyProcessState    = copyProcessExists
	copyJobProcessAlive = jobProcessAlive
)

func readCopyUnitFacts(ctx context.Context, unit string) (copyUnitFactsValue, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", //nolint:gosec // G204: a validated vaire unit name.
		"--property=LoadState,ActiveState,SubState,MainPID,ControlPID,InvocationID", unit).Output()
	if err != nil {
		return copyUnitFactsValue{}, err
	}
	facts := copyUnitFactsValue{}
	for line := range strings.SplitSeq(string(out), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "LoadState":
			facts.LoadState = value
		case "ActiveState":
			facts.ActiveState = value
		case "SubState":
			facts.SubState = value
		case "MainPID":
			facts.MainPID, err = strconv.Atoi(value)
			if err != nil {
				return copyUnitFactsValue{}, err
			}
		case "ControlPID":
			facts.ControlPID, err = strconv.Atoi(value)
			if err != nil {
				return copyUnitFactsValue{}, err
			}
		case "InvocationID":
			facts.InvocationID = value
		}
	}
	if facts.ActiveState == "" {
		return copyUnitFactsValue{}, errors.New("systemctl returned no ActiveState")
	}
	return facts, nil
}

func copyJobUnit(rec engine.JobRecord) (unit, invocation string) {
	if rec.Unit != "" {
		return rec.Unit, rec.InvocationID
	}
	if rec.InvocationID != "" {
		return timerUnit(rec), rec.InvocationID
	}
	return "", ""
}

// copyJobLiveness is the conservative copy-only liveness decision. It needs a
// positively inactive unit, no live or indeterminate worker/MainPID/ControlPID,
// and a successful /proc scan showing no worker for the exact job id. A
// terminal record is not evidence by itself.
func copyJobLiveness(ctx context.Context, rec engine.JobRecord) copyLiveness {
	if rec.State == engine.JobQueued {
		created, err := time.Parse(time.RFC3339, rec.CreatedAt)
		if err != nil || time.Since(created) < queuedGrace {
			return copyLivenessUnknown
		}
	}
	if rec.PID > 0 && copyJobProcessAlive(rec.PID, rec.Job) {
		return copyLivenessActive
	}
	unit, _ := copyJobUnit(rec)
	if unit == "" {
		return copyLivenessUnknown
	}
	facts, err := copyReadUnitFacts(ctx, unit)
	if err != nil {
		return copyLivenessUnknown
	}
	switch facts.ActiveState {
	case "active", "activating", "reloading", "deactivating":
		return copyLivenessActive
	case "inactive", "failed":
	default:
		return copyLivenessUnknown
	}
	if facts.LoadState != "loaded" && facts.LoadState != "not-found" {
		return copyLivenessUnknown
	}
	if facts.MainPID > 0 && copyJobProcessAlive(facts.MainPID, rec.Job) {
		return copyLivenessActive
	}
	if facts.ControlPID > 0 {
		alive, ok := copyProcessState(facts.ControlPID)
		if !ok {
			return copyLivenessUnknown
		}
		if alive {
			return copyLivenessActive
		}
	}
	present, ok := copyScanWorker(rec.ID)
	if !ok {
		return copyLivenessUnknown
	}
	if present {
		return copyLivenessActive
	}
	return copyLivenessInactive
}

func copyProcessExists(pid int) (alive bool, ok bool) {
	if pid <= 0 {
		return false, true
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, syscall.EPERM):
		return true, true
	case errors.Is(err, syscall.ESRCH):
		return false, true
	default:
		return false, false
	}
}

// scanCopyWorkerProcesses scans /proc for a live vaire worker for exactly id.
// It reports ok=false when the scan cannot be trusted.
func scanCopyWorkerProcesses(id string) (present bool, ok bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isDecimal(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return false, false
		}
		fields := strings.Split(string(raw), "\x00")
		for i := 0; i+2 < len(fields); i++ {
			if fields[i] != "node" || fields[i+1] != "run" || fields[i+2] != "db-copy" {
				continue
			}
			for j := i + 3; j+1 < len(fields); j++ {
				if fields[j] == "--job-id" && fields[j+1] == id {
					return true, true
				}
			}
		}
	}
	return false, true
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// copyLease is the operation's lifetime lease. The parent and every wrapped
// subprocess hold it shared; lifecycle pruning takes it exclusive.
type copyLease struct {
	file     *os.File
	path     string
	identity string
	token    string
}

func openCopyLease(ws *backup.CopyWorkspace) (*copyLease, error) {
	if ws == nil {
		return nil, copyFinalizeError("copy_workspace_lease_failed", "the copy workspace is missing", "Retry with a new job.")
	}
	f, err := ws.Create(backup.CopyWorkspaceLeaseName)
	if err != nil {
		return nil, err
	}
	token := rpc.NewRequestID()
	fail := func(cause error) (*copyLease, error) {
		_ = f.Close()
		removeErr := ws.RemoveFile(backup.CopyWorkspaceLeaseName)
		return nil, errors.Join(cause, removeErr)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		return fail(copyFinalizeError("copy_workspace_lease_failed", "the copy workspace lease could not be written", "Retry with a new job."))
	}
	if err := f.Sync(); err != nil {
		return fail(copyFinalizeError("copy_workspace_lease_failed", "the copy workspace lease could not be synced", "Retry with a new job."))
	}
	identity, err := backup.CopyWorkspaceLeaseIdentity(f)
	if err != nil {
		return fail(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return fail(copyFinalizeError("copy_workspace_lease_failed", "the copy workspace lease could not be locked", "Retry with a new job."))
	}
	return &copyLease{file: f, path: ws.FilePath(backup.CopyWorkspaceLeaseName), identity: identity, token: token}, nil
}

func (l *copyLease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

const copyLeaseOuterTemplate = `set -eu
lease=$1; want=$2; token=$3; inner=$4; shift 4
exec 9<"$lease"
got=$({{STAT}} -Lc '%d:%i' /proc/self/fd/9) || exit 125
[ "$got" = "$want" ] || exit 125
[ "$({{CAT}} "$lease")" = "$token" ] || exit 125
exec {{FLOCK}} -n -s /proc/self/fd/9 {{SHELL}} -c "$inner" vaire-copy-locked "$lease" "$want" "$token" "$@"
`

const copyLeaseInnerTemplate = `lease=$1; want=$2; token=$3; shift 3
got=$({{STAT}} -Lc '%d:%i' "$lease") || exit 125
[ "$got" = "$want" ] || exit 125
[ "$({{CAT}} "$lease")" = "$token" ] || exit 125
exec "$@"
`

func copyLeaseScripts() (outer, inner string) {
	replace := strings.NewReplacer(
		"{{STAT}}", copyStatTool,
		"{{CAT}}", copyCatTool,
		"{{FLOCK}}", copyLeaseTool,
		"{{SHELL}}", copyShellTool,
	)
	return replace.Replace(copyLeaseOuterTemplate), replace.Replace(copyLeaseInnerTemplate)
}

type copyLeaseExecutor struct {
	inner backup.CopyExecutor
	lease *copyLease
}

func (e copyLeaseExecutor) Exec(ctx context.Context, stepID string, cmd engine.Command, report engine.Reporter) error {
	if e.lease == nil {
		return e.inner.Exec(ctx, stepID, cmd, report)
	}
	resolved, err := resolveCopyCommand(cmd.Name)
	if err != nil {
		return fmt.Errorf("resolve %s for the copy lease: %w", cmd.Name, err)
	}
	outer, inner := copyLeaseScripts()
	wrapped := cmd
	wrapped.Name = copyShellTool
	wrapped.Args = []string{"-c", outer, "vaire-copy-lease", e.lease.path, e.lease.identity, e.lease.token, inner, resolved}
	wrapped.Args = append(wrapped.Args, cmd.Args...)
	return e.inner.Exec(ctx, stepID, wrapped, report)
}

var copyCommandDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

func resolveCopyCommand(name string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	for _, dir := range copyCommandDirs {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is not in the fixed copy command path", name)
}

func checkCopyLeaseTools() error {
	for _, path := range []string{copyLeaseTool, copyStatTool, copyCatTool, copyShellTool} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return output.NewError(output.CodeMissingTool,
				"a copy lifecycle helper is missing", output.ExitMissingTool,
				"Install util-linux and coreutils on the node, then retry.").
				WithDetails(map[string]any{"reason": "copy_lease_tool_missing", "path": path})
		}
	}
	return nil
}

// startCopyJob writes the private job spec and starts its transient unit.
// Before systemd-run is invoked every failure is definite and cleans up the
// spec. Once it is invoked, an error is ambiguous: the ID is preserved, the
// queued record is left alone, and the unread spec is removed best effort.
func startCopyJob(ctx context.Context, env *Env, job string, params any, jobSecrets map[string]string) (string, error) {
	id := env.Req.RequestID
	operationID := env.Req.OperationID
	if operationID == "" {
		operationID = id
	}
	spec := jobSpec{Job: job, Secrets: jobSecrets, OperationID: operationID, ParentRequestID: env.Req.RequestID}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return "", err
		}
		spec.Params = raw
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(copyJobDir, 0o700); err != nil {
		return "", err
	}
	specPath := filepath.Join(copyJobDir, id+".json")
	if err := engine.WriteFileAtomic(specPath, raw, 0o600); err != nil {
		return "", copyJobPreparationError(err, copyRemoveJobSpec(id))
	}
	unit := jobUnit(id)
	if err := copyUpdateState(func(st *engine.State) error {
		st.PutJob(engine.JobRecord{ID: id, Job: job, DB: rpcJobDatabase(job, spec.Params), Unit: unit, State: engine.JobQueued, CreatedAt: stamp()})
		return nil
	}); err != nil {
		return "", copyJobPreparationError(err, copyRemoveJobSpec(id))
	}
	if err := copyLaunchJob(ctx, env, unit, job, id); err != nil {
		removeErr := copyRemoveJobSpec(id)
		return id, &copyJobStartAmbiguousError{ID: id, Cause: err, SpecErr: removeErr}
	}
	env.Report(output.Event{Type: "progress", Phase: "job", Detail: id, Message: "job " + id + " started as " + unit})
	return id, nil
}

func copyJobPreparationError(cause, cleanup error) error {
	if cleanup == nil {
		return cause
	}
	return output.NewError(output.CodePrecondition,
		"the copy job could not be prepared and its private spec could not be removed", output.ExitPrecondition,
		"Inspect /var/lib/vaire/jobs as root; no job was started.").
		WithDetails(map[string]any{"reason": "copy_job_start_failed", "spec_cleanup_failed": true})
}

func removeCopyJobSpec(id string) error {
	if !validJobID(id) {
		return output.NewError(output.CodeUsage, "job id "+id+" is not a ULID", output.ExitUsage, "")
	}
	err := os.Remove(filepath.Join(copyJobDir, id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func defaultCopyProvision(ctx context.Context, env *Env, cfg config.Cluster, previous []byte, db config.ClusterSpec) (config.ClusterSpec, []output.Warning, error) {
	return provisionDatabase(ctx, env, cfg, previous, db, createOptions{deferInitialBackup: true})
}

func defaultCopyExtensionPlan(source []backup.SourceExtension) ([]backup.SourceExtension, []string, []backup.ExtensionMatch, error) {
	states := make([]steps.ExtensionState, len(source))
	for i, ext := range source {
		states[i] = steps.ExtensionState{Name: ext.Name, Version: ext.Version, Schema: ext.Schema}
	}
	plan, extensions, err := steps.CopyExtensionPlan(states)
	if err != nil {
		return nil, nil, nil, err
	}
	dest := make([]backup.SourceExtension, len(plan))
	for i, ext := range plan {
		dest[i] = backup.SourceExtension{Name: ext.Name, Version: ext.Version, Schema: ext.Schema}
	}
	matches, err := backup.MatchSourceExtensions(source, dest)
	if err != nil {
		return nil, nil, nil, err
	}
	return dest, extensions, matches, nil
}

// copySourceFromSecrets parses exactly one of the two source secret forms.
// Parsing registers the raw URL and decoded password for redaction; errors
// never echo either.
func copySourceFromSecrets(values map[string]string) (backup.Source, error) {
	rawURL, rawSpec := values[CopySourceURLSecret], values[CopySourceSpecSecret]
	rawCA := values[CopySourceCASecret]
	if rawCA != "" {
		secrets.Add(rawCA)
	}
	var src backup.Source
	var err error
	switch {
	case rawURL != "" && rawSpec != "":
		return backup.Source{}, output.NewError(output.CodeValidation,
			"pass either a source URL or the individual source fields, not both", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "invalid_source_input"})
	case rawURL != "":
		src, err = backup.ParseSourceURL(rawURL)
		if err != nil {
			return backup.Source{}, err
		}
	case rawSpec != "":
		secrets.Add(rawSpec)
		dec := json.NewDecoder(strings.NewReader(rawSpec))
		dec.DisallowUnknownFields()
		var spec CopySourceSecret
		if err := dec.Decode(&spec); err != nil {
			return backup.Source{}, output.NewError(output.CodeValidation,
				"the source connection details are not valid", output.ExitConfig,
				"Enter the source host, port, database, user and password as separate fields.").
				WithDetails(map[string]any{"reason": "invalid_source_input"})
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return backup.Source{}, output.NewError(output.CodeValidation,
				"the source connection details are not valid", output.ExitConfig,
				"Pass exactly one source connection object.").
				WithDetails(map[string]any{"reason": "invalid_source_input"})
		}
		secrets.Add(spec.Password)
		if spec.CACertPEM != "" && rawCA != "" {
			return backup.Source{}, output.NewError(output.CodeValidation,
				"the source CA certificate was supplied twice", output.ExitConfig,
				"Supply the CA certificate once.").
				WithDetails(map[string]any{"reason": "invalid_source_input"})
		}
		src, err = backup.NewSource(backup.SourceSpec{
			Host:     spec.Host,
			Port:     spec.Port,
			Database: spec.Database,
			User:     spec.User,
			Password: spec.Password,
			TLSMode:  spec.TLSMode,
			CACert:   []byte(spec.CACertPEM),
		})
		if err != nil {
			return backup.Source{}, err
		}
	default:
		return backup.Source{}, output.NewError(output.CodeInputRequired,
			"the source connection details were not supplied", output.ExitInput,
			"Pass a postgres:// URL on stdin (copy.source_url) or the individual fields (copy.source_spec).").
			WithDetails(map[string]any{"reason": "source_required", "secrets": []string{CopySourceURLSecret, CopySourceSpecSecret}})
	}
	if rawCA != "" {
		return src.WithCACert([]byte(rawCA))
	}
	return src, nil
}

// copyJobSecrets are the only request secrets a db-copy job spec carries:
// the role passwords, the service secrets steps read, one source form and the
// optional separately supplied source CA content.
func copyJobSecrets(values map[string]string, name string) (map[string]string, error) {
	out := stepSecrets(values)
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(name, role)
		password := values[key]
		if password == "" {
			return nil, output.NewError(output.CodeInputRequired,
				"the password for "+name+"_"+role+" was not supplied", output.ExitInput,
				"Generate it on the operator and pass it as "+key+".").
				WithDetails(map[string]any{"reason": "input_required", "secret": key})
		}
		out[key] = password
	}
	if values[CopySourceURLSecret] != "" && values[CopySourceSpecSecret] != "" {
		return nil, output.NewError(output.CodeValidation,
			"pass either a source URL or the individual source fields, not both", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "invalid_source_input"})
	}
	if values[CopySourceURLSecret] == "" && values[CopySourceSpecSecret] == "" {
		return nil, output.NewError(output.CodeInputRequired,
			"the source connection details were not supplied", output.ExitInput,
			"Pass a postgres:// URL on stdin (copy.source_url) or the individual fields (copy.source_spec).").
			WithDetails(map[string]any{"reason": "source_required", "secrets": []string{CopySourceURLSecret, CopySourceSpecSecret}})
	}
	out[CopySourceURLSecret] = values[CopySourceURLSecret]
	out[CopySourceSpecSecret] = values[CopySourceSpecSecret]
	if values[CopySourceCASecret] != "" {
		out[CopySourceCASecret] = values[CopySourceCASecret]
	}
	return out, nil
}

// checkCopyTargetAbsent enforces the copy namespace. allowProposed accepts
// exactly the one entry addDatabaseFrom is about to save. Every probe is
// non-following and fails closed: an existing file, a dangling symlink or an
// unreadable path means the name is not free.
func checkCopyTargetAbsent(ctx context.Context, cfg config.Cluster, name string, allowProposed bool) error {
	count := 0
	for _, cluster := range cfg.Clusters {
		if cluster.Name == name {
			count++
		}
	}
	if allowProposed {
		if count != 1 {
			return copyTargetError("target_config_changed",
				"the proposed database entry is not exactly the one vaire validated",
				"Do not retry into an existing database; choose a new name.")
		}
	} else if count > 0 {
		return existingDatabase(name)
	}

	inventory, err := copyClusterInventory(ctx)
	if err != nil {
		return copyTargetError("target_unreadable",
			"vaire could not read the PostgreSQL cluster inventory",
			"The target name cannot be proved free; check pg_lsclusters on the node and retry.")
	}
	if major, ok := inventory[name]; ok {
		return output.NewError(output.CodePrecondition,
			"a PostgreSQL "+major+" cluster named "+name+" already exists", output.ExitPrecondition,
			"Pick another name; vaire never imports into an existing cluster.").
			WithDetails(map[string]any{"reason": "target_cluster_exists", "database": name, "postgres_version": major})
	}

	paths := []string{copyConfDir(name)}
	if cfg.Server.DataRoot != "" {
		paths = append(paths, cfg.Server.DataDirectory(name))
	}
	for _, path := range paths {
		if _, err := copyLstat(path); err == nil {
			return copyTargetError("target_path_exists",
				"a data or configuration path for "+name+" already exists",
				"Pick another name, or inspect and remove the orphan path yourself; vaire never deletes it.").
				WithDetails(map[string]any{"reason": "target_path_exists", "database": name, "path": path})
		} else if !errors.Is(err, fs.ErrNotExist) {
			return copyTargetError("target_unreadable",
				"vaire could not inspect the target's data or configuration path",
				"The target name cannot be proved free; inspect the node's filesystem and retry.").
				WithDetails(map[string]any{"reason": "target_unreadable", "database": name, "path": path})
		}
	}

	recs, err := copyLoadRecords()
	if err != nil {
		return copyTargetError("target_unreadable",
			"vaire could not read its lifecycle records",
			"The target name cannot be proved free; check /var/lib/vaire and retry.")
	}
	_, restoring := recs.Restores[name]
	_, drilling := recs.Drilling[name]
	_, undoing := recs.Undoing[name]
	_, unfollowing := recs.Unfollowing[name]
	if restoring || drilling || undoing || unfollowing || recs.IsAwaiting(name) {
		return copyTargetError("target_reserved",
			name+" is owned by another unfinished vaire operation",
			"Finish that operation first; vaire does not copy into a reserved or partial target.")
	}
	return nil
}

func copyTargetError(reason, message, hint string) *output.CLIError {
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, hint).
		WithDetails(map[string]any{"reason": reason})
}

// copyPreviewTarget builds the exact proposed entry and runs the ordinary
// start and memory-budget checks without saving anything. It never reserves a
// port or a target name.
func copyPreviewTarget(env *Env, cfg config.Cluster, p dbCopyParams, extensions []string) (config.ClusterSpec, config.Resources, []output.Warning, error) {
	spec := config.ClusterSpec{
		Name:             p.Name,
		Size:             p.Size,
		Memory:           p.Memory,
		Profile:          p.Profile,
		StatementTimeout: "60s",
		Extensions:       append([]string(nil), extensions...),
	}
	preview := cfg
	preview.Clusters = append([]config.ClusterSpec(nil), cfg.Clusters...)
	if err := placeCluster(preview, &spec); err != nil {
		return config.ClusterSpec{}, config.Resources{}, nil, err
	}
	preview.Clusters = append(preview.Clusters, spec)
	if err := preview.Validate(); err != nil {
		return config.ClusterSpec{}, config.Resources{}, nil, config.ValidationError(err)
	}
	host := runtimeFor(env, false).Host
	if err := guardRecoveryFloorChanges(cfg, preview, host); err != nil {
		return config.ClusterSpec{}, config.Resources{}, nil, err
	}
	if err := addDatabaseCheckStarts(preview, spec, host, 0); err != nil {
		return config.ClusterSpec{}, config.Resources{}, nil, err
	}
	if err := checkBudget(preview, spec.Name, 0, host, p.AllowOvercommit, copyCommand(p)); err != nil {
		return config.ClusterSpec{}, config.Resources{}, nil, err
	}
	res, err := preview.Resources(spec)
	if err != nil {
		return config.ClusterSpec{}, config.Resources{}, nil, err
	}
	return spec, res, oversubscriptionWarnings(preview, host), nil
}

func copyFloorAdmission(env *Env, cfg config.Cluster, p dbCopyParams) error {
	spec := config.ClusterSpec{Name: p.Name, Size: p.Size, Memory: p.Memory, Profile: p.Profile}
	return guardRecoveryFloorAddition(cfg, spec, runtimeFor(env, true).Host)
}

// copyCommand is the db copy command at another size: what the
// memory_overcommitted refusal suggests.
func copyCommand(p dbCopyParams) func(size string) string {
	return func(size string) string {
		cmd := "vaire db copy " + p.Name + " --size " + size
		if p.Profile != "" {
			cmd += " --profile " + p.Profile
		}
		return cmd
	}
}

// handleDBCopyPreflight is the read-only db.copy.preflight RPC. It validates
// the target namespace/resources and the source, but creates no job, saves no
// configuration and does not occupy the target name.
func handleDBCopyPreflight(ctx context.Context, env *Env) (res Result, err error) {
	var p dbCopyParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := checkCopyName(p.Name); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	if err := copyFloorAdmission(env, cfg, p); err != nil {
		return Result{}, err
	}
	if err := checkCopyTargetAbsent(ctx, cfg, p.Name, false); err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	src, err := copySourceFromSecrets(env.Req.Secrets)
	if err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	if err := copyCheckTools(); err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	ws, err := copyOpenWorkspace(env.Req.RequestID)
	if err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	lease, err := openCopyLease(ws)
	if err != nil {
		remErr := copyRemoveOwnWorkspace(ws, nil)
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, errors.Join(err, remErr))
	}
	defer func() {
		if cerr := copyCleanupWorkspace(ws, lease); cerr != nil {
			if err == nil {
				err = cerr
			} else {
				env.Report(output.Event{Type: "warning", Code: "copy_cleanup_failed", Message: copyCleanupMessage(cerr)})
			}
		}
	}()
	if err := markCopyWorkspace(ws, copyOperationPreflight, lease); err != nil {
		remErr := copyRemoveOwnWorkspace(ws, lease)
		ws, lease = nil, nil
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, errors.Join(err, remErr))
	}

	exec := copyLeaseExecutor{inner: engine.Shell{Secrets: secrets.Default, Log: env.Log}, lease: lease}
	pre, err := copyPreflightSource(ctx, exec,
		"db.copy.preflight", copySafeReporter(env.Report), src, ws)
	if err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	_, extensions, matches, err := copyExtensionPlan(pre.Extensions)
	if err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	spec, resources, warnings, err := copyPreviewTarget(env, cfg, p, extensions)
	if err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	space, err := copyCheckCopySpace(ws, cfg.Server.DataDirectory(p.Name), pre.SizeBytes)
	if err != nil {
		return Result{}, copyJobFailure(ctx, cfg, &copyProgress{phase: copyPhasePreflight, database: p.Name}, err)
	}
	warnings = append(warnings, pre.Warnings...)
	return Result{Data: map[string]any{
		"source": copySourceInfo(src, pre),
		"target": map[string]any{
			"database":   spec,
			"resources":  resources,
			"extensions": matches,
			"planned":    extensions,
			"space":      space,
			"advisory":   true,
		},
		"dry_run":        false,
		"source_checked": true,
	}, Warnings: warnings}, nil
}

// handleDBCopy starts the detachable db-copy job. It validates cheap inputs
// and the destination preview, then lets the job own every mutation, lock and
// source effect. It does not add the database to cluster.toml first.
func handleDBCopy(ctx context.Context, env *Env) (Result, error) {
	var p dbCopyParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := checkCopyName(p.Name); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	if err := copyFloorAdmission(env, cfg, p); err != nil {
		return Result{}, err
	}
	if err := checkCopyTargetAbsent(ctx, cfg, p.Name, false); err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		spec, resources, warnings, err := copyPreviewTarget(env, cfg, p, []string{"pg_stat_statements"})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{
			"database":       spec,
			"resources":      resources,
			"dry_run":        true,
			"source_checked": false,
			"advisory":       true,
		}, Warnings: warnings}, nil
	}
	if _, err := copySourceFromSecrets(env.Req.Secrets); err != nil {
		return Result{}, err
	}
	jobSecrets, err := copyJobSecrets(env.Req.Secrets, p.Name)
	if err != nil {
		return Result{}, err
	}
	spec, resources, warnings, err := copyPreviewTarget(env, cfg, p, []string{"pg_stat_statements"})
	if err != nil {
		return Result{}, err
	}
	pruned := pruneCopyWorkspaces(ctx)
	id, err := copyStartJob(ctx, env, "db-copy", p, jobSecrets)
	if err != nil {
		var ambiguous *copyJobStartAmbiguousError
		if errors.As(err, &ambiguous) {
			return Result{}, attachJobWarnings(copyAmbiguousStartError(ctx, cfg, p.Name, ambiguous), append(warnings, pruned...))
		}
		return Result{}, attachJobWarnings(err, append(warnings, pruned...))
	}
	res, err := followJob(ctx, env, id)
	if err != nil {
		return res, attachJobWarnings(copyFollowFailure(ctx, cfg, p.Name, id, err), append(warnings, pruned...))
	}
	if m, ok := res.Data.(map[string]any); ok {
		if raw, ok := m["result"].(json.RawMessage); ok {
			var jobData struct {
				Database config.ClusterSpec `json:"database"`
			}
			if json.Unmarshal(raw, &jobData) == nil && jobData.Database.Name != "" {
				m["database"] = jobData.Database
			}
		}
		if _, exists := m["database"]; !exists {
			m["database"] = spec
		}
		if _, exists := m["resources"]; !exists {
			m["resources"] = resources
		}
	}
	res.Warnings = mergeJobWarnings(res.Warnings, append(warnings, pruned...))
	return res, nil
}

func copyAmbiguousStartError(ctx context.Context, cfg config.Cluster, name string, ambiguous *copyJobStartAmbiguousError) error {
	target := copyReadTargetState(ctx, cfg, name)
	details := map[string]any{
		"reason":                  JobOutcomeUnknown,
		"phase":                   "unknown",
		"start":                   "unknown",
		"import_outcome":          "unknown",
		"credentials_may_be_used": true,
		"database":                name,
		"target_present":          target.Present(),
		"target":                  target,
	}
	if ambiguous != nil {
		details["job_id"] = ambiguous.ID
		details["spec_removed"] = ambiguous.SpecErr == nil
		if ambiguous.SpecErr != nil {
			details["spec_cleanup_failed"] = true
			details["spec_cleanup_error"] = secrets.Redact(ambiguous.SpecErr.Error())
		}
	}
	return output.NewError(output.CodeError,
		"the copy job's start could not be confirmed", output.ExitGeneric,
		"Keep the generated credentials and the job id; check vaire job follow <id> before retrying or reusing the name.").
		WithDetails(details)
}

// copyFollowFailure classifies a follow failure after the copy job was
// successfully started. A failure to read or follow the started job's state
// is an unknown outcome, not a terminal copy failure: the job may still run
// any phase, so its ID and possible credentials must be preserved. A terminal
// job result (jobOutcome) passes through unchanged, including its truthful
// target/import details.
func copyFollowFailure(ctx context.Context, cfg config.Cluster, name, id string, err error) error {
	if !copyFollowObservation(err, id) {
		return err
	}
	target := copyReadTargetState(ctx, cfg, name)
	return output.NewError(output.CodeError,
		"the copy job was started but its progress could not be observed", output.ExitGeneric,
		"Keep the generated credentials and the job id; check vaire job follow "+id+" and vaire db list before retrying or reusing the name.").
		WithDetails(map[string]any{
			"reason":                  JobOutcomeUnknown,
			"phase":                   "unknown",
			"start":                   "confirmed",
			"observation_failed":      true,
			"import_outcome":          "unknown",
			"credentials_may_be_used": true,
			"job_id":                  id,
			"database":                name,
			"target_present":          target.Present(),
			"target":                  target,
		})
}

// copyFollowObservation reports whether err is a failure to observe a started
// job rather than its recorded terminal outcome. A missing record immediately
// after a confirmed launch is also an observation inconsistency, not proof
// that the job failed.
func copyFollowObservation(err error, id string) bool {
	var observed *jobObservationError
	if errors.As(err, &observed) {
		return true
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	jobID, _ := details["job_id"].(string)
	return reason == "job_not_found" && jobID == id
}

func checkCopyName(name string) error {
	if err := checkDatabaseName(name); err != nil {
		return err
	}
	if err := config.CheckDatabaseName(name); err != nil {
		return output.NewError(output.CodeValidation, "database name "+name+" "+err.Error(), output.ExitConfig, "Pass a shorter name.")
	}
	return nil
}

// jobCopy is the db-copy job: a fresh locked read, the read-only source
// preflight, an export before any destination exists, the authoritative
// namespace recheck, ordinary provisioning with its first backup deferred,
// the strict owner import, copy-specific access grants and verification,
// ANALYZE, then the backup the ordinary create would have taken. Every phase
// failure reports the exact phase and whether any destination artefact exists.
//
// The job deliberately holds the node-exclusive lock from before its fresh
// config read through the export and import: for this version, serializing
// ordinary desired-state changes is safer than inventing a lock-release and
// reacquire state machine around a detachable, potentially long export.
func jobCopy(ctx context.Context, env *jobEnv) (res any, err error) {
	var p dbCopyParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	prog := &copyProgress{phase: copyPhasePreflight, database: p.Name}
	if err := checkCopyName(p.Name); err != nil {
		return nil, copyJobFailure(ctx, config.Cluster{}, prog, err)
	}
	src, err := copySourceFromSecrets(env.Secrets)
	if err != nil {
		return nil, copyJobFailure(ctx, config.Cluster{}, prog, err)
	}
	cfg, _, err := copyState().Load()
	if err != nil {
		return nil, copyJobFailure(ctx, config.Cluster{}, prog, err)
	}
	env.Cluster = cfg
	if err := copyFloorAdmission(copyCreateEnv(env, cfg), cfg, p); err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	if err := checkCopyTargetAbsent(ctx, cfg, p.Name, false); err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	if err := copyCheckTools(); err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	ws, err := copyOpenWorkspace(env.ID)
	if err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	lease, err := openCopyLease(ws)
	if err != nil {
		remErr := copyRemoveOwnWorkspace(ws, nil)
		return nil, copyJobFailure(ctx, cfg, prog, errors.Join(err, remErr))
	}
	cleanupDone := false
	defer func() {
		if cleanupDone {
			return
		}
		if cerr := copyCleanupWorkspace(ws, lease); cerr != nil {
			env.Report(output.Event{Type: "warning", Code: "copy_cleanup_failed", Message: copyCleanupMessage(cerr)})
			if err != nil {
				err = addCopyCleanupDetail(err, cerr)
			}
		}
	}()
	if err := markCopyWorkspace(ws, copyOperationJob, lease); err != nil {
		remErr := copyRemoveOwnWorkspace(ws, lease)
		ws, lease = nil, nil
		cleanupDone = true
		return nil, copyJobFailure(ctx, cfg, prog, errors.Join(err, remErr))
	}

	prog.enter(env.Report, copyPhasePreflight)
	shell := engine.Shell{Secrets: secrets.Default}
	exec := copyLeaseExecutor{inner: shell, lease: lease}
	pre, err := copyPreflightSource(ctx, exec, env.ID+"-preflight", copySafeReporter(env.Report), src, ws)
	if err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	for _, warning := range pre.Warnings {
		env.Report(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	}
	_, extensions, matches, err := copyExtensionPlan(pre.Extensions)
	if err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	createEnv := copyCreateEnv(env, cfg)
	if _, _, _, err := copyPreviewTarget(createEnv, cfg, p, extensions); err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	if _, err := copyCheckCopySpace(ws, cfg.Server.DataDirectory(p.Name), pre.SizeBytes); err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}

	prog.enter(env.Report, copyPhaseExporting)
	archive, err := copyExportSource(ctx, exec, env.ID+"-export", copySafeReporter(env.Report), src, ws)
	if err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	selection, err := copySelectArchiveTOC(ctx, exec, env.ID+"-select", copySafeReporter(env.Report), ws, archive,
		backup.SelectionOptions{SourceDatabase: src.Database, Extensions: pre.Extensions})
	if err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}

	// The target may have been taken while the export ran, and it must be
	// rechecked again under the save lock immediately before insertion.
	copyBeforeProvision()
	if err := checkCopyTargetAbsent(ctx, cfg, p.Name, false); err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	if _, err := copyCheckCopySpace(ws, cfg.Server.DataDirectory(p.Name), pre.SizeBytes); err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}

	prog.enter(env.Report, copyPhaseProvisioning)
	build := func(config.Cluster) (config.ClusterSpec, error) {
		spec := config.ClusterSpec{
			Name:             p.Name,
			Size:             p.Size,
			Memory:           p.Memory,
			Profile:          p.Profile,
			StatementTimeout: "60s",
			Extensions:       append([]string(nil), extensions...),
		}
		return spec, nil
	}
	beforeSave := func(cfgWithTarget config.Cluster, _ config.ClusterSpec) error {
		copyBeforeSave()
		return checkCopyTargetAbsent(ctx, cfgWithTarget, p.Name, true)
	}
	saved, previous, created, err := copyAddDatabase(ctx, createEnv, build, p.AllowOvercommit, copyCommand(p), beforeSave)
	if err != nil {
		return nil, copyJobFailure(ctx, cfg, prog, err)
	}
	created, warnings, err := copyProvision(ctx, createEnv, saved, previous, created)
	if err != nil {
		fresh, _, loadErr := copyState().Load()
		if loadErr != nil {
			fresh = saved
		}
		return nil, copyJobFailure(ctx, fresh, prog, err)
	}
	env.Cluster = saved
	actual, err := copyTargetExtensions(ctx, steps.ClusterPG(created), p.Name)
	if err != nil {
		return nil, copyJobFailure(ctx, saved, prog, err)
	}
	if _, err := backup.MatchSourceExtensions(pre.Extensions, actual); err != nil {
		return nil, copyJobFailure(ctx, saved, prog, err)
	}

	prog.enter(env.Report, copyPhaseImporting)
	prog.importRan = true
	local := steps.ClusterPG(created)
	if err := copyRestoreStrict(ctx, exec, env.ID+"-restore", copySafeReporter(env.Report), ws, backup.StrictRestoreOptions{
		Local:     local,
		Target:    p.Name,
		Owner:     p.Name + "_owner",
		Selection: selection,
	}); err != nil {
		return nil, copyJobFailure(ctx, saved, prog, err)
	}
	prog.imported = true

	prog.enter(env.Report, copyPhaseFinalizing)
	if err := copyRestorePolicy(ctx, local, created); err != nil {
		return nil, copyJobFailure(ctx, saved, prog, copyFinalizeError("copy_policy_failed",
			"vaire could not restore the copied database's connect and admin policy",
			"Inspect the partial database; do not retry the import into the same database."))
	}
	if err := copyRunGrants(ctx, local, p.Name); err != nil {
		return nil, copyJobFailure(ctx, saved, prog, copyFinalizeError("copy_grants_failed",
			"vaire could not apply the copied objects' access policy",
			"Inspect the partial database; do not retry the import into the same database."))
	}
	if err := copyVerifyObjects(ctx, local, p.Name); err != nil {
		return nil, copyJobFailure(ctx, saved, prog, err)
	}
	if err := copyAnalyze(ctx, local, p.Name); err != nil {
		return nil, copyJobFailure(ctx, saved, prog, copyFinalizeError("copy_analyze_failed",
			"ANALYZE of the copied database failed",
			"Inspect the partial database; do not retry the import into the same database."))
	}

	backupState := copyBackupState{Configured: saved.BackupConfigured()}
	if backupState.Configured {
		rt := jobRuntime(env)
		if w := copyFirstBackup(ctx, saved, rt, env.Report, p.Name); w != nil {
			warning := *w
			backupState.Warning = &warning
			warnings = append(warnings, warning)
		} else {
			backupState.Ready = true
		}
	}
	prog.backup = backupState

	// Cleanup is part of success, not a warning: the archive and source
	// access files may not survive as an apparently successful copy.
	if cerr := copyCleanupWorkspace(ws, lease); cerr != nil {
		cleanupDone = true
		prog.cleanupFailed = true
		return nil, copyJobFailure(ctx, saved, prog, cerr)
	}
	cleanupDone = true
	ws, lease = nil, nil

	target := copyTargetState{Name: p.Name, InConfig: true, Cluster: true, Database: true,
		DataDirectory: true, ConfigDirectory: true, Known: true}
	prog.enter(env.Report, copyPhaseSucceeded)
	return map[string]any{
		"database":   created,
		"source":     copySourceInfo(src, pre),
		"extensions": matches,
		"target":     target,
		"phase":      copyPhaseSucceeded,
		"imported":   true,
		"backup":     backupState,
		"warnings":   warnings,
	}, nil
}

func copyCreateEnv(env *jobEnv, cfg config.Cluster) *Env {
	return &Env{Req: rpc.Request{Secrets: env.Secrets}, Cluster: cfg, Report: env.Report}
}

func copySourceInfo(src backup.Source, pre backup.SourcePreflight) map[string]any {
	return map[string]any{
		"endpoint":            src.RedactedEndpoint(),
		"url":                 src.RedactedURL(),
		"server_version":      pre.ServerVersion,
		"server_version_text": pre.ServerVersionText,
		"database":            pre.Database,
		"size_bytes":          pre.SizeBytes,
		"connect_ok":          pre.ConnectOK,
		"catalog_ok":          pre.CatalogOK,
		"tls_mode":            pre.TLSMode,
		"extensions":          pre.Extensions,
		"warnings":            pre.Warnings,
	}
}

// copySafeReporter forwards structured progress and warnings but never a raw
// subprocess log line. C02 already bounds its diagnostics; this second copy
// boundary keeps source SQL, row data and server continuations out of the
// node stream and job records.
func copySafeReporter(next engine.Reporter) engine.Reporter {
	if next == nil {
		return func(output.Event) {}
	}
	return func(ev output.Event) {
		if ev.Type == "log" {
			return
		}
		next(ev)
	}
}

// copyOperationKinds are the only operation-ownership markers a copy
// workspace may carry. PruneCopyWorkspace refuses anything else.
const (
	copyOperationJob       = "db-copy"
	copyOperationPreflight = "db-copy-preflight"
)

// markCopyWorkspace records which operation owns the workspace and which
// lease identity protects it before any source access files or archive are
// written.
func markCopyWorkspace(ws *backup.CopyWorkspace, kind string, lease *copyLease) error {
	if ws == nil || lease == nil {
		return copyFinalizeError("copy_workspace_failed", "the copy workspace is missing", "Retry with a new job.")
	}
	return ws.WriteFile(backup.CopyWorkspaceMarkerName,
		[]byte(kind+"\n"+filepath.Base(ws.Dir)+"\n"+lease.identity+"\n"+lease.token+"\n"))
}

// copyRemoveOwnWorkspace is the setup-failure cleanup of a workspace this
// process just created, before any lease-protected subprocess could use it.
// CopyWorkspace.Remove anchors removal to the verified parent entry.
func copyRemoveOwnWorkspace(ws *backup.CopyWorkspace, lease *copyLease) error {
	var errs []error
	if lease != nil {
		errs = append(errs, lease.Close())
	}
	if ws != nil {
		errs = append(errs, ws.Remove())
	}
	return errors.Join(errs...)
}

// copySkippedFields marks a skipped copy truthfully: no workspace, source
// connection or target mutation happens before the job's locks.
func copySkippedFields(job string, res map[string]any) {
	if job != "db-copy" {
		return
	}
	res["phase"] = copyPhasePreflight
	res["import_ran"] = false
}

func copyCleanupWorkspace(ws *backup.CopyWorkspace, lease *copyLease) error {
	if ws == nil {
		return nil
	}
	var closeErr error
	if lease != nil {
		closeErr = lease.Close()
	}
	rootDir := filepath.Dir(ws.Dir)
	jobID := filepath.Base(ws.Dir)
	closeErr = errors.Join(closeErr, ws.Close())
	owner, err := copyWorkspaceOwner()
	if err != nil {
		return errors.Join(closeErr, err)
	}
	_, pruneErr := backup.PruneCopyWorkspace(rootDir, jobID, owner)
	if err := errors.Join(closeErr, pruneErr); err != nil {
		return copyFinalizeError("copy_cleanup_failed",
			"the copy workspace cleanup did not complete",
			"Inspect /var/lib/vaire-copies as root; no database was changed.")
	}
	return nil
}

func copyCleanupMessage(err error) string {
	return "the copy workspace cleanup did not complete: " + secrets.Redact(err.Error())
}

// pruneCopyWorkspaces removes only copy-owned workspaces whose exact job
// record proves they are terminal or inactive. It never scans outside the
// copy root, never removes a database directory, and leaves a directory with
// no matching db-copy record or with an active job alone.
func pruneCopyWorkspaces(ctx context.Context) []output.Warning {
	st, err := loadJobState()
	if err != nil {
		// Without job records inactivity cannot be verified, so neither
		// workspaces nor private specs are removed.
		return []output.Warning{{Code: "copy_workspace_prune_failed",
			Message: "the job records could not be read, so no stale copy workspace or private spec was removed"}}
	}
	var warnings []output.Warning

	// Workspace pruning needs the root and its listing. Spec cleanup does not:
	// an absent, unsafe or unreadable root must not retain a verified-inactive
	// copy spec carrying source credentials.
	var entries []os.DirEntry
	var owner backup.WorkspaceOwner
	rootUsable := false
	info, rootErr := copyLstat(copyRootDir)
	switch {
	case errors.Is(rootErr, fs.ErrNotExist):
		// A missing root is normal; spec cleanup still runs.
	case rootErr != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir():
		warnings = append(warnings, output.Warning{Code: "copy_workspace_prune_failed",
			Message: "the copy workspace root is not a plain directory, so no stale workspace was removed"})
	default:
		entries, err = copyReadDir(copyRootDir)
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "copy_workspace_prune_failed",
				Message: "the copy workspace root could not be listed, so no stale workspace was removed"})
			break
		}
		owner, err = copyWorkspaceOwner()
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "copy_workspace_prune_failed",
				Message: "the postgres workspace owner could not be determined, so no stale workspace was removed"})
			break
		}
		rootUsable = true
	}
	if rootUsable {
		for _, entry := range entries {
			name := entry.Name()
			if !backup.CopyWorkspaceJobID(name) {
				continue
			}
			rec, ok := st.Job(name)
			if !ok {
				// A preflight workspace has no job record. Only a verified
				// preflight marker with a free lifetime lease may be removed;
				// age is not inactivity. Any other unrecorded directory is
				// unknown and is left alone.
				kind, err := backup.CopyWorkspaceOperation(copyRootDir, name, owner)
				if err != nil {
					warnings = append(warnings, output.Warning{Code: "copy_workspace_prune_blocked",
						Message: "unrecorded copy workspace " + name + " was left untouched: " + secrets.Redact(err.Error())})
					continue
				}
				if kind != copyOperationPreflight {
					warnings = append(warnings, output.Warning{Code: "copy_workspace_prune_blocked",
						Message: "unrecorded copy workspace " + name + " has no matching job record and was left untouched"})
					continue
				}
			} else {
				if rec.Job != "db-copy" {
					continue
				}
				if !copyWorkspaceInactive(ctx, rec) {
					if rec.Final() {
						warnings = append(warnings, output.Warning{Code: "copy_workspace_prune_blocked",
							Message: "copy workspace " + name + " was retained because its terminal job is not positively inactive"})
					}
					continue
				}
			}
			removed, err := backup.PruneCopyWorkspace(copyRootDir, name, owner)
			if err != nil {
				code := "copy_workspace_prune_failed"
				if errors.Is(err, backup.ErrCopyWorkspaceBusy) {
					code = "copy_workspace_prune_blocked"
				}
				warnings = append(warnings, output.Warning{Code: code,
					Message: "copy workspace " + name + " was left untouched: " + secrets.Redact(err.Error())})
				continue
			}
			_ = removed
		}
	}
	warnings = append(warnings, cleanupCopyJobSpecs(ctx, st)...)
	return warnings
}

// cleanupCopyJobSpecs removes a private spec only after the copy job has
// positively no live worker. An absent spec is already consumed or cleaned;
// an unreadable/unverifiable job is retained and reported.
func cleanupCopyJobSpecs(ctx context.Context, st engine.State) []output.Warning {
	var warnings []output.Warning
	for _, rec := range st.Jobs {
		if rec.Job != "db-copy" {
			continue
		}
		if !copyWorkspaceInactive(ctx, rec) {
			if rec.Final() {
				warnings = append(warnings, output.Warning{Code: "copy_job_spec_cleanup_failed",
					Message: "the private job spec for " + rec.ID + " was retained because its terminal job is not positively inactive"})
			}
			continue
		}
		if err := copyRemoveJobSpec(rec.ID); err != nil {
			warnings = append(warnings, output.Warning{Code: "copy_job_spec_cleanup_failed",
				Message: "the private job spec for " + rec.ID + " was left in place: " + secrets.Redact(err.Error())})
		}
	}
	return warnings
}

// copyTargetStateOf reads the destination's actual state without mutation.
func copyTargetStateOf(ctx context.Context, cfg config.Cluster, name string) copyTargetState {
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	st := copyTargetState{Name: name}
	pathsKnown := true
	for _, cluster := range cfg.Clusters {
		if cluster.Name == name {
			st.InConfig = true
			break
		}
	}
	if cfg.Server.DataRoot != "" {
		if _, err := copyLstat(cfg.Server.DataDirectory(name)); err == nil {
			st.DataDirectory = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			pathsKnown = false
		}
	}
	if _, err := copyLstat(copyConfDir(name)); err == nil {
		st.ConfigDirectory = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		pathsKnown = false
	}
	inventory, invErr := copyClusterInventory(qctx)
	if invErr != nil {
		// The cluster inventory is unreadable, so its absence cannot be
		// claimed; the paths and config entry still say what is present.
		return st
	}
	st.Cluster = inventory[name] != ""
	if spec, ok := cfg.ClusterByName(name); ok {
		if st.Cluster {
			exists, err := databaseIn(qctx, steps.ClusterPG(spec), name)
			if err == nil {
				st.Database = exists
				st.Known = pathsKnown
			}
		} else {
			st.Known = pathsKnown
		}
		return st
	}
	st.Known = !st.Cluster && pathsKnown
	return st
}

func defaultCopyTargetExtensions(ctx context.Context, local pg.Local, database string) ([]backup.SourceExtension, error) {
	rows, err := local.Query(ctx, database,
		"SELECT e.extname, e.extversion, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY e.extname", nil)
	if err != nil {
		return nil, copyFinalizeError("copy_extension_verify_failed",
			"vaire could not read the new database's installed extensions",
			"Inspect the partial database; do not retry the import into the same database.")
	}
	out := make([]backup.SourceExtension, 0, len(rows))
	for _, row := range rows {
		out = append(out, backup.SourceExtension{Name: row["extname"], Version: row["extversion"], Schema: row["nspname"]})
	}
	return out, nil
}

func defaultCopyRestorePolicy(ctx context.Context, local pg.Local, spec config.ClusterSpec) error {
	if err := local.Exec(ctx, "postgres", steps.DatabaseConnectSQL(spec.Name, spec.Name), nil); err != nil {
		return err
	}
	return local.Exec(ctx, "postgres", steps.AdminAccessSQL(spec), nil)
}

func defaultCopyRunGrants(ctx context.Context, local pg.Local, database string) error {
	return local.Exec(ctx, database, steps.CopyObjectGrantsSQL(database), nil)
}

func defaultCopyVerifyObjects(ctx context.Context, local pg.Local, database string) error {
	rows, err := local.Query(ctx, database, steps.CopyObjectVerifySQL(database), nil)
	if err != nil {
		return copyFinalizeError("copy_verify_failed",
			"vaire could not verify the copied objects' owners and access",
			"Inspect the partial database; do not retry the import into the same database.")
	}
	if len(rows) != 1 {
		return copyFinalizeError("copy_verify_failed",
			"vaire's copied-object verification returned no result",
			"Inspect the partial database; do not retry the import into the same database.")
	}
	counts := map[string]int{}
	for key, value := range rows[0] {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return copyFinalizeError("copy_verify_failed",
				"vaire's copied-object verification returned an invalid count",
				"Inspect the partial database; do not retry the import into the same database.")
		}
		counts[key] = n
	}
	total := 0
	for _, key := range []string{"database_owner_mismatch", "database_connect_missing", "admin_policy_missing",
		"migrate_owner_missing", "object_owner_mismatch", "schema_usage_missing", "table_privilege_missing",
		"foreign_table_privilege_missing", "sequence_privilege_missing", "function_privilege_missing"} {
		total += counts[key]
	}
	if total > 0 {
		return output.NewError(output.CodePrecondition,
			"the copied database does not have vaire's owner and access policy", output.ExitPrecondition,
			"Inspect the partial database; do not retry the import into the same database.").
			WithDetails(map[string]any{"reason": "copy_verify_failed", "counts": counts})
	}
	return nil
}

func defaultCopyAnalyze(ctx context.Context, local pg.Local, database string) error {
	return local.Exec(ctx, database, steps.CopyAnalyzeSQL(database), nil)
}

func copyFinalizeError(reason, message, hint string) error {
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, hint).
		WithDetails(map[string]any{"reason": reason})
}

// copyJobFailure makes a job failure truthful and sanitized: it records the
// phase, whether an import could have run and the destination's actual
// artefacts, and strips unclassified subprocess diagnostics and source SQL.
func copyJobFailure(ctx context.Context, cfg config.Cluster, prog *copyProgress, cause error) error {
	safe := copySanitizeError(cause)
	details := map[string]any{}
	code, message, hint := output.CodeError, "the database copy failed", ""
	var cliErr *output.CLIError
	if errors.As(safe, &cliErr) {
		code, message, hint = cliErr.Code, cliErr.Message, cliErr.Hint
		if message == "" {
			message = "the database copy failed"
		}
		for key, value := range copySafeDetails(cliErr.Details) {
			details[key] = value
		}
	}
	if copyHasSourceCleanupFailure(cause) {
		details["cleanup_failed"] = true
		details["workspace_cleanup"] = "failed"
	}
	phase, database, importRan := copyPhasePreflight, "", false
	imported, cleanupFailed := false, false
	var backup copyBackupState
	if prog != nil {
		phase, database, importRan = prog.phase, prog.database, prog.importRan
		imported, cleanupFailed, backup = prog.imported, prog.cleanupFailed, prog.backup
	}
	if details["reason"] == "has_followers" {
		details["source_database"] = details["database"]
	}
	if details["reason"] == "follower_recovery_floor_decrease" {
		details["follower_database"] = details["database"]
	}
	target := copyTargetState{Name: database}
	if database != "" {
		target = copyReadTargetState(ctx, cfg, database)
	}
	details["phase"] = phase
	details["import_ran"] = importRan
	details["imported"] = imported
	details["target_present"] = target.Present()
	details["target"] = target
	details["database"] = database
	details["backup"] = backup
	if cleanupFailed {
		details["cleanup_failed"] = true
		details["workspace_cleanup"] = "failed"
		details["blocked"] = true
	}
	if _, ok := details["reason"]; !ok || details["reason"] == "" {
		details["reason"] = copyPhaseReason(phase)
	}
	hint = copyFailureHint(hint, target)
	return output.NewError(code, message, output.ExitFor(code), hint).WithDetails(details)
}

// copySanitizeError keeps only fixed messages and classified detail fields.
// Raw pg_dump/pg_restore stderr and source SQL are never stored in a job
// record or returned in a structured result.
func copySanitizeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, engine.ErrInterrupted) {
		return output.NewError(output.CodeError, "the database copy was interrupted", output.ExitGeneric,
			"The source is unchanged; the destination may be partial. Do not replay the import.").
			WithDetails(map[string]any{"reason": "cancelled"})
	}
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) {
		details := map[string]any{}
		for key, value := range copySafeDetails(cliErr.Details) {
			details[key] = value
		}
		message := cliErr.Message
		if message == "" {
			// WrapError leaves Message empty and keeps raw text in Cause.
			// A job outcome never copies that cause.
			message = "the database copy failed"
		}
		return output.NewError(cliErr.Code, secrets.Redact(message), output.ExitFor(cliErr.Code), secrets.Redact(cliErr.Hint)).
			WithDetails(details)
	}
	return output.NewError(output.CodeError, "the database copy failed", output.ExitGeneric,
		"Inspect the node's run log; do not retry the import into an existing destination.").
		WithDetails(map[string]any{"reason": "copy_failed"})
}

func copySafeDetails(details any) map[string]any {
	out := map[string]any{}
	values := map[string]any{}
	switch m := details.(type) {
	case map[string]any:
		values = m
	case map[string]string:
		for key, value := range m {
			values[key] = value
		}
	default:
		return out
	}
	for _, key := range []string{"reason", "secret", "secret_key", "secrets", "database", "cluster", "exit_status",
		"largest_size", "suggested_command", "version", "source_version", "space", "counts",
		"path", "postgres_version"} {
		value, ok := values[key]
		if !ok {
			continue
		}
		if key == "reason" {
			if reason, ok := value.(string); !ok || !copyReasonPattern.MatchString(reason) {
				continue
			}
		}
		out[key] = value
	}
	// Retain only validated local names and allowlisted numeric floor facts.
	if values["reason"] == "has_followers" || values["reason"] == "follower_recovery_floor_decrease" {
		if source, ok := values["source"].(string); ok && config.DatabasePattern.MatchString(source) {
			out["source"] = source
		}
		if names, ok := values["followers"].([]string); ok {
			safe := make([]string, 0, len(names))
			for _, name := range names {
				if config.DatabasePattern.MatchString(name) {
					safe = append(safe, name)
				}
			}
			out["followers"] = safe
		}
		if drops, ok := values["settings"].([]recoveryFloorDecrease); ok {
			safe := make([]recoveryFloorDecrease, 0, len(drops))
			for _, drop := range drops {
				if slices.Contains(steps.FloorSettings, drop.Setting) {
					safe = append(safe, drop)
				}
			}
			out["settings"] = safe
		}
	}
	if values["reason"] == "copy_workspace_access_denied" {
		if stage, ok := values["stage"].(string); ok && (stage == "ancestor" || stage == "source-access") {
			out["stage"] = stage
		}
		if file, ok := values["file"].(string); ok && (file == "directory" || file == "copy-root" || file == "source.pgpass" || file == "source-ca.pem" || file == "system-ca-bundle") {
			out["file"] = file
		}
	}
	return out
}

func copyHasSourceCleanupFailure(err error) bool {
	if err == nil {
		return false
	}
	// This walk intentionally inspects each direct node of a joined error.
	if cliErr, ok := err.(*output.CLIError); ok { //nolint:errorlint // errors.As would skip later joined cleanup errors
		details, _ := cliErr.Details.(map[string]any)
		return details["reason"] == "secret_cleanup_failed" || details["reason"] == "workspace_cleanup_failed"
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if copyHasSourceCleanupFailure(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return copyHasSourceCleanupFailure(wrapped.Unwrap())
	}
	return false
}

var copyReasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func copyPhaseReason(phase string) string {
	switch phase {
	case copyPhasePreflight:
		return "copy_preflight_failed"
	case copyPhaseExporting:
		return "copy_export_failed"
	case copyPhaseProvisioning:
		return "copy_provision_failed"
	case copyPhaseImporting:
		return "copy_import_failed"
	case copyPhaseFinalizing:
		return "copy_finalize_failed"
	default:
		return "copy_failed"
	}
}

func copyFailureHint(hint string, target copyTargetState) string {
	dest := "No destination database was created."
	if target.Present() {
		dest = "The destination may be partial; vaire never deletes or replays it automatically. Inspect it, then choose a new name or drop it yourself with vaire db drop " + target.Name +
			". vaire apply can finish provisioning, but never the failed data import."
	}
	if hint == "" {
		return dest
	}
	return hint + " " + dest
}

func addCopyCleanupDetail(err error, cleanup error) error {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	copied := *cliErr
	details := map[string]any{}
	if m, ok := cliErr.Details.(map[string]any); ok {
		for key, value := range m {
			details[key] = value
		}
	}
	details["cleanup_failed"] = true
	details["cleanup_error"] = secrets.Redact(cleanup.Error())
	copied.Details = details
	return &copied
}
