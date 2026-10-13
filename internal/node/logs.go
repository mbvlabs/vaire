package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/steps"
)

type logsParams struct {
	Unit string `json:"unit"`
	// DB picks the cluster whose log the postgres unit reads; empty means
	// the only one.
	DB     string `json:"db,omitempty"`
	Since  string `json:"since"`
	Lines  int    `json:"lines"`
	Follow bool   `json:"follow"`
}

// logUnits are the units eru reads, mapped to their source. postgres is
// the log file of one database's cluster, postgresql@18-<db>.
var logUnits = map[string]string{
	"postgres":  "file:",
	"pgbouncer": "journal:pgbouncer.service",
	"nginx":     "journal:nginx.service",
	"eru-certs": "journal:eru-certs.service",
	"eru":       "runlog:",
}

func handleLogs(ctx context.Context, env *Env) (Result, error) {
	var p logsParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	unit := p.Unit
	if unit == "" {
		unit = "postgres"
	}
	source, ok := logUnits[unit]
	if !ok {
		return Result{}, output.NewError(output.CodeUsage, "unknown unit "+unit, output.ExitUsage, "Use one of details.units.").
			WithDetails(map[string]any{"units": sortedUnits()})
	}
	if unit == "postgres" {
		spec, err := onlyCluster(env.Cluster, p.DB)
		if err != nil {
			return Result{}, err
		}
		source = "file:" + steps.PGLogPath(spec.Name)
	}
	lines := p.Lines
	if lines <= 0 {
		lines = 100
	}
	if err := streamLog(ctx, source, p.Since, lines, p.Follow, env.Report); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"unit": unit, "source": source}}, nil
}

func sortedUnits() []string {
	out := make([]string, 0, len(logUnits))
	for name := range logUnits {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// streamLog runs the reader for source and emits one log event per line.
func streamLog(ctx context.Context, source, since string, lines int, follow bool, report engine.Reporter) error {
	kind, target, _ := cutSource(source)
	var name string
	var args []string
	switch kind {
	case "file":
		name = "tail"
		args = []string{"-n", strconv.Itoa(lines)}
		if follow {
			args = append(args, "-f")
		}
		args = append(args, target)
	case "journal":
		name = "journalctl"
		args = []string{"-u", target, "-o", "cat", "-n", strconv.Itoa(lines)}
		if since != "" {
			args = append(args, "--since", since)
		}
		if follow {
			args = append(args, "-f")
		}
	case "runlog":
		path, err := newestRunLog()
		if err != nil {
			return output.NewError(output.CodePrecondition, "there are no eru run logs yet", output.ExitPrecondition, "")
		}
		name = "tail"
		args = []string{"-n", strconv.Itoa(lines)}
		if follow {
			args = append(args, "-f")
		}
		args = append(args, path)
	default:
		return fmt.Errorf("unknown log source %q", source)
	}

	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed readers with a validated target.
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	scanLines(stdout, report)
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		return output.WrapError(output.CodeExternalCommand, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String())), output.ExitExternal, "")
	}
	return nil
}

func cutSource(source string) (kind, target string, ok bool) {
	for _, k := range []string{"file:", "journal:", "runlog:"} {
		if rest, found := strings.CutPrefix(source, k); found {
			return strings.TrimSuffix(k, ":"), rest, true
		}
	}
	return "", "", false
}

func scanLines(r io.Reader, report engine.Reporter) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		if report != nil {
			report(output.Event{Type: "log", Step: "logs", Line: scanner.Text()})
		}
	}
}

// newestRunLog returns the most recently modified run log.
func newestRunLog() (string, error) {
	entries, err := os.ReadDir(engine.LogDir)
	if err != nil {
		return "", err
	}
	type candidate struct {
		path string
		mod  int64
	}
	var files []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, candidate{path: filepath.Join(engine.LogDir, e.Name()), mod: info.ModTime().UnixNano()})
	}
	if len(files) == 0 {
		return "", os.ErrNotExist
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod > files[j].mod })
	return files[0].path, nil
}
