package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/pg"
)

func TestReadOnlyCopyError(t *testing.T) {
	recovery := fmt.Errorf("begin: %w", &pgconn.PgError{Code: "0A000", Message: "cannot set transaction read-write mode during recovery"})
	other := &pgconn.PgError{Code: "42601", Message: "syntax error"}
	cases := []struct {
		name string
		db   string
		err  error
		want bool
	}{
		{"restoring copy in recovery", "app2__restoring", recovery, true},
		{"message without sqlstate", "app2__restoring", errors.New("cannot set transaction read-write mode during recovery"), true},
		{"other error on the copy", "app2__restoring", other, false},
		{"live database", "app2", recovery, false},
		{"drill copy", "app2__drill", recovery, false},
	}
	for _, c := range cases {
		err := readOnlyCopyError(c.db, c.err)
		if (err != nil) != c.want {
			t.Fatalf("%s: got %v, want mapped=%v", c.name, err, c.want)
		}
		if err == nil {
			continue
		}
		var cliErr *output.CLIError
		if !errors.As(err, &cliErr) || cliErr.Code != output.CodePrecondition {
			t.Fatalf("%s: got %v, want precondition_failed", c.name, err)
		}
	}
}

func TestSQLDataSeveralStatements(t *testing.T) {
	one := "1"
	results := []*pg.Result{
		{CommandTag: "CREATE TABLE"},
		{Columns: []string{"n"}, Rows: [][]*string{{&one}}, CommandTag: "SELECT 1", RowCount: 1},
	}
	data := newSQLData(results)
	human := data.render()
	if !strings.Contains(human, "CREATE TABLE") || !strings.Contains(human, "(1 rows)") {
		t.Fatalf("render = %q, want both statements", human)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Columns    []string `json:"columns"`
		CommandTag string   `json:"command_tag"`
		Results    []struct {
			CommandTag string `json:"command_tag"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.CommandTag != "SELECT 1" || len(got.Columns) != 1 || len(got.Results) != 2 || got.Results[0].CommandTag != "CREATE TABLE" {
		t.Fatalf("json = %s", raw)
	}
}

func TestSQLDataOneStatementKeepsShape(t *testing.T) {
	data := newSQLData([]*pg.Result{{CommandTag: "UPDATE 3"}})
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "results") || !strings.Contains(string(raw), `"command_tag":"UPDATE 3"`) {
		t.Fatalf("json = %s", raw)
	}
	if data.render() != "UPDATE 3" {
		t.Fatalf("render = %q", data.render())
	}
	if newSQLData(nil).render() != "no rows" {
		t.Fatal("no statements should render as no rows")
	}
}
