package maint

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/mbvlabs/eru/internal/pg"
)

// Run kinds.
const (
	Vacuum  = "vacuum"
	Analyze = "analyze"
	Reindex = "reindex"
	Repack  = "repack"
)

// RunKinds are the maintenance runs.
var RunKinds = []string{Vacuum, Analyze, Reindex, Repack}

// Options are a maintenance run's flags.
type Options struct {
	Kind           string `json:"kind"`
	DB             string `json:"db"`
	Table          string `json:"table,omitempty"`
	Index          string `json:"index,omitempty"`
	Analyze        bool   `json:"analyze,omitempty"`
	Freeze         bool   `json:"freeze,omitempty"`
	Parallel       int    `json:"parallel,omitempty"`
	CleanupInvalid bool   `json:"cleanup_invalid,omitempty"`
	// NotConcurrently is reindex --concurrently=false, which eru refuses.
	NotConcurrently bool `json:"not_concurrently,omitempty"`
}

// Validate checks o before anything runs.
func (o Options) Validate() error {
	if !slices.Contains(RunKinds, o.Kind) {
		return fmt.Errorf("unknown maintenance %q", o.Kind)
	}
	if o.DB == "" {
		return errors.New("a maintenance run needs a database")
	}
	if o.Table != "" && o.Index != "" {
		return errors.New("pass --table or --index, not both")
	}
	if o.Index != "" && o.Kind != Reindex {
		return errors.New("--index is for reindex")
	}
	if o.Kind == Reindex && o.NotConcurrently {
		return errors.New("REINDEX without CONCURRENTLY blocks writes to the table; eru only reindexes concurrently")
	}
	if o.Parallel < 0 || o.Parallel > 16 {
		return errors.New("--parallel must be 0-16")
	}
	for _, name := range []string{o.Table, o.Index} {
		if name != "" {
			if _, err := QuoteRelation(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Heavy reports whether the run reads a whole database's worth of data and
// so takes the server's heavy-I/O lock, never running beside a full backup
// or a drill: a vacuum of the database, and every reindex.
func (o Options) Heavy() bool {
	return (o.Kind == Vacuum && o.Table == "") || o.Kind == Reindex
}

// Statements are the SQL statements of a run on one table or index, or of
// a reindex; empty for a database-wide vacuum or analyze, which runs
// vacuumdb (VacuumdbArgs).
func (o Options) Statements() ([]string, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	switch o.Kind {
	case Vacuum:
		if o.Table == "" {
			return nil, nil
		}
		t, _ := QuoteRelation(o.Table)
		opts := "ANALYZE " + boolWord(o.Analyze) + ", FREEZE " + boolWord(o.Freeze)
		if o.Parallel > 0 {
			opts += ", PARALLEL " + strconv.Itoa(o.Parallel)
		}
		return []string{"VACUUM (" + opts + ") " + t}, nil
	case Analyze:
		if o.Table == "" {
			return nil, nil
		}
		t, _ := QuoteRelation(o.Table)
		return []string{"ANALYZE " + t}, nil
	case Reindex:
		switch {
		case o.Index != "":
			i, _ := QuoteRelation(o.Index)
			return []string{"REINDEX INDEX CONCURRENTLY " + i}, nil
		case o.Table != "":
			t, _ := QuoteRelation(o.Table)
			return []string{"REINDEX TABLE CONCURRENTLY " + t}, nil
		}
		return []string{"REINDEX DATABASE CONCURRENTLY " + pg.Quote(o.DB)}, nil
	}
	return nil, nil
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// VacuumdbArgs are vacuumdb's arguments for a database-wide vacuum or
// analyze as user (eru_admin, or postgres on a protected database) over
// socketDir and port, with jobs workers.
func (o Options) VacuumdbArgs(socketDir, user string, port, jobs int) []string {
	if jobs < 1 {
		jobs = 1
	}
	args := []string{"-h", socketDir, "-p", strconv.Itoa(port), "-U", user, "--jobs=" + strconv.Itoa(jobs)}
	switch {
	case o.Kind == Analyze:
		args = append(args, "--analyze-only")
	case o.Analyze:
		args = append(args, "--analyze")
	}
	if o.Kind == Vacuum && o.Freeze {
		args = append(args, "--freeze")
	}
	return append(args, "--dbname="+o.DB)
}

// InvalidIndexesSQL lists the invalid indexes a failed concurrent reindex
// leaves behind (named *_ccnew*), as schema.name.
const InvalidIndexesSQL = `SELECT n.nspname || '.' || c.relname AS name
FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE NOT i.indisvalid AND c.relname LIKE '%\_ccnew%' ORDER BY 1`

// DropInvalidSQL drops one leftover index without blocking writes.
func DropInvalidSQL(name string) (string, error) {
	q, err := QuoteRelation(name)
	if err != nil {
		return "", err
	}
	return "DROP INDEX CONCURRENTLY IF EXISTS " + q, nil
}

// StaleTablesSQL lists the tables whose statistics are stale: more than 10%
// of their rows changed since the last analyze (maint-daily).
const StaleTablesSQL = `SELECT schemaname || '.' || relname AS name
FROM pg_stat_user_tables
WHERE n_mod_since_analyze > 0.1 * greatest(n_live_tup, 1000)
ORDER BY n_mod_since_analyze DESC`

// HasRepackSQL tells whether pg_repack is installed.
const HasRepackSQL = `SELECT count(*) AS n FROM pg_extension WHERE extname = 'pg_repack'`
