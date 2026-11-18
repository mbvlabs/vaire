package node

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// fakeUnfollowRenameSQL models the follower cluster a db unfollow renames on.
// It reuses the fork rename fake for the catalog, sessions and the shared SQL,
// and adds the follower's role catalog. Only the statements the production
// helper actually sends are applied, so the tests observe real ordering and
// postconditions.
type fakeUnfollowRenameSQL struct {
	*fakeForkRenameSQL
	roles map[string]bool
	// oidDeadlineSeen records that a captured-OID read ran with a deadline,
	// which is the bounded compensation context after the parent was cancelled.
	oidDeadlineSeen bool
}

func newFakeUnfollowRenameSQL() *fakeUnfollowRenameSQL {
	base := newFakeForkRenameSQL()
	base.port = "5434"
	base.dataDir = "/var/lib/postgresql/18/app_follower"
	base.databases = []*fakeForkDatabase{{oid: "42", name: "app", allow: true}}

	return &fakeUnfollowRenameSQL{
		fakeForkRenameSQL: base,
		roles: map[string]bool{
			"app_owner": true, "app_migrate": true, "app_app": true, "app_ro": true,
		},
	}
}

// target is the follower cluster's ownership-bound rename endpoint.
func (f *fakeUnfollowRenameSQL) target() unfollowRenameTarget {
	return unfollowRenameTarget{
		local: f, cluster: "app_follower", port: 5434,
		dataDir:       "/var/lib/postgresql/18/app_follower",
		sourcePort:    5433,
		sourceDataDir: "/var/lib/postgresql/18/app",
	}
}

func (f *fakeUnfollowRenameSQL) setDatabases(names ...string) {
	f.databases = nil
	for i, name := range names {
		f.databases = append(f.databases, &fakeForkDatabase{oid: strconv.Itoa(42 + i), name: name, allow: true})
	}
}

func (f *fakeUnfollowRenameSQL) targetRoles() {
	f.roles = map[string]bool{
		"app_follower_owner": true, "app_follower_migrate": true,
		"app_follower_app": true, "app_follower_ro": true,
	}
}

func (f *fakeUnfollowRenameSQL) Query(ctx context.Context, db, sql string, vars map[string]string) ([]map[string]string, error) {
	switch {
	case strings.Contains(sql, "FROM pg_roles"):
		f.queries = append(f.queries, sql)

		rows := make([]map[string]string, 0, len(f.roles))
		for role := range f.roles {
			rows = append(rows, map[string]string{"rolname": role})
		}

		sort.Slice(rows, func(i, j int) bool { return rows[i]["rolname"] < rows[j]["rolname"] })

		return rows, nil
	case strings.Contains(sql, "FROM pg_database WHERE datname IN"):
		f.queries = append(f.queries, sql)

		rows := f.databaseRows(sql)
		for _, row := range rows {
			// readRenames selects datname; the capture and gate use the same
			// catalog identity under its name column.
			row["datname"] = row["name"]
		}

		return rows, nil
	}

	if strings.Contains(sql, "FROM pg_database WHERE oid =") {
		if _, ok := ctx.Deadline(); ok {
			f.oidDeadlineSeen = true
		}
	}

	return f.fakeForkRenameSQL.Query(ctx, db, sql, vars)
}

func (f *fakeUnfollowRenameSQL) Exec(ctx context.Context, db, sql string, vars map[string]string) error {
	if strings.HasPrefix(sql, "BEGIN;") {
		// PostgreSQL refuses RENAME while another backend is connected to
		// the database, so the fake must do the same: a missing drain fails
		// here rather than passing silently.
		for _, database := range f.databases {
			if f.sessionCount(database.oid) != 0 {
				f.executed = append(f.executed, sql)
				return fmt.Errorf("database %q is being accessed by other users", database.name)
			}
		}
	}

	if strings.HasPrefix(sql, "BEGIN;") || strings.Contains(sql, "ALLOW_CONNECTIONS") {
		return f.fakeForkRenameSQL.Exec(ctx, db, sql, vars)
	}

	f.executed = append(f.executed, sql)

	return nil
}

func (f *fakeUnfollowRenameSQL) last() string { return strings.Join(f.executed, "\n") }

// EFIN-1: the unfollow rename proves its endpoint, captures the source
// database's OID and original flag, refuses new connections, drains only that
// OID's sessions, and restores the flag in the rename transaction.
func TestUnfollowRenameQuiescesFollowerOIDAndRestoresOriginalFlag(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.addSession("42", "pg_cron launcher")
	fake.addSession("42", "app")
	fake.addSession("7", "pg_cron launcher")
	fake.addPrepared("7")

	mustNoErr(t, renameFollower(context.Background(), fake.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow"))
	rejectf(t, fake.sessionCount("42") != 0, "captured database sessions remain: %d", fake.sessionCount("42"))
	rejectf(t, fake.sessionCount("7") != 1, "a session in another database was terminated: %d", fake.sessionCount("7"))
	database := fake.dbByName("app_follower")
	rejectf(t, database == nil || database.oid != "42" || !database.allow, "renamed database state = %+v", database)

	for _, query := range fake.queries {
		rejectf(t, strings.Contains(query, "application_name"), "the drain selected by application_name: %s", query)
	}

	checkUnfollowRenameTransaction(t, fake)
}

// checkUnfollowRenameTransaction proves the gate/drain/rename order and the
// transaction body the follower rename sends.
func checkUnfollowRenameTransaction(t *testing.T, fake *fakeUnfollowRenameSQL) {
	t.Helper()

	attestEvent := fake.indexOfEvent("query:attest")
	gateEvent := fake.indexOfEvent("exec:allow")
	terminateEvent := fake.indexOfEvent("query:terminate")
	transactionEvent := fake.indexOfEvent("exec:rename")
	rejectf(t, attestEvent < 0 || gateEvent < 0 || terminateEvent < 0 || transactionEvent < 0,
		"missing effect: attest=%d gate=%d terminate=%d transaction=%d", attestEvent, gateEvent, terminateEvent, transactionEvent)
	rejectf(t, attestEvent >= gateEvent || gateEvent >= terminateEvent || terminateEvent >= transactionEvent,
		"effect order: attest=%d gate=%d terminate=%d transaction=%d", attestEvent, gateEvent, terminateEvent, transactionEvent)

	transaction := fake.indexOfBeginTransaction()
	rejectf(t, transaction < 0, "no rename transaction: %v", fake.executed)

	body := fake.executed[transaction]
	for _, want := range []string{
		`ALTER DATABASE "app" RENAME TO "app_follower";`,
		`ALTER DATABASE "app_follower" ALLOW_CONNECTIONS true;`,
		"DO $vaire$",
		"COMMIT;",
	} {
		rejectf(t, !strings.Contains(body, want), "rename transaction lacks %q:\n%s", want, body)
	}

	rejectf(t, strings.Contains(body, `ALTER DATABASE "app" ALLOW_CONNECTIONS false;`),
		"the connection gate shares the rename transaction:\n%s", body)
	rejectf(t, fake.indexOfExec(`ALTER DATABASE "app" ALLOW_CONNECTIONS false;`) < 0,
		"no committed connection gate: %v", fake.executed)
	terminate := fake.indexOfQuery("pg_terminate_backend")
	rejectf(t, terminate < 0 || !strings.Contains(fake.queries[terminate], "datid = 42"),
		"termination did not select the captured OID: %v", fake.queries)
	rejectf(t, terminate < 0 || !strings.Contains(fake.queries[terminate], "pg_terminate_backend(pid, 2000)"),
		"termination timeout is not the bounded positive value: %v", fake.queries)
}

// EFIN-1: a database that already disallowed connections keeps that value.
func TestUnfollowRenameKeepsOriginalAllowConnectionsFalse(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.dbByName("app").allow = false
	fake.addSession("42", "pg_cron launcher")

	if err := renameFollower(context.Background(), fake.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow"); err != nil {
		t.Fatal(err)
	}

	database := fake.dbByName("app_follower")
	if database == nil || database.allow {
		t.Fatalf("original allow_connections=false was not restored: %+v", database)
	}

	for _, sql := range fake.executed {
		if strings.Contains(sql, "ALLOW_CONNECTIONS true") {
			t.Fatalf("the rename enabled the database despite the original flag:\n%s", sql)
		}
	}
}

// EFIN-1: the rename refuses a runner that is not the follower cluster named by
// cluster.toml, before any write.
func TestUnfollowRenameRefusesUnattestedFollowerEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeUnfollowRenameSQL, *unfollowRenameTarget)
	}{
		{name: "wrong port", mutate: func(f *fakeUnfollowRenameSQL, _ *unfollowRenameTarget) { f.port = "5433" }},
		{name: "malformed port", mutate: func(f *fakeUnfollowRenameSQL, _ *unfollowRenameTarget) { f.port = "port" }},
		{name: "wrong data directory", mutate: func(f *fakeUnfollowRenameSQL, _ *unfollowRenameTarget) {
			f.dataDir = "/var/lib/postgresql/18/app"
		}},
		{name: "source cluster name", mutate: func(_ *fakeUnfollowRenameSQL, target *unfollowRenameTarget) {
			target.cluster = "app"
		}},
		{name: "source port", mutate: func(f *fakeUnfollowRenameSQL, target *unfollowRenameTarget) {
			f.port = "5433"
			target.port = 5433
		}},
		{name: "source data directory", mutate: func(f *fakeUnfollowRenameSQL, target *unfollowRenameTarget) {
			f.dataDir = "/var/lib/postgresql/18/app"
			target.dataDir = "/var/lib/postgresql/18/app"
		}},
		{name: "missing source port", mutate: func(_ *fakeUnfollowRenameSQL, target *unfollowRenameTarget) {
			target.sourcePort = 0
		}},
		{name: "missing port", mutate: func(_ *fakeUnfollowRenameSQL, target *unfollowRenameTarget) {
			target.port = 0
		}},
		{name: "relative data directory", mutate: func(_ *fakeUnfollowRenameSQL, target *unfollowRenameTarget) {
			target.dataDir = "18/app_follower"
		}},
		{name: "relative source data directory", mutate: func(_ *fakeUnfollowRenameSQL, target *unfollowRenameTarget) {
			target.sourceDataDir = "18/app"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeUnfollowRenameSQL()
			target := fake.target()
			tc.mutate(fake, &target)

			err := renameFollower(context.Background(), target, "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow")
			if err == nil {
				t.Fatal("an unattested follower endpoint was accepted")
			}

			if len(fake.executed) != 0 {
				t.Fatalf("an unattested endpoint was mutated: %v", fake.executed)
			}
		})
	}
}

// EFIN-1: a cron-like launcher reconnecting between termination rounds is
// bounded and the original flag is restored.
func TestUnfollowRenameDrainBoundsReconnectingCronLauncher(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.reconnect = true
	fake.addSession("42", "pg_cron launcher")

	err := renameFollower(context.Background(), fake.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow")
	if err == nil || !strings.Contains(err.Error(), "unresolved session") {
		t.Fatalf("reconnecting launcher = %v", err)
	}

	if got := strings.Count(strings.Join(fake.queries, "\n"), "pg_terminate_backend(pid, 2000)"); got != unfollowRenameDrainRounds {
		t.Fatalf("termination rounds = %d, want the bound %d", got, unfollowRenameDrainRounds)
	}

	if fake.indexOfBeginTransaction() >= 0 {
		t.Fatal("the rename ran with unresolved sessions")
	}

	database := fake.dbByName("app")
	if database == nil || !database.allow {
		t.Fatalf("compensation did not restore the original flag: %+v", database)
	}
}

// EFIN-1: a failed rename restores the original flag.
func TestUnfollowRenameFailureCompensatesOriginalFlag(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.failRename = errFakeForkRename

	err := renameFollower(context.Background(), fake.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow")
	if !errors.Is(err, errFakeForkRename) {
		t.Fatalf("rename failure = %v", err)
	}

	database := fake.dbByName("app")
	if database == nil || !database.allow {
		t.Fatalf("compensation did not restore the original flag: %+v", database)
	}

	if fake.indexOfExec(`ALTER DATABASE "app" ALLOW_CONNECTIONS true;`) < 0 {
		t.Fatalf("compensation did not run by OID: %v", fake.executed)
	}
}

// EFIN-1: cancellation compensates with a detached, bounded context.
func TestUnfollowRenameCancellationCompensatesWithDetachedContext(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake.cancelOnRename = cancel

	err := renameFollower(ctx, fake.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rename = %v", err)
	}

	if ctx.Err() == nil {
		t.Fatal("the test context was not cancelled")
	}

	database := fake.dbByName("app")
	if database == nil || !database.allow {
		t.Fatalf("cancellation left the database fenced: %+v", database)
	}

	if !fake.oidDeadlineSeen {
		t.Fatal("compensation did not run with a bounded deadline")
	}
}

// EFIN-1: an ambiguous commit is settled through the captured OID, never a
// guessed name.
func TestUnfollowRenameAmbiguousCommitRestoresCapturedOID(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.ambiguousRename = true

	err := renameFollower(context.Background(), fake.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow")
	if !errors.Is(err, errFakeForkAmbiguousCommit) {
		t.Fatalf("ambiguous commit = %v", err)
	}

	database := fake.dbByName("app_follower")
	if database == nil || database.oid != "42" || !database.allow {
		t.Fatalf("ambiguous commit did not settle the same OID: %+v", database)
	}

	if fake.indexOfQuery("FROM pg_database WHERE oid = 42") < 0 {
		t.Fatalf("compensation did not inspect the captured OID: %v", fake.queries)
	}

	if fake.indexOfExec(`ALTER DATABASE "app_follower" ALLOW_CONNECTIONS true;`) >= 0 {
		t.Fatalf("compensation blindly changed the current name: %v", fake.executed)
	}
}

// EFIN-1: missing passwords refuse before the gate.
func TestUnfollowRenamePreparesPasswordsBeforeGate(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()

	err := renameFollower(context.Background(), fake.target(), "app", "app_follower", map[string]string{}, nil, "db-unfollow")
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("missing passwords = %v", err)
	}

	if len(fake.executed) != 0 {
		t.Fatalf("invalid rename SQL mutated the cluster: %v", fake.executed)
	}
}

// EFIN-1: prepared transactions are refused before the gate, not guessed away.
func TestUnfollowRenameRefusesPreparedTransactionBeforeGate(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.addPrepared("42")

	err := renameFollower(context.Background(), fake.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow")
	if err == nil || !strings.Contains(err.Error(), "prepared transactions") {
		t.Fatalf("prepared transaction = %v", err)
	}

	if len(fake.executed) != 0 {
		t.Fatalf("a prepared transaction was renamed around: %v", fake.executed)
	}
}

// EFIN-1: after the rename succeeds but a later settlement fails, the retry
// sees the follower's database name already and only finishes roles and
// passwords, without a gate, a drain or a source-name assumption.
func TestUnfollowRenameRetryAfterRenameSkipsGate(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.addSession("42", "pg_cron launcher")

	passwords := rolePasswords("app_follower")
	if err := renameFollower(context.Background(), fake.target(), "app", "app_follower", passwords, nil, "db-unfollow"); err != nil {
		t.Fatal(err)
	}

	fake.targetRoles()
	fake.addSession("42", "pg_cron launcher")
	fake.executed, fake.queries, fake.events = nil, nil, nil
	fake.oidDeadlineSeen = false

	if err := renameFollower(context.Background(), fake.target(), "app", "app_follower", passwords, nil, "db-unfollow"); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if fake.indexOfEvent("exec:allow") >= 0 || fake.indexOfEvent("query:terminate") >= 0 {
		t.Fatalf("the retry gated or drained an already-renamed database: %v %v", fake.events, fake.executed)
	}

	if got := fake.sessionCount("42"); got != 1 {
		t.Fatalf("the retry terminated a session in the already-renamed database: %d", got)
	}

	if fake.indexOfBeginTransaction() >= 0 {
		t.Fatalf("the retry ran a database rename transaction: %v", fake.executed)
	}

	if sql := fake.last(); strings.Contains(sql, "ALTER DATABASE") {
		t.Fatalf("the retry named the gone source database:\n%s", sql)
	}

	database := fake.dbByName("app_follower")
	if database == nil || database.oid != "42" || !database.allow {
		t.Fatalf("the retry changed the renamed database: %+v", database)
	}
}

// EFIN-1: the maintenance database itself is refused before the gate, because
// fencing it would also block the compensation connection.
func TestUnfollowRenameRefusesPostgresMaintenanceDatabase(t *testing.T) {
	fake := newFakeUnfollowRenameSQL()
	fake.setDatabases("postgres")
	fake.roles = map[string]bool{"postgres_owner": true, "postgres_migrate": true, "postgres_app": true, "postgres_ro": true}

	err := renameFollower(context.Background(), fake.target(), "postgres", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow")
	if err == nil || !strings.Contains(err.Error(), "maintenance connection") {
		t.Fatalf("rename of the maintenance database = %v", err)
	}

	if len(fake.executed) != 0 {
		t.Fatalf("the maintenance database was mutated: %v", fake.executed)
	}
}

// EFIN-1: the rename helper only ever addresses the follower's runner. A
// source cluster with the same database name is untouched.
func TestUnfollowRenameLeavesTheSourceClusterAlone(t *testing.T) {
	source := newFakeUnfollowRenameSQL()
	source.setDatabases("app")
	source.addSession("42", "app")

	follower := newFakeUnfollowRenameSQL()
	follower.addSession("42", "pg_cron launcher")

	if err := renameFollower(context.Background(), follower.target(), "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow"); err != nil {
		t.Fatal(err)
	}

	if len(source.executed) != 0 || len(source.queries) != 0 {
		t.Fatalf("the source cluster took SQL: exec=%v query=%v", source.executed, source.queries)
	}

	if got := source.sessionCount("42"); got != 1 {
		t.Fatalf("the source cluster's session was terminated: %d", got)
	}
}
