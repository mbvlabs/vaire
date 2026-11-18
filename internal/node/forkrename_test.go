package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/pg"
)

var (
	errFakeForkRename           = errors.New("rename transaction failed")
	errFakeForkAmbiguousCommit  = errors.New("commit outcome unknown")
	errFakeForkOwnership        = errors.New("ownership rejected")
	fakeForkAllowStatement      = regexp.MustCompile(`ALTER DATABASE ("(?:[^"]|"")*") ALLOW_CONNECTIONS (true|false);`)
	fakeForkRenameStatement     = regexp.MustCompile(`ALTER DATABASE ("(?:[^"]|"")*") RENAME TO ("(?:[^"]|"")*");`)
	fakeForkOIDPredicate        = regexp.MustCompile(`oid = ([0-9]+)`)
	fakeForkDatIDPredicate      = regexp.MustCompile(`datid = ([0-9]+)`)
	fakeForkPreparedPredicate   = regexp.MustCompile(`d\.oid = ([0-9]+)`)
	fakeForkPostcondition       = regexp.MustCompile(`oid = ([0-9]+) AND datname = '([^']*)' AND datallowconn = (true|false)`)
	fakeForkStringLiteral       = regexp.MustCompile(`'([^']*)'`)
	fakeForkTerminateTimeoutSQL = regexp.MustCompile(`pg_terminate_backend\(pid, ([1-9][0-9]*)\)`)
)

type fakeForkDatabase struct {
	oid   string
	name  string
	allow bool
}

type fakeForkSession struct {
	pid int
	oid string
	app string
}

// fakeForkRenameSQL models the owned copy's catalog, sessions and the SQL the
// rename helper issues. It applies only the statements the helper actually
// sends, so tests observe the real ordering and postconditions.
type fakeForkRenameSQL struct {
	port            string
	dataDir         string
	databases       []*fakeForkDatabase
	sessions        []*fakeForkSession
	prepared        map[string]int
	executed        []string
	queries         []string
	events          []string
	nextPID         int
	reconnect       bool
	cancelOnRename  context.CancelFunc
	ambiguousRename bool
	failRename      error
	onGate          func(*fakeForkRenameSQL)
}

func newFakeForkRenameSQL() *fakeForkRenameSQL {
	return &fakeForkRenameSQL{
		port:      "5480",
		dataDir:   "/var/lib/postgresql/18/target",
		databases: []*fakeForkDatabase{{oid: "42", name: "app", allow: true}},
		prepared:  map[string]int{},
		nextPID:   100,
	}
}

func (f *fakeForkRenameSQL) addSession(oid, app string) {
	f.nextPID++
	f.sessions = append(f.sessions, &fakeForkSession{pid: f.nextPID, oid: oid, app: app})
}

func (f *fakeForkRenameSQL) addPrepared(oid string) { f.prepared[oid] = 1 }

func (f *fakeForkRenameSQL) dbByName(name string) *fakeForkDatabase {
	for _, database := range f.databases {
		if database.name == name {
			return database
		}
	}

	return nil
}

func (f *fakeForkRenameSQL) dbByOID(oid string) *fakeForkDatabase {
	for _, database := range f.databases {
		if database.oid == oid {
			return database
		}
	}

	return nil
}

func (f *fakeForkRenameSQL) sessionCount(oid string) int {
	count := 0

	for _, session := range f.sessions {
		if session.oid == oid {
			count++
		}
	}

	return count
}

func (f *fakeForkRenameSQL) setAllow(name string, allow bool) error {
	database := f.dbByName(name)
	if database == nil {
		return fmt.Errorf("database %q does not exist", name)
	}

	database.allow = allow

	return nil
}

func (f *fakeForkRenameSQL) renameDatabase(from, to string) error {
	database := f.dbByName(from)
	if database == nil {
		return fmt.Errorf("database %q does not exist", from)
	}

	if f.dbByName(to) != nil {
		return fmt.Errorf("database %q already exists", to)
	}

	database.name = to

	return nil
}

func (f *fakeForkRenameSQL) Exec(ctx context.Context, _ string, sql string, _ map[string]string) error {
	f.executed = append(f.executed, sql)
	if strings.HasPrefix(sql, "BEGIN;") {
		return f.execRename(ctx, sql)
	}

	return f.execAllow(ctx, sql)
}

func (f *fakeForkRenameSQL) execRename(ctx context.Context, sql string) error {
	f.events = append(f.events, "exec:rename")
	if f.cancelOnRename != nil {
		f.cancelOnRename()
		return context.Canceled
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if f.failRename != nil {
		return f.failRename
	}

	if err := f.applyTransaction(sql); err != nil {
		return err
	}

	if f.ambiguousRename {
		return errFakeForkAmbiguousCommit
	}

	return nil
}

func (f *fakeForkRenameSQL) execAllow(ctx context.Context, sql string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.events = append(f.events, "exec:allow")

	matches := fakeForkAllowStatement.FindAllStringSubmatch(sql, -1)
	if len(matches) == 0 {
		return fmt.Errorf("unexpected exec: %s", sql)
	}

	gate := false

	for _, match := range matches {
		allow := match[2] == "true"

		gate = gate || !allow
		if err := f.setAllow(fakeForkUnquote(match[1]), allow); err != nil {
			return err
		}
	}

	if gate && f.onGate != nil {
		f.onGate(f)
	}

	return nil
}

func (f *fakeForkRenameSQL) applyTransaction(sql string) error {
	if match := fakeForkRenameStatement.FindStringSubmatch(sql); match != nil {
		if err := f.renameDatabase(fakeForkUnquote(match[1]), fakeForkUnquote(match[2])); err != nil {
			return err
		}
	}

	for _, match := range fakeForkAllowStatement.FindAllStringSubmatch(sql, -1) {
		if err := f.setAllow(fakeForkUnquote(match[1]), match[2] == "true"); err != nil {
			return err
		}
	}

	if strings.Contains(sql, "DO $vaire$") {
		match := fakeForkPostcondition.FindStringSubmatch(sql)
		if match == nil {
			return errors.New("missing rename postcondition")
		}

		database := f.dbByOID(match[1])
		if database == nil || database.name != match[2] || database.allow != (match[3] == "true") {
			return errors.New("rename postcondition failed")
		}
	}

	return nil
}

func (f *fakeForkRenameSQL) Query(ctx context.Context, _ string, sql string, _ map[string]string) ([]map[string]string, error) {
	f.queries = append(f.queries, sql)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	switch {
	case strings.Contains(sql, "current_setting('port')"):
		f.events = append(f.events, "query:attest")
		return []map[string]string{{"port": f.port, "data_dir": f.dataDir}}, nil
	case strings.Contains(sql, "FROM pg_database WHERE datname IN"):
		f.events = append(f.events, "query:database")
		return f.databaseRows(sql), nil
	case strings.Contains(sql, "FROM pg_database WHERE oid ="):
		f.events = append(f.events, "query:oid")

		match := fakeForkOIDPredicate.FindStringSubmatch(sql)
		if match == nil {
			return nil, errors.New("oid predicate missing")
		}

		database := f.dbByOID(match[1])
		if database == nil {
			return nil, nil
		}

		return []map[string]string{{"name": database.name, "allowconn": fakeForkBool(database.allow)}}, nil
	case strings.Contains(sql, "pg_prepared_xacts"):
		f.events = append(f.events, "query:prepared")

		if !strings.Contains(sql, "FROM pg_prepared_xacts p JOIN pg_database d ON d.datname = p.database WHERE d.oid = ") {
			return nil, fmt.Errorf("prepared query does not join pg_database by datname and filter the captured OID: %s", sql)
		}

		match := fakeForkPreparedPredicate.FindStringSubmatch(sql)
		if match == nil {
			return nil, errors.New("prepared OID predicate missing")
		}

		return []map[string]string{{"n": strconv.Itoa(f.prepared[match[1]])}}, nil
	case strings.Contains(sql, "pg_terminate_backend"):
		f.events = append(f.events, "query:terminate")

		if fakeForkTerminateTimeoutSQL.FindStringSubmatch(sql) == nil {
			return nil, errors.New("termination timeout missing")
		}

		match := fakeForkDatIDPredicate.FindStringSubmatch(sql)
		if match == nil {
			return nil, errors.New("datid predicate missing")
		}

		return f.terminate(match[1]), nil
	case strings.Contains(sql, "FROM pg_stat_activity"):
		f.events = append(f.events, "query:sessions")

		match := fakeForkDatIDPredicate.FindStringSubmatch(sql)
		if match == nil {
			return nil, errors.New("datid predicate missing")
		}

		return f.sessionRows(match[1]), nil
	}

	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func (f *fakeForkRenameSQL) databaseRows(sql string) []map[string]string {
	rows := make([]map[string]string, 0, 2)

	for _, match := range fakeForkStringLiteral.FindAllStringSubmatch(sql, -1) {
		database := f.dbByName(match[1])
		if database == nil {
			continue
		}

		rows = append(rows, map[string]string{"oid": database.oid, "name": database.name, "allowconn": fakeForkBool(database.allow)})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i]["oid"] < rows[j]["oid"] })

	return rows
}

func (f *fakeForkRenameSQL) sessionRows(oid string) []map[string]string {
	rows := make([]map[string]string, 0, len(f.sessions))
	for _, session := range f.sessions {
		if session.oid == oid {
			rows = append(rows, map[string]string{"pid": strconv.Itoa(session.pid), "application_name": session.app})
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i]["pid"] < rows[j]["pid"] })

	return rows
}

func (f *fakeForkRenameSQL) terminate(oid string) []map[string]string {
	kept := f.sessions[:0]

	rows := make([]map[string]string, 0, len(f.sessions))
	for _, session := range f.sessions {
		if session.oid != oid {
			kept = append(kept, session)
			continue
		}

		rows = append(rows, map[string]string{"pid": strconv.Itoa(session.pid), "terminated": "true"})
	}

	f.sessions = kept
	if f.reconnect {
		f.addSession(oid, "pg_cron scheduler")
	}

	return rows
}

func fakeForkUnquote(identifier string) string {
	return strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(identifier, `"`), `"`), `""`, `"`)
}

func fakeForkBool(value bool) string {
	if value {
		return "t"
	}

	return "f"
}

func fakeForkRenameTarget(fake *fakeForkRenameSQL) forkRenameCopy {
	return forkRenameCopy{local: fake, port: 5480, dataDir: "/var/lib/postgresql/18/target"}
}

func (f *fakeForkRenameSQL) indexOfEvent(event string) int {
	for i, got := range f.events {
		if got == event {
			return i
		}
	}

	return -1
}

func (f *fakeForkRenameSQL) indexOfExec(exact string) int {
	for i, sql := range f.executed {
		if sql == exact {
			return i
		}
	}

	return -1
}

// indexOfBeginTransaction reports where the rename transaction body starts.
func (f *fakeForkRenameSQL) indexOfBeginTransaction() int {
	for i, sql := range f.executed {
		if strings.HasPrefix(sql, "BEGIN;") {
			return i
		}
	}

	return -1
}

func (f *fakeForkRenameSQL) indexOfQuery(part string) int {
	for i, sql := range f.queries {
		if strings.Contains(sql, part) {
			return i
		}
	}

	return -1
}

func TestForkRenameQuiescesOnlyCapturedOIDAndRestoresOriginalFlag(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.addSession("42", "pg_cron scheduler")
	fake.addSession("42", "app")
	fake.addSession("7", "pg_cron scheduler")
	fake.addPrepared("7")

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	if err != nil {
		t.Fatal(err)
	}

	if fake.sessionCount("42") != 0 {
		t.Fatalf("captured database sessions remain: %d", fake.sessionCount("42"))
	}

	if fake.sessionCount("7") != 1 {
		t.Fatalf("a session in another database was terminated: %d", fake.sessionCount("7"))
	}

	database := fake.dbByName("target")
	if database == nil || database.oid != "42" || !database.allow {
		t.Fatalf("renamed database state = %+v", database)
	}

	if fake.indexOfQuery("application_name") >= 0 {
		t.Fatal("the drain selected by application_name")
	}

	gateEvent := fake.indexOfEvent("exec:allow")
	transactionEvent := fake.indexOfEvent("exec:rename")
	terminateEvent := fake.indexOfEvent("query:terminate")

	attestEvent := fake.indexOfEvent("query:attest")
	if attestEvent < 0 || gateEvent < 0 || terminateEvent < 0 || transactionEvent < 0 {
		t.Fatalf("missing effect: attest=%d gate=%d terminate=%d transaction=%d", attestEvent, gateEvent, terminateEvent, transactionEvent)
	}

	if attestEvent >= gateEvent || gateEvent >= terminateEvent || terminateEvent >= transactionEvent {
		t.Fatalf("effect order: attest=%d gate=%d terminate=%d transaction=%d", attestEvent, gateEvent, terminateEvent, transactionEvent)
	}

	transaction := fake.indexOfBeginTransaction()
	terminate := fake.indexOfQuery("pg_terminate_backend")

	body := fake.executed[transaction]
	for _, want := range []string{
		`ALTER DATABASE "app" RENAME TO "target";`,
		`ALTER DATABASE "target" ALLOW_CONNECTIONS true;`,
		"DO $vaire$",
		"COMMIT;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rename transaction lacks %q:\n%s", want, body)
		}
	}

	if strings.Contains(body, `ALTER DATABASE "app" ALLOW_CONNECTIONS false;`) {
		t.Errorf("the connection gate shares the rename transaction:\n%s", body)
	}

	if !strings.Contains(fake.queries[terminate], "datid = 42") {
		t.Errorf("termination did not select the captured OID: %s", fake.queries[terminate])
	}

	if !strings.Contains(fake.queries[terminate], "pg_terminate_backend(pid, 2000)") {
		t.Errorf("termination timeout is not the bounded positive value: %s", fake.queries[terminate])
	}
}

func TestForkRenameKeepsOriginalAllowConnectionsFalse(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.dbByName("app").allow = false
	fake.addSession("42", "pg_cron scheduler")

	if err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target")); err != nil {
		t.Fatal(err)
	}

	database := fake.dbByName("target")
	if database == nil || database.allow {
		t.Fatalf("original allow_connections=false was not restored: %+v", database)
	}

	for _, sql := range fake.executed {
		if strings.Contains(sql, `ALTER DATABASE "target" ALLOW_CONNECTIONS true;`) {
			t.Fatalf("the rename enabled the database despite the original flag:\n%s", sql)
		}
	}
}

func TestForkRenamePreparesSQLBeforeAnyMutation(t *testing.T) {
	fake := newFakeForkRenameSQL()

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", map[string]string{})
	if err == nil {
		t.Fatal("missing passwords were accepted")
	}

	if len(fake.executed) != 0 || len(fake.queries) != 0 {
		t.Fatalf("invalid rename SQL mutated or queried: exec=%v query=%v", fake.executed, fake.queries)
	}
}

func TestRenameForkRejectsMismatchedOwnedCopy(t *testing.T) {
	m := &backup.Materialised{Name: "target", Source: "app", Port: 5480, SourcePort: 5432, Data: "/var/lib/postgresql/18/target"}

	for _, tc := range []struct {
		name   string
		source string
		target string
	}{
		{name: "source", source: "other", target: "target"},
		{name: "destination", source: "app", target: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := renameFork(context.Background(), m, tc.source, tc.target, rolePasswords("target"), 5433); err == nil {
				t.Fatal("a rename that does not match the owned copy was accepted")
			}
		})
	}
}

func TestForkRenameRefusesDestinationPresent(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.databases = append(fake.databases, &fakeForkDatabase{oid: "99", name: "target", allow: true})

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("destination present = %v", err)
	}

	if len(fake.executed) != 0 {
		t.Fatalf("a refused rename executed: %v", fake.executed)
	}
}

func TestForkRenameRefusesMissingSource(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.databases = nil

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing source = %v", err)
	}

	if len(fake.executed) != 0 {
		t.Fatalf("a refused rename executed: %v", fake.executed)
	}
}

func TestForkRenameRefusesEndpointMismatch(t *testing.T) {
	cases := []struct {
		name    string
		port    string
		dataDir string
	}{
		{name: "default port", port: "5432", dataDir: "/var/lib/postgresql/18/target"},
		{name: "destination port", port: "5433", dataDir: "/var/lib/postgresql/18/target"},
		{name: "wrong data directory", port: "5480", dataDir: "/var/lib/postgresql/18/app"},
		{name: "malformed port", port: "port", dataDir: "/var/lib/postgresql/18/target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeForkRenameSQL()
			fake.port, fake.dataDir = tc.port, tc.dataDir

			err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
			if err == nil {
				t.Fatal("an unattested endpoint was accepted")
			}

			if len(fake.executed) != 0 {
				t.Fatalf("an unattested endpoint was mutated: %v", fake.executed)
			}
		})
	}
}

func TestNewForkRenameCopyRejectsUnsafePortsAndPaths(t *testing.T) {
	target := "/var/lib/postgresql/18/target"

	cases := []struct {
		name string
		m    *backup.Materialised
		live int
	}{
		{name: "nil", m: nil, live: 5433},
		{name: "zero port", m: &backup.Materialised{Port: 0, Data: target}, live: 5433},
		{name: "default port", m: &backup.Materialised{Port: 5432, Data: target}, live: 5433},
		{name: "source port", m: &backup.Materialised{Port: 5433, Data: target}, live: 5434},
		{name: "destination port", m: &backup.Materialised{Port: 5480, SourcePort: 5432, Data: target}, live: 5480},
		{name: "source equals temporary port", m: &backup.Materialised{Port: 5480, SourcePort: 5480, Data: target}, live: 5433},
		{name: "missing live port", m: &backup.Materialised{Port: 5480, SourcePort: 5432, Data: target}, live: 0},
		{name: "relative data directory", m: &backup.Materialised{Port: 5480, SourcePort: 5432, Data: "18/target"}, live: 5433},
		{name: "empty data directory", m: &backup.Materialised{Port: 5480, SourcePort: 5432}, live: 5433},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newForkRenameCopy(tc.m, tc.live); err == nil {
				t.Fatal("an unsafe copy endpoint was accepted")
			}
		})
	}

	got, err := newForkRenameCopy(&backup.Materialised{Port: 5480, SourcePort: 5432, Data: target}, 5433)
	if err != nil {
		t.Fatal(err)
	}

	local, ok := got.local.(pg.Local)
	if !ok || local.Port != 5480 || local.Host == "" || got.port != 5480 || got.dataDir != target {
		t.Fatalf("owned endpoint = %+v, local = %+v", got, local)
	}
}

func TestForkRenamePreparedXactSQLJoinsCatalogByCapturedOID(t *testing.T) {
	fake := newFakeForkRenameSQL()

	target := fakeForkRenameTarget(fake)
	if err := target.refusePrepared(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}

	const want = "SELECT count(*)::text AS n FROM pg_prepared_xacts p JOIN pg_database d ON d.datname = p.database WHERE d.oid = 42"
	if len(fake.queries) != 1 || fake.queries[0] != want {
		t.Fatalf("prepared query = %q, want %q", fake.queries, want)
	}
}

func TestForkRenameRefusesPreparedTransactionBeforeGate(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.addPrepared("42")

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	if err == nil || !strings.Contains(err.Error(), "prepared transactions") {
		t.Fatalf("prepared transaction = %v", err)
	}

	if len(fake.executed) != 0 {
		t.Fatalf("a prepared transaction was renamed around: %v", fake.executed)
	}
}

func TestForkRenamePreparesTransactionDuringDrainCompensates(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.onGate = func(f *fakeForkRenameSQL) { f.addPrepared("42") }

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	if err == nil || !strings.Contains(err.Error(), "prepared transactions") {
		t.Fatalf("prepared transaction during drain = %v", err)
	}

	database := fake.dbByName("app")
	if database == nil || !database.allow {
		t.Fatalf("compensation did not restore the original flag: %+v", database)
	}
}

func TestForkRenameDrainBoundsReconnectingScheduler(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.reconnect = true
	fake.addSession("42", "pg_cron scheduler")

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	if err == nil || !strings.Contains(err.Error(), "unresolved session") {
		t.Fatalf("reconnecting scheduler = %v", err)
	}

	if got := strings.Count(strings.Join(fake.queries, "\n"), "pg_terminate_backend(pid, 2000)"); got != forkRenameDrainRounds {
		t.Fatalf("termination rounds = %d, want the bound %d", got, forkRenameDrainRounds)
	}

	if fake.indexOfBeginTransaction() >= 0 {
		t.Fatal("the rename ran with unresolved sessions")
	}

	database := fake.dbByName("app")
	if database == nil || !database.allow {
		t.Fatalf("compensation did not restore the original flag: %+v", database)
	}
}

func TestForkRenameFailureRestoresOriginalAllowConnections(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.failRename = errFakeForkRename

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
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

func TestForkRenameCancellationCompensatesWithDetachedContext(t *testing.T) {
	fake := newFakeForkRenameSQL()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake.cancelOnRename = cancel

	err := renameForkOnCopy(ctx, fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
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
}

func TestForkRenameAmbiguousCommitInspectsCapturedOID(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.ambiguousRename = true

	err := renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	if !errors.Is(err, errFakeForkAmbiguousCommit) {
		t.Fatalf("ambiguous commit = %v", err)
	}

	database := fake.dbByName("target")
	if database == nil || database.oid != "42" || !database.allow {
		t.Fatalf("ambiguous commit did not settle the same OID: %+v", database)
	}

	if fake.indexOfExec(`ALTER DATABASE "target" ALLOW_CONNECTIONS true;`) >= 0 {
		t.Fatalf("compensation blindly changed the current name: %v", fake.executed)
	}

	if fake.indexOfQuery("FROM pg_database WHERE oid = 42") < 0 {
		t.Fatalf("compensation did not inspect the captured OID: %v", fake.queries)
	}
}

func TestRestoreForkRenameAllowConnectionsUsesCapturedOIDName(t *testing.T) {
	fake := newFakeForkRenameSQL()
	fake.dbByName("app").name = "target"
	fake.dbByName("target").allow = false

	target := fakeForkRenameTarget(fake)
	if err := restoreForkRenameAllowConnections(context.Background(), target, "42", true); err != nil {
		t.Fatal(err)
	}

	database := fake.dbByName("target")
	if database == nil || !database.allow {
		t.Fatalf("current OID name was not restored: %+v", database)
	}

	if fake.indexOfExec(`ALTER DATABASE "target" ALLOW_CONNECTIONS true;`) < 0 {
		t.Fatalf("compensation did not use the current OID name: %v", fake.executed)
	}

	if fake.indexOfExec(`ALTER DATABASE "app" ALLOW_CONNECTIONS true;`) >= 0 {
		t.Fatalf("compensation guessed the old name: %v", fake.executed)
	}

	fake.databases = nil
	before := len(fake.executed)

	if err := restoreForkRenameAllowConnections(context.Background(), target, "42", true); err == nil {
		t.Fatal("compensation guessed a missing OID")
	}

	if len(fake.executed) != before {
		t.Fatal("compensation modified a missing OID")
	}
}

type rejectingForkOwnership struct{}

func (rejectingForkOwnership) CreateData(context.Context, backup.MaterialisationPaths) error {
	return nil
}

func (rejectingForkOwnership) CreateConfig(context.Context, backup.MaterialisationPaths) error {
	return nil
}

func (rejectingForkOwnership) Guard(context.Context, backup.MaterialisationPaths, backup.MaterialisationAction, func() error) error {
	return errFakeForkOwnership
}

func TestForkCronSettlementOrdersDeactivateRenameRetarget(t *testing.T) {
	s := newOwnedForkFixture(t, "success")
	s.stubJobRuntime(t)
	s.stubForkHooks(t)
	s.cfg.Clusters[0].Extensions = []string{"pg_cron"}

	var order []string

	oldDeactivate, oldRetarget := deactivateCopyCron, retargetCopyCron

	t.Cleanup(func() { deactivateCopyCron, retargetCopyCron = oldDeactivate, oldRetarget })

	deactivateCopyCron = func(context.Context, sqlRunner) (cronSettlement, error) {
		order = append(order, "deactivate")
		return cronSettlement{MetadataDB: "source", Installed: true}, nil
	}
	retargetCopyCron = func(context.Context, sqlRunner, cronSettlement, string, string) (int, error) {
		order = append(order, "retarget")
		return 0, nil
	}
	forkRename = func(context.Context, *backup.Materialised, string, string, map[string]string, int) error {
		order = append(order, "rename")
		return nil
	}
	_, skipped, err := RunJob(context.Background(), "db-fork", JobOptions{ID: s.owner.Job}, nil)
	rejectf(t, skipped != "", "job skipped: %s", skipped)

	if err != nil {
		t.Fatal(err)
	}

	want := []string{"deactivate", "rename", "retarget"}
	if len(order) != len(want) {
		t.Fatalf("cron settlement order = %v, want %v", order, want)
	}

	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cron settlement order = %v, want %v", order, want)
		}
	}
}

func TestForkRenameRunsOnlyInsideAcceptedOwnership(t *testing.T) {
	fake := newFakeForkRenameSQL()
	m := &backup.Materialised{
		Name: "target", Source: "app", Port: 5480,
		Data: filepath.Join(t.TempDir(), "target"), Ownership: rejectingForkOwnership{},
	}
	called := false

	err := m.GuardOwnership(context.Background(), backup.MaterialisationRename, func() error {
		called = true
		return renameForkOnCopy(context.Background(), fakeForkRenameTarget(fake), "app", "target", rolePasswords("target"))
	})
	if !errors.Is(err, errFakeForkOwnership) || called {
		t.Fatalf("ownership rejection: called=%v err=%v", called, err)
	}

	if len(fake.executed) != 0 || len(fake.queries) != 0 {
		t.Fatalf("rejected ownership still reached SQL: exec=%v query=%v", fake.executed, fake.queries)
	}
}
