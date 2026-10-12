package pg

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/secrets"
)

// Local runs SQL on the node through the postgres OS user, because pg_hba
// allows postgres only by peer and the node process runs as root (§9.1). The
// zero value is the live cluster on its default socket; Host and Port point
// it at another instance, such as a scratch restore on the drill socket.
type Local struct {
	// Host is a socket directory; empty uses psql's default.
	Host string
	// Port is the instance's port; 0 uses psql's default.
	Port int
}

// localTimeout bounds one psql invocation.
const localTimeout = 5 * time.Minute

// varNamePattern is the psql -v variable name format.
var varNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Exec runs sql against db and discards the output. vars become psql -v
// parameters; they must be non-secret literals with identifier names.
func (l Local) Exec(ctx context.Context, db, sql string, vars map[string]string) error {
	_, err := l.run(ctx, db, sql, vars, false)
	return err
}

// Query runs sql against db and returns rows as column-name -> value maps. It
// uses psql's CSV output.
func (l Local) Query(ctx context.Context, db, sql string, vars map[string]string) ([]map[string]string, error) {
	out, err := l.run(ctx, db, sql, vars, true)
	if err != nil {
		return nil, err
	}
	return parseCSV(out)
}

// QueryRows runs sql against db and returns the column order and rows, with
// nil for SQL NULL.
func (l Local) QueryRows(ctx context.Context, db, sql string, vars map[string]string) ([]string, [][]*string, error) {
	out, err := l.run(ctx, db, sql, vars, true)
	if err != nil {
		return nil, nil, err
	}
	return parseCSVRows(out)
}

// run feeds sql to psql on stdin as the postgres OS user.
func (l Local) run(ctx context.Context, db, sql string, vars map[string]string, csvOut bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, localTimeout)
	defer cancel()
	args := []string{"-u", "postgres", "--", "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1"}
	if l.Host != "" {
		args = append(args, "-h", l.Host)
	}
	if l.Port != 0 {
		args = append(args, "-p", strconv.Itoa(l.Port))
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		if !varNamePattern.MatchString(name) {
			return nil, fmt.Errorf("psql variable name %q is invalid", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.ContainsAny(vars[name], "\n\r") {
			return nil, fmt.Errorf("psql variable %s contains a newline", name)
		}
		args = append(args, "-v", name+"="+vars[name])
	}
	if csvOut {
		args = append(args, "--csv")
	}
	args = append(args, "-d", db, "-f", "-")
	cmd := exec.CommandContext(ctx, "runuser", args...) //nolint:gosec // G204: fixed runuser/psql invocation; db is validated by the caller.
	cmd.Stdin = strings.NewReader(sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := secrets.Redact(strings.TrimSpace(stderr.String()))
		if msg == "" {
			return nil, fmt.Errorf("psql: %w", err)
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

// parseCSV turns psql --csv output into column-name -> value maps.
func parseCSV(raw []byte) ([]map[string]string, error) {
	header, records, err := readCSV(raw)
	if err != nil {
		return nil, err
	}
	if header == nil {
		return nil, nil
	}
	out := make([]map[string]string, 0, len(records))
	for _, record := range records {
		row := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(record) {
				row[name] = record[i]
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// parseCSVRows turns psql --csv output into an ordered column list and rows.
func parseCSVRows(raw []byte) ([]string, [][]*string, error) {
	header, records, err := readCSV(raw)
	if err != nil {
		return nil, nil, err
	}
	if header == nil {
		return nil, nil, nil
	}
	rows := make([][]*string, 0, len(records))
	for _, record := range records {
		row := make([]*string, len(header))
		for i := range header {
			if i < len(record) && record[i] != "" {
				v := record[i]
				row[i] = &v
			}
		}
		rows = append(rows, row)
	}
	return header, rows, nil
}

func readCSV(raw []byte) ([]string, [][]string, error) {
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("parse psql output: %w", err)
	}
	if len(records) == 0 {
		return nil, nil, nil
	}
	return records[0], records[1:], nil
}
