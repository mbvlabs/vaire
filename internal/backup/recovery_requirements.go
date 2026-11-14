package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// RecoveryRequirement is a known recovery floor and the configured value.
type RecoveryRequirement struct {
	Setting    string `json:"setting"`
	Required   int64  `json:"required"`
	Configured int64  `json:"configured"`
}

type recoveryFloor struct {
	label, setting   string
	minimum, maximum int64
}

var recoveryFloors = []recoveryFloor{
	{"max_connections setting", "max_connections", 1, 262143},
	{"max_worker_processes setting", "max_worker_processes", 0, 262143},
	{"max_wal_senders setting", "max_wal_senders", 0, 262143},
	{"max_prepared_xacts setting", "max_prepared_transactions", 0, 262143},
	{"max_locks_per_xact setting", "max_locks_per_transaction", 10, 2147483647},
}

var decimalRecoveryValue = regexp.MustCompile(`^[0-9]+$`)

// parseControlRequirements accepts only the exact PG18 C-locale labels.
// stderr must also be checked separately: pg_controldata can exit successfully
// after warning that the control-file CRC is invalid.
func parseControlRequirements(stdout, stderr string) (map[string]int64, error) {
	if strings.TrimSpace(stderr) != "" {
		return nil, errors.New("pg_controldata reported a diagnostic")
	}
	values := make(map[string]int64, len(recoveryFloors))
	for _, line := range strings.Split(stdout, "\n") {
		label, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		for _, floor := range recoveryFloors {
			if strings.TrimSpace(label) != floor.label {
				continue
			}
			if _, exists := values[floor.setting]; exists {
				return nil, errors.New("duplicate control requirement")
			}
			n, err := recoveryNumber(strings.TrimSpace(value), floor)
			if err != nil {
				return nil, errors.New("malformed control requirement")
			}
			values[floor.setting] = n
		}
	}
	if len(values) != len(recoveryFloors) {
		return nil, errors.New("missing control requirement")
	}
	return values, nil
}

func recoveryNumber(value string, floor recoveryFloor) (int64, error) {
	if !decimalRecoveryValue.MatchString(value) {
		return 0, errors.New("not a decimal integer")
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < floor.minimum || n > floor.maximum {
		return 0, errors.New("invalid recovery value")
	}
	return n, nil
}

// RecoveryRequirementsUnmet constructs the precondition error only for known
// insufficient settings. The caller must have inspected every setting using
// postgres -C against the actual temporary configuration.
func RecoveryRequirementsUnmet(target string, required, configured map[string]int64) error {
	var unmet []RecoveryRequirement
	for _, floor := range recoveryFloors {
		r, rok := required[floor.setting]
		c, cok := configured[floor.setting]
		if !rok || !cok || r < floor.minimum || c < floor.minimum || r > floor.maximum || c > floor.maximum {
			return recoveryInspectionFailure(target)
		}
		if c < r {
			unmet = append(unmet, RecoveryRequirement{floor.setting, r, c})
		}
	}
	if len(unmet) == 0 {
		return nil
	}
	return output.NewError(output.CodePrecondition, "the temporary cluster does not meet the restored recovery requirements", output.ExitPrecondition,
		"Request sufficient settings and use a new unused target name; later WAL may require higher values.").
		WithDetails(map[string]any{"reason": "recovery_requirements_unmet", "target": target, "settings": unmet})
}

// recoveryProbe runs fixed PG18 tools without forwarding their output into job
// logs. PostgreSQL diagnostics can contain arbitrary configuration text.
var recoveryProbe = runRecoveryProbe

type limitedProbeBuffer struct {
	bytes.Buffer
}

func (b *limitedProbeBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 128*1024 {
		return 0, errors.New("inspection output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func runRecoveryProbe(ctx context.Context, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := recoveryProbeCommand(ctx, args...)
	var stdout, stderr limitedProbeBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func recoveryProbeCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "runuser", append([]string{"-u", "postgres", "--"}, args...)...) //nolint:gosec // G204: fixed PG18 tools and managed paths.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C", "HOME=/var/lib/postgresql"}
	return cmd
}

var recoveryStartupSettings = []string{
	"lc_messages=C",
	"log_min_messages=info",
	"log_error_verbosity=default",
	"logging_collector=off",
	"log_destination=stderr",
	"log_line_prefix=%p:",
}

// The pg_ctl.conf parser splits options on whitespace. The final prefix
// override has no whitespace so pg_ctl passes one unambiguous argument.
const recoveryCtlOptions = "pg_ctl_options = '-o -clc_messages=C -o -clog_min_messages=info -o -clog_error_verbosity=default -o -clogging_collector=off -o -clog_destination=stderr -o -clog_line_prefix=%p:'\n"
const recoveryOriginalCtl = "pg_ctl.conf.vaire-original"

var emptyPgCtlOptions = regexp.MustCompile(`(?i)^pg_ctl_options\s*(?:=\s*|\s+)''\s*(?:#.*)?$`)
var autoFloorStart = regexp.MustCompile(`(?i)^\s*(max_connections|max_worker_processes|max_wal_senders|max_prepared_transactions|max_locks_per_transaction)(?:\s|=|$)`)
var autoFloorValue = regexp.MustCompile(`^(?:'[^']*'|[^\s#=]+)\s*(?:#.*)?$`)

// reconcileRecoveryFloors removes only restored ALTER SYSTEM floor
// assignments for an explicit destination. PostgreSQL then resolves the
// destination's ordinary 10/50/60/90 layer precedence itself during -C and
// startup. No guessed value or increase is written into the auto file.
func (m *Materialised) reconcileRecoveryFloors() error {
	if m.DestinationConfig == nil {
		return nil
	}
	path := filepath.Join(m.Data, "postgresql.auto.conf")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: restored data directory.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return recoveryInspectionFailure(m.Name)
	}
	var kept strings.Builder
	changed := false
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		text := strings.TrimRight(line, "\r\n")
		if indices := autoFloorStart.FindStringSubmatchIndex(text); indices != nil {
			value := strings.TrimSpace(text[indices[3]:])
			if strings.HasPrefix(value, "=") {
				value = strings.TrimSpace(value[1:])
			}
			if !autoFloorValue.MatchString(value) {
				return recoveryInspectionFailure(m.Name)
			}
			changed = true
			continue
		}
		kept.WriteString(line)
	}
	if changed {
		if err := writeClusterFile(path, []byte(kept.String()), 0o600, "postgres", "postgres"); err != nil {
			return recoveryInspectionFailure(m.Name)
		}
	}
	return nil
}

func (m *Materialised) prepareRecoveryDiagnostics() error {
	path := filepath.Join(m.ConfDir(), "pg_ctl.conf")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: temporary cluster registration.
	if err != nil {
		return recoveryInspectionFailure(m.Name)
	}
	if string(raw) != recoveryCtlOptions {
		seenEmpty := false
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if seenEmpty || !emptyPgCtlOptions.MatchString(line) {
				return recoveryInspectionFailure(m.Name)
			}
			seenEmpty = true
		}
		if err := writeClusterFile(filepath.Join(m.ConfDir(), recoveryOriginalCtl), raw, 0o644, "postgres", "postgres"); err != nil {
			return recoveryInspectionFailure(m.Name)
		}
	} else if _, err := os.Stat(filepath.Join(m.ConfDir(), recoveryOriginalCtl)); err != nil {
		return recoveryInspectionFailure(m.Name)
	}
	if err := writeClusterFile(path, []byte(recoveryCtlOptions), 0o644, "postgres", "postgres"); err != nil {
		return recoveryInspectionFailure(m.Name)
	}
	return nil
}

// restoreRecoveryDiagnostics makes the temporary startup overrides disappear
// before adoption. The backup permits an interrupted adoption to be retried.
func (m *Materialised) restoreRecoveryDiagnostics() error {
	original := filepath.Join(m.ConfDir(), recoveryOriginalCtl)
	raw, err := os.ReadFile(original) //nolint:gosec // G304: temporary cluster registration.
	if errors.Is(err, os.ErrNotExist) {
		current, readErr := os.ReadFile(filepath.Join(m.ConfDir(), "pg_ctl.conf")) //nolint:gosec // G304: temporary cluster registration.
		if readErr != nil {
			return readErr
		}
		if string(current) == recoveryCtlOptions {
			return recoveryInspectionFailure(m.Name)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := writeClusterFile(filepath.Join(m.ConfDir(), "pg_ctl.conf"), raw, 0o644, "postgres", "postgres"); err != nil {
		return err
	}
	return os.Remove(original)
}

// inspectRecoveryRequirements is called after registration and before start.
// pg_ctl.conf can supply additional postgres command-line options which -C
// would otherwise miss. Until those can be reproduced exactly, fail closed
// when that file contains active directives.
func (m *Materialised) inspectRecoveryRequirements(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(m.ConfDir(), "pg_ctl.conf")) //nolint:gosec // G304: temporary cluster registration.
	if err != nil {
		return recoveryInspectionFailure(m.Name)
	}
	if string(raw) != recoveryCtlOptions {
		return recoveryInspectionFailure(m.Name)
	}
	stdout, stderr, err := recoveryProbe(ctx, pgBin+"/pg_controldata", "-D", m.Data)
	if err != nil {
		return recoveryInspectionFailure(m.Name)
	}
	required, err := parseControlRequirements(stdout, stderr)
	if err != nil {
		return recoveryInspectionFailure(m.Name)
	}
	configured := make(map[string]int64, len(recoveryFloors))
	for _, floor := range recoveryFloors {
		args := []string{pgBin + "/postgres", "-D", m.Data}
		for _, setting := range recoveryStartupSettings {
			args = append(args, "-c", setting)
		}
		args = append(args, "-c", "config_file="+filepath.Join(m.ConfDir(), "postgresql.conf"), "-C", floor.setting)
		stdout, stderr, err = recoveryProbe(ctx, args...)
		if err != nil || strings.TrimSpace(stderr) != "" {
			return recoveryInspectionFailure(m.Name)
		}
		n, err := recoveryNumber(strings.TrimSpace(stdout), floor)
		if err != nil {
			return recoveryInspectionFailure(m.Name)
		}
		configured[floor.setting] = n
	}
	return RecoveryRequirementsUnmet(m.Name, required, configured)
}

func recoveryInspectionFailure(target string) error {
	return output.NewError(output.CodeExternalCommand, "could not inspect recovery requirements", output.ExitExternal,
		"Inspect the restored control file and temporary configuration.").
		WithDetails(map[string]any{"reason": "recovery_inspection_failed", "target": target})
}

// RetainRecoveryFailure identifies only the two failures for which the
// lifecycle owner must retain the exact partial target and its configuration.
func RetainRecoveryFailure(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*output.CLIError); ok { //nolint:errorlint // Examine each wrapper, including outer CLI errors, before unwrapping.
		if details, ok := e.Details.(map[string]any); ok &&
			(details["reason"] == "recovery_requirements_unmet" || details["reason"] == "recovery_parameter_mismatch") {
			return true
		}
	}
	if e, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range e.Unwrap() {
			if RetainRecoveryFailure(child) {
				return true
			}
		}
		return false
	}
	return RetainRecoveryFailure(errors.Unwrap(err))
}

var recoveryMismatchDetail = regexp.MustCompile(`^(max_connections|max_worker_processes|max_wal_senders|max_prepared_transactions|max_locks_per_transaction) = ([0-9]+) is a lower setting than on the primary server, where its value was ([0-9]+)\.$`)

// recoveryMismatch matches a message and its immediately associated DETAIL,
// regardless of PostgreSQL's configured line prefix. It emits no raw log data.
func recoveryMismatch(raw string) (RecoveryRequirement, bool) {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if !strings.HasSuffix(strings.TrimSpace(line), "WARNING:  hot standby is not possible because of insufficient parameter settings") &&
			!strings.HasSuffix(strings.TrimSpace(line), "FATAL:  recovery aborted because of insufficient parameter settings") {
			continue
		}
		if i+1 >= len(lines) || !strings.Contains(lines[i+1], "DETAIL:") {
			continue
		}
		marker := "WARNING:  "
		if strings.Contains(line, "FATAL:  ") {
			marker = "FATAL:  "
		}
		prefix, _, _ := strings.Cut(line, marker)
		detailPrefix, detail, _ := strings.Cut(lines[i+1], "DETAIL:")
		if prefix != detailPrefix {
			continue
		}
		match := recoveryMismatchDetail.FindStringSubmatch(strings.TrimSpace(detail))
		if match == nil {
			continue
		}
		configured, err1 := strconv.ParseInt(match[2], 10, 64)
		required, err2 := strconv.ParseInt(match[3], 10, 64)
		for _, floor := range recoveryFloors {
			if floor.setting == match[1] && err1 == nil && err2 == nil &&
				configured >= floor.minimum && required <= floor.maximum && configured < required {
				return RecoveryRequirement{Setting: match[1], Required: required, Configured: configured}, true
			}
		}
	}
	return RecoveryRequirement{}, false
}

func recoveryMismatchFailure(target string, mismatch RecoveryRequirement) error {
	return output.NewError(output.CodeExternalCommand, fmt.Sprintf("recovery of %s encountered insufficient parameter settings", target), output.ExitExternal,
		"Request sufficient settings and use a new unused target name; later WAL may require higher values.").
		WithDetails(map[string]any{"reason": "recovery_parameter_mismatch", "target": target, "stage": "recovery",
			"setting": mismatch.Setting, "required": mismatch.Required, "configured": mismatch.Configured})
}
