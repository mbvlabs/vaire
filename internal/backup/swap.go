package backup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/pg"
)

// PreRestoreName is the name a promote gives the replaced database:
// <db>__pre_restore_<UTC yyyymmddThhmm>.
func PreRestoreName(db string, at time.Time) string {
	return db + "__pre_restore_" + at.UTC().Format("20060102T1504")
}

// RestoringName is the database a restore of db loads into.
func RestoringName(db string) string { return db + "__restoring" }

// MaxRestorableName is the longest database name whose restore names still
// fit PostgreSQL's 63-byte identifiers.
const MaxRestorableName = 63 - len("__pre_restore_20060102T1504")

// Swap replaces live with replacement and keeps the old copy as aside, in a
// few seconds:
//
//  1. no new connections to either database, and every remaining backend on
//     them is terminated (direct 5432 sessions and PgBouncer's server
//     connections alike);
//  2. ALTER DATABASE <live> RENAME TO <aside>, then <replacement> to <live>;
//  3. <live> accepts connections again; <aside> stays closed.
//
// Only this database's clients see errors, for those few seconds. PgBouncer
// isn't paused: its next server connection for <live> opens by name and lands
// on the replacement. Role names and passwords don't change, so applications
// reconnect with the same credentials.
func Swap(ctx context.Context, local pg.Local, live, replacement, aside string) error {
	if err := closeDatabase(ctx, local, live); err != nil {
		_ = openDatabase(context.WithoutCancel(ctx), local, live)
		return err
	}
	if err := closeDatabase(ctx, local, replacement); err != nil {
		_ = openDatabase(context.WithoutCancel(ctx), local, live)
		return err
	}
	if err := rename(ctx, local, live, aside); err != nil {
		_ = openDatabase(context.WithoutCancel(ctx), local, live)
		_ = openDatabase(context.WithoutCancel(ctx), local, replacement)
		return err
	}
	if err := rename(ctx, local, replacement, live); err != nil {
		// Put the original back under its name.
		_ = rename(context.WithoutCancel(ctx), local, aside, live)
		_ = openDatabase(context.WithoutCancel(ctx), local, live)
		_ = openDatabase(context.WithoutCancel(ctx), local, replacement)
		return err
	}
	return openDatabase(ctx, local, live)
}

// closeDatabase stops new connections to db and terminates the ones it has.
func closeDatabase(ctx context.Context, local pg.Local, db string) error {
	sql := fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS false;\n%s", pg.Quote(db), terminateSQL(db))
	return local.Exec(ctx, "postgres", sql, nil)
}

// openDatabase lets connections in again.
func openDatabase(ctx context.Context, local pg.Local, db string) error {
	return local.Exec(ctx, "postgres", fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS true;\n", pg.Quote(db)), nil)
}

func terminateSQL(db string) string {
	return "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = '" + db + "' AND pid <> pg_backend_pid();\n"
}

// rename renames from to to, terminating stragglers and retrying while
// PostgreSQL still reports the database as in use.
func rename(ctx context.Context, local pg.Local, from, to string) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = local.Exec(ctx, "postgres", fmt.Sprintf("ALTER DATABASE %s RENAME TO %s;\n", pg.Quote(from), pg.Quote(to)), nil)
		if err == nil || !strings.Contains(err.Error(), "being accessed by other users") {
			return err
		}
		_ = local.Exec(ctx, "postgres", terminateSQL(from), nil)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return err
}
