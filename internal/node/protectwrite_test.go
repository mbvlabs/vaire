package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/totp"
)

// writeEnv is a request on protectedCluster with params and, when code is
// set, the protect_code secret.
func writeEnv(params, code string, dryRun bool) *Env {
	env := codeEnv(params, code)
	env.Cluster = protectedCluster()
	env.Req.DryRun = dryRun
	return env
}

func TestRequireWriteCodeSQL(t *testing.T) {
	g, clock := newTestGuard(t)
	called := false
	h := g.requireWriteCode("writing to it", "db", sqlWrites, func(context.Context, *Env) (Result, error) {
		called = true
		return Result{}, nil
	})
	run := func(params, code string) error {
		called = false
		_, err := h(context.Background(), writeEnv(params, code, false))
		return err
	}

	// Before protect init, a write to a protected database is refused and
	// a read runs.
	err := run(`{"db":"app","write":true}`, "")
	wantReason(t, err, output.CodeDatabaseProtected, "protect_not_initialized")
	if called {
		t.Fatal("handler ran without protection set up")
	}

	secret := setUp(t, g, clock)
	clock.t = clock.t.Add(totp.Period)

	// Reads never need a code, on a protected database or not.
	for _, params := range []string{`{"db":"app","query":"select 1"}`, `{"db":"app","write":false}`, `{"db":"other","query":"select 1"}`} {
		if err := run(params, ""); err != nil || !called {
			t.Fatalf("read %s: err %v, called %v", params, err, called)
		}
	}
	// A write to an unprotected database needs none either.
	if err := run(`{"db":"other","write":true}`, ""); err != nil || !called {
		t.Fatalf("write to other: err %v, called %v", err, called)
	}

	// A write to a protected one does.
	err = run(`{"db":"app","write":true}`, "")
	wantReason(t, err, output.CodeDatabaseProtected, "write_requires_human")
	if called {
		t.Fatal("handler ran without a code")
	}
	err = run(`{"db":"app","write":true}`, codeFor(t, secret, clock.t, 7))
	wantReason(t, err, output.CodeProtectCodeRejected, "code_invalid")
	if called {
		t.Fatal("handler ran with a wrong code")
	}
	code := codeFor(t, secret, clock.t, 0)
	if err := run(`{"db":"app","write":true}`, code); err != nil || !called {
		t.Fatalf("write with code: err %v, called %v", err, called)
	}
	// One code is one write: the same code again is refused.
	err = run(`{"db":"app","write":true}`, code)
	wantReason(t, err, output.CodeProtectCodeRejected, "code_reused")
	if called {
		t.Fatal("handler ran with a reused code")
	}
}

func TestRequireWriteCodeMaintKill(t *testing.T) {
	g, clock := newTestGuard(t)
	secret := setUp(t, g, clock)
	clock.t = clock.t.Add(totp.Period)
	called := false
	h := g.requireWriteCode("terminating its sessions", "db", nil, func(context.Context, *Env) (Result, error) {
		called = true
		return Result{}, nil
	})
	run := func(params, code string, dryRun bool) error {
		called = false
		_, err := h(context.Background(), writeEnv(params, code, dryRun))
		return err
	}
	// A dry run terminates nothing, so it needs no code.
	if err := run(`{"db":"app","idle_in_tx":true}`, "", true); err != nil || !called {
		t.Fatalf("dry run: err %v, called %v", err, called)
	}
	err := run(`{"db":"app","idle_in_tx":true}`, "", false)
	wantReason(t, err, output.CodeDatabaseProtected, "write_requires_human")
	if called || !strings.Contains(err.Error(), "terminating its sessions") {
		t.Fatalf("kill without code: %v, called %v", err, called)
	}
	if err := run(`{"db":"other","idle_in_tx":true}`, "", false); err != nil || !called {
		t.Fatalf("kill on other: err %v, called %v", err, called)
	}
	if err := run(`{"db":"app","idle_in_tx":true}`, codeFor(t, secret, clock.t, 0), false); err != nil || !called {
		t.Fatalf("kill with code: err %v, called %v", err, called)
	}
}

func TestWriteAccess(t *testing.T) {
	g, clock := newTestGuard(t)
	var granted []writeRole
	grant := func(_ context.Context, spec config.ClusterSpec, role writeRole) error {
		if spec.Name != "app" {
			t.Fatalf("grant on %s", spec.Name)
		}
		granted = append(granted, role)
		return nil
	}
	password := strings.Repeat("p", writeRolePasswordMin)
	accessWith := func(params, code, password string) (WriteAccess, error) {
		env := writeEnv(params, code, false)
		if password != "" {
			if env.Req.Secrets == nil {
				env.Req.Secrets = map[string]string{}
			}
			env.Req.Secrets[WritePasswordSecret] = password
		}
		res, err := g.writeAccess(context.Background(), env, grant)
		if err != nil {
			return WriteAccess{}, err
		}
		return res.Data.(WriteAccess), nil
	}
	access := func(params, code string) (WriteAccess, error) { return accessWith(params, code, password) }

	// Not protected, and no copy the server holds open (this test node has
	// no records.json): the CLI writes as vaire_admin, and no role is made.
	for _, db := range []string{"other", "app__restoring"} {
		got, err := access(`{"db":"`+db+`"}`, "")
		if err != nil || got.Protected || len(granted) != 0 {
			t.Fatalf("%s: %+v %v, granted %d", db, got, err, len(granted))
		}
	}
	_, err := access(`{}`, "")
	if code, _ := errReason(t, err); code != output.CodeInputRequired {
		t.Fatalf("no db: %v", err)
	}

	_, err = access(`{"db":"app"}`, "123456")
	wantReason(t, err, output.CodeDatabaseProtected, "protect_not_initialized")

	secret := setUp(t, g, clock)
	clock.t = clock.t.Add(totp.Period)

	_, err = access(`{"db":"app"}`, "")
	wantReason(t, err, output.CodeDatabaseProtected, "write_requires_human")
	_, err = access(`{"db":"app"}`, codeFor(t, secret, clock.t, 9))
	wantReason(t, err, output.CodeProtectCodeRejected, "code_invalid")
	if len(granted) != 0 {
		t.Fatalf("a role was made without a valid code: %+v", granted)
	}

	// A valid code without the CLI's password (or a short one) makes no
	// role.
	for _, pw := range []string{"", "short"} {
		_, err = accessWith(`{"db":"app"}`, codeFor(t, secret, clock.t, 0), pw)
		if code, _ := errReason(t, err); code != output.CodeInputRequired || len(granted) != 0 {
			t.Fatalf("password %q: %v, granted %d", pw, err, len(granted))
		}
		clock.t = clock.t.Add(totp.Period)
	}

	code := codeFor(t, secret, clock.t, 0)
	got, err := access(`{"db":"app"}`, code)
	if err != nil {
		t.Fatal(err)
	}
	// The role logs in with the password the CLI sent.
	if !got.Protected || len(granted) != 1 || got.User != granted[0].User || granted[0].Password != password {
		t.Fatalf("access %+v, granted %+v", got, granted)
	}
	if !strings.HasPrefix(got.User, writeRolePrefix) {
		t.Fatalf("role %q", got.User)
	}
	until, err := time.Parse(time.RFC3339, got.ValidUntil)
	if err != nil || !until.Equal(clock.t.Add(writeRoleLogin).Truncate(time.Second)) {
		t.Fatalf("valid until %s (%v), want %s", got.ValidUntil, err, clock.t.Add(writeRoleLogin))
	}
	// The same code makes no second role.
	_, err = access(`{"db":"app"}`, code)
	wantReason(t, err, output.CodeProtectCodeRejected, "code_reused")
	if len(granted) != 1 {
		t.Fatalf("granted %d roles", len(granted))
	}
}

// A copy the server holds open resolves to the live database's spec, pointed
// at the copy's port, so a write to it follows the live database's
// protection; anything else is not a database of the server's.
func TestWriteSpec(t *testing.T) {
	recs := backup.Records{Restores: map[string]backup.Restore{"app": {Database: "app", Port: 5439}}}
	cfg := protectedCluster()
	spec, ok := writeSpec(cfg, backup.Records{}, "app")
	if !ok || !spec.Protected || spec.Port != 0 {
		t.Fatalf("app: %+v %v", spec, ok)
	}
	spec, ok = writeSpec(cfg, recs, "app__restoring")
	if !ok || !spec.Protected || spec.Name != "app" || spec.Port != 5439 {
		t.Fatalf("app__restoring: %+v %v, want app at 5439", spec, ok)
	}
	for _, name := range []string{"app__restoring", "app__pre_restore_20260101T000000", "other__restoring", "missing"} {
		if spec, ok := writeSpec(cfg, backup.Records{}, name); ok {
			t.Errorf("%s: resolved to %+v, want no cluster", name, spec)
		}
	}
}

// An open copy of a protected database is listed in the gate's protected
// names, so the CLI asks for a code before writing to it.
func TestProtectedCopies(t *testing.T) {
	recs := backup.Records{Restores: map[string]backup.Restore{
		"app":   {Database: "app", Port: 5439},
		"other": {Database: "other", Port: 5440},
		"gone":  {Database: "gone"},
	}}
	got := copiesOf(protectedCluster(), recs)
	if len(got) != 1 || got[0] != "app__restoring" {
		t.Fatalf("copies = %v, want [app__restoring]", got)
	}
}

func TestWriteRoleSQL(t *testing.T) {
	role := writeRole{User: "vaire_write_0a1b2c3d4e5f", Password: "x", ValidUntil: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	sql := writeRoleSQL(config.ClusterSpec{Name: "app"}, role, "SCRAM-SHA-256$v")
	for _, want := range []string{
		`CREATE ROLE "vaire_write_0a1b2c3d4e5f" LOGIN NOINHERIT PASSWORD 'SCRAM-SHA-256$v' VALID UNTIL '2026-10-15T12:00:00Z' IN ROLE "app_owner";`,
		`GRANT CONNECT, TEMPORARY ON DATABASE "app" TO "vaire_write_0a1b2c3d4e5f";`,
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("missing %q in\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "SUPERUSER") || strings.Contains(sql, "CREATEROLE") {
		t.Fatalf("write role has too much:\n%s", sql)
	}
}

// TestWriteMethodsGuarded checks the registry wraps the methods that write
// to a database: with the real guard and no code, a write to a protected
// database is refused before the handler touches PostgreSQL.
func TestWriteMethodsGuarded(t *testing.T) {
	m := methods()
	for _, tc := range []struct{ method, params string }{
		{"sql", `{"db":"app","query":"delete from t","write":true}`},
		{"maint.kill", `{"db":"app","idle_in_tx":true}`},
		{"db.write-access", `{"db":"app"}`},
	} {
		meth, ok := m[tc.method]
		if !ok {
			t.Fatalf("method %s missing", tc.method)
		}
		env := &Env{Cluster: protectedCluster(), Req: rpc.Request{Method: tc.method, Params: json.RawMessage(tc.params), Confirm: "app/kill"}}
		if _, err := meth.handler(context.Background(), env); err == nil {
			t.Fatalf("%s on protected app without a code ran", tc.method)
		}
	}
	if m["db.write-access"].runLog != nil {
		t.Fatal("db.write-access must write no run log: its result carries a password")
	}
}
