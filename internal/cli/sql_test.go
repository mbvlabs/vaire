package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eru/internal/cli/output"
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
