package node

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/totp"
)

// TestWriteAccessLoginPath follows a write login to a protected database
// the way it runs live. The CLI makes a password and sends it as a request
// secret; the node registers every request secret for redaction (Serve),
// creates the role and writes the result through its event writer, which
// redacts it; the CLI decodes the result and logs in through the session
// pooler, whose auth_query runs the session auth function in the database
// the <db>__auth alias opens. The function must hand out the verifier the
// role was created with while the role is valid and nothing once it
// expired, and that verifier must accept the password the CLI logs in with.
//
// Live, the node made the password, the event writer redacted it from the
// result, and the CLI logged in with the redaction placeholder: PgBouncer
// refused it with "password authentication failed".
func TestWriteAccessLoginPath(t *testing.T) {
	g, clock := newTestGuard(t)
	secret := setUp(t, g, clock)
	clock.t = clock.t.Add(totp.Period)

	// The CLI's side (sessionLogin).
	password, err := secrets.Base62(32)
	if err != nil {
		t.Fatal(err)
	}
	env := writeEnv(`{"db":"app"}`, codeFor(t, secret, clock.t, 0), false)
	env.Req.Secrets[WritePasswordSecret] = password
	// The node's side: Serve registers the request's secrets first.
	for _, v := range env.Req.Secrets {
		secrets.Add(v)
	}
	var created string
	grant := func(_ context.Context, spec config.ClusterSpec, role writeRole) error {
		verifier, err := pg.SCRAMVerifier(role.Password)
		created = writeRoleSQL(spec, role, verifier)
		return err
	}
	res, err := g.writeAccess(context.Background(), env, grant)
	if err != nil {
		t.Fatal(err)
	}

	// The result as the CLI reads it off the node's stdout.
	var stdout bytes.Buffer
	out, _ := newTestWriter(t, &stdout)
	out.ok("db.write-access", res)
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := lines[len(lines)-1]
	var ev struct {
		Type     string `json:"type"`
		Envelope struct {
			OK   bool            `json:"ok"`
			Data json.RawMessage `json:"data"`
		} `json:"envelope"`
	}
	if err := json.Unmarshal([]byte(last), &ev); err != nil || ev.Type != "result" || !ev.Envelope.OK {
		t.Fatalf("result line %s: %v", last, err)
	}
	var access WriteAccess
	if err := json.Unmarshal(ev.Envelope.Data, &access); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(last, secrets.Placeholder) {
		t.Fatalf("the result the CLI depends on was redacted: %s", last)
	}
	if !access.Protected || !strings.HasPrefix(access.User, writeRolePrefix) {
		t.Fatalf("access %+v", access)
	}

	// The role as the node created it.
	m := regexp.MustCompile(`CREATE ROLE "([^"]+)" LOGIN NOINHERIT PASSWORD '([^']+)' VALID UNTIL '([^']+)' IN ROLE "app_owner";`).FindStringSubmatch(created)
	if m == nil {
		t.Fatalf("no CREATE ROLE in\n%s", created)
	}
	role, verifier := m[1], m[2]
	if role != access.User {
		t.Fatalf("result names %s, the node created %s", access.User, role)
	}
	// VALID UNTIL carries its zone, so the server's TimeZone cannot move it.
	validUntil, err := time.Parse(time.RFC3339, m[3])
	if err != nil || !strings.HasSuffix(m[3], "Z") || !validUntil.Equal(clock.t.Add(writeRoleLogin).Truncate(time.Second)) {
		t.Fatalf("VALID UNTIL %q (%v), want %s", m[3], err, clock.t.Add(writeRoleLogin))
	}
	if !scramAccepts(t, verifier, password) {
		t.Fatal("the role's verifier does not accept the password the CLI logs in with")
	}
	if scramAccepts(t, verifier, secrets.Placeholder) {
		t.Fatal("the role's verifier accepts the redaction placeholder")
	}

	// The session pooler: the app entry's auth_query runs on app__auth,
	// which must open the database the auth functions and roles are made in.
	ini := string(render.SessionIni(config.Cluster{}, []render.PoolerDatabase{{Name: "app", Port: 5433, MaxConnections: 10}}, "/run/vaire-pgbouncer-session"))
	alias := iniValue(t, ini, render.AuthAlias("app"))
	if !strings.Contains(" "+alias+" ", " dbname="+steps.AuthDB+" ") {
		t.Fatalf("%s = %s does not open %s", render.AuthAlias("app"), alias, steps.AuthDB)
	}
	if !strings.Contains(iniValue(t, ini, "app"), "auth_dbname="+render.AuthAlias("app")) {
		t.Fatalf("app does not run its auth_query on %s:\n%s", render.AuthAlias("app"), ini)
	}
	if user := iniValue(t, ini, "auth_user"); user != "pgbouncer_auth" {
		t.Fatalf("auth_user = %s", user)
	}
	fn := regexp.MustCompile(`FROM vaire\.(\w+)\(\$1\)`).FindStringSubmatch(iniValue(t, ini, "auth_query"))
	if fn == nil {
		t.Fatalf("auth_query in\n%s", ini)
	}
	authSQL := steps.AuthFunctionsSQL()
	if !strings.Contains(authSQL, "GRANT EXECUTE ON FUNCTION vaire."+fn[1]+"(text) TO pgbouncer_auth;") {
		t.Fatalf("pgbouncer_auth may not run vaire.%s", fn[1])
	}
	where := authWhere(t, authSQL, fn[1])

	// A fresh write role: a member of app_owner only, never a superuser.
	row := shadowRow{usename: role, valuntil: validUntil, memberOf: []string{"app_owner"}}
	for _, at := range []time.Duration{0, time.Minute, writeRoleLogin - time.Second} {
		if !evalAuthWhere(t, where, row, role, clock.t.Add(at)) {
			t.Fatalf("vaire.%s returns nothing for %s %s after it was made", fn[1], role, at)
		}
	}
	// Expired: the function returns nothing, and the pooler refuses it.
	for _, at := range []time.Duration{writeRoleLogin, writeRoleLogin + time.Second, time.Hour} {
		if evalAuthWhere(t, where, row, role, clock.t.Add(at)) {
			t.Fatalf("vaire.%s returns %s %s after it was made, past its VALID UNTIL", fn[1], role, at)
		}
	}
	// Another user's name never returns the write role.
	if evalAuthWhere(t, where, row, "vaire_admin", clock.t) {
		t.Fatal("the auth function matched another name")
	}
}

// scramAccepts reports whether a SCRAM-SHA-256 verifier accepts password:
// PgBouncer, holding the verifier from auth_query, checks the client's
// proof against its StoredKey.
func scramAccepts(t *testing.T, verifier, password string) bool {
	t.Helper()
	m := regexp.MustCompile(`^SCRAM-SHA-256\$(\d+):([^$]+)\$([^:]+):(.+)$`).FindStringSubmatch(verifier)
	if m == nil {
		t.Fatalf("verifier %q", verifier)
	}
	iterations, _ := strconv.Atoi(m[1])
	salt, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil {
		t.Fatal(err)
	}
	stored, err := base64.StdEncoding.DecodeString(m[3])
	if err != nil {
		t.Fatal(err)
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, salted)
	mac.Write([]byte("Client Key"))
	got := sha256.Sum256(mac.Sum(nil))
	return hmac.Equal(got[:], stored)
}

// iniValue is the value of key's first line in ini.
func iniValue(t *testing.T, ini, key string) string {
	t.Helper()
	for _, line := range strings.Split(ini, "\n") {
		if k, v, ok := strings.Cut(line, " = "); ok && k == key {
			return v
		}
	}
	t.Fatalf("no %s in\n%s", key, ini)
	return ""
}

// authWhere is the WHERE clauses of auth function fn in sql.
func authWhere(t *testing.T, sql, fn string) []string {
	t.Helper()
	_, body, ok := strings.Cut(sql, "CREATE OR REPLACE FUNCTION vaire."+fn+"(p_usename text)")
	if !ok {
		t.Fatalf("vaire.%s is not created in\n%s", fn, sql)
	}
	body, _, _ = strings.Cut(body, "$vaire$;")
	if !strings.Contains(body, "FROM pg_shadow") {
		t.Fatalf("vaire.%s does not read pg_shadow:\n%s", fn, body)
	}
	_, where, ok := strings.Cut(body, "WHERE ")
	if !ok {
		t.Fatalf("vaire.%s has no WHERE", fn)
	}
	var kept []string
	for _, line := range strings.Split(where, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "--") {
			kept = append(kept, l)
		}
	}
	return strings.Split(strings.Join(kept, " "), " AND ")
}

// shadowRow is the part of a pg_shadow row the auth functions read.
type shadowRow struct {
	usename  string
	super    bool
	valuntil time.Time
	memberOf []string
}

var (
	notInRe   = regexp.MustCompile(`^usename NOT IN \(((?:'[^']*',?)+)\)$`)
	hasRoleRe = regexp.MustCompile(`^NOT pg_has_role\(usename, '([^']+)', 'MEMBER'\)$`)
)

// evalAuthWhere evaluates the auth function's WHERE clauses for row, called
// with name at now. A clause it does not know fails the test, so a change
// to the function has to be taught here.
func evalAuthWhere(t *testing.T, where []string, row shadowRow, name string, now time.Time) bool {
	t.Helper()
	match := true
	for _, clause := range where {
		var ok bool
		switch {
		case clause == "usename = p_usename":
			ok = row.usename == name
		case clause == "NOT usesuper":
			ok = !row.super
		case clause == "(valuntil IS NULL OR valuntil > now())":
			// timestamptz against timestamptz: no zone is involved.
			ok = row.valuntil.IsZero() || row.valuntil.After(now)
		case notInRe.MatchString(clause):
			list := notInRe.FindStringSubmatch(clause)[1]
			ok = !slices.Contains(strings.Split(strings.ReplaceAll(list, "'", ""), ","), row.usename)
		case hasRoleRe.MatchString(clause):
			ok = !slices.Contains(row.memberOf, hasRoleRe.FindStringSubmatch(clause)[1])
		default:
			t.Fatalf("auth function clause %q is not known to this test", clause)
		}
		match = match && ok
	}
	return match
}
