package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/totp"
)

// testClock is a settable clock for the guard.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func newTestGuard(t *testing.T) (protectGuard, *testClock) {
	t.Helper()
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	return protectGuard{path: filepath.Join(t.TempDir(), "protect.json"), now: clock.now}, clock
}

func errReason(t *testing.T, err error) (string, string) {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("got %v, want a CLIError", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return cliErr.Code, reason
}

func wantReason(t *testing.T, err error, code, reason string) {
	t.Helper()
	gotCode, gotReason := errReason(t, err)
	if gotCode != code || gotReason != reason {
		t.Fatalf("got %s/%s (%v), want %s/%s", gotCode, gotReason, err, code, reason)
	}
}

// codeFor returns the current code for secret at the guard's time plus
// offset steps.
func codeFor(t *testing.T, secret string, at time.Time, offset int64) string {
	t.Helper()
	key, err := totp.DecodeSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	return totp.HOTP(key, totp.Step(at)+offset, totp.Digits)
}

func codeEnv(params, code string) *Env {
	req := rpc.Request{Params: json.RawMessage(params)}
	if code != "" {
		req.Secrets = map[string]string{ProtectCodeSecret: code}
	}
	return &Env{Req: req}
}

// setUp runs protect.init and protect.confirm and returns the secret.
func setUp(t *testing.T, g protectGuard, clock *testClock) string {
	t.Helper()
	res, err := g.init(codeEnv(`{"label":"prod"}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	secret := res.Data.(map[string]any)["secret"].(string)
	if _, err := g.confirm(codeEnv("", codeFor(t, secret, clock.t, 0))); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return secret
}

func TestProtectInitConfirm(t *testing.T) {
	g, clock := newTestGuard(t)
	if ok, _ := g.configured(); ok {
		t.Fatal("configured before init")
	}
	if _, err := g.confirm(codeEnv("", "123456")); err == nil {
		t.Fatal("confirm without init should fail")
	} else {
		wantReason(t, err, output.CodePrecondition, "protect_not_pending")
	}
	res, err := g.init(codeEnv(`{"label":"prod"}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(map[string]any)
	secret := data["secret"].(string)
	if uri := data["uri"].(string); uri != totp.URI("eru", "prod", secret) {
		t.Fatalf("uri %s", uri)
	}
	if ok, _ := g.configured(); ok {
		t.Fatal("a pending secret must not count as configured")
	}
	wrong := codeFor(t, secret, clock.t, 5)
	if _, err := g.confirm(codeEnv("", wrong)); err == nil {
		t.Fatal("wrong code confirmed")
	} else {
		wantReason(t, err, output.CodeProtectCodeRejected, "code_invalid")
	}
	if _, err := g.confirm(codeEnv("", codeFor(t, secret, clock.t, 0))); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if ok, _ := g.configured(); !ok {
		t.Fatal("not configured after confirm")
	}
	info, err := os.Stat(g.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode %o, want 600", perm)
	}
	st, _ := g.load()
	if st.Failures != 0 || st.Pending != "" || st.Secret != secret {
		t.Fatalf("state after confirm: %+v", st)
	}

	// A second init needs --rotate.
	_, err = g.init(codeEnv(`{}`, ""))
	wantReason(t, err, output.CodePrecondition, "protect_already_initialized")
}

func TestProtectRotate(t *testing.T) {
	g, clock := newTestGuard(t)
	if _, err := g.init(codeEnv(`{"rotate":true}`, "")); err == nil {
		t.Fatal("rotate before init should fail")
	}
	old := setUp(t, g, clock)
	clock.t = clock.t.Add(totp.Period)

	_, err := g.init(codeEnv(`{"rotate":true}`, ""))
	wantReason(t, err, output.CodeProtectCodeRejected, "code_required")

	res, err := g.init(codeEnv(`{"rotate":true}`, codeFor(t, old, clock.t, 0)))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	fresh := res.Data.(map[string]any)["secret"].(string)
	// Until the new secret is confirmed, the old one still lifts a
	// protection.
	if st, _ := g.load(); st.Secret != old || st.Pending != fresh {
		t.Fatalf("state during rotate: %+v", st)
	}
	// The new secret's code in the same step as the old one's last code is
	// accepted: the pending secret has its own replay history.
	if _, err := g.confirm(codeEnv("", codeFor(t, fresh, clock.t, 0))); err != nil {
		t.Fatalf("confirm rotate: %v", err)
	}
	if st, _ := g.load(); st.Secret != fresh || st.Pending != "" {
		t.Fatalf("state after rotate: %+v", st)
	}
}

func liftedClusters() (config.Cluster, config.Cluster) {
	old := protectedCluster()
	lifted := protectedCluster()
	lifted.Clusters[0].Protected = false
	return old, lifted
}

func TestCheckUnprotectTOTP(t *testing.T) {
	g, clock := newTestGuard(t)
	old, lifted := liftedClusters()
	if got := Unprotected(old, lifted); len(got) != 1 || got[0] != "app" {
		t.Fatalf("Unprotected = %v", got)
	}

	// Without a TOTP secret, nothing lifts it.
	wantReason(t, g.checkUnprotect(old, lifted, "123456"), output.CodeDatabaseProtected, "protect_not_initialized")

	secret := setUp(t, g, clock)
	clock.t = clock.t.Add(totp.Period)

	// Unchanged protection needs no code.
	if err := g.checkUnprotect(old, old, ""); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
	wantReason(t, g.checkUnprotect(old, lifted, ""), output.CodeProtectCodeRejected, "code_required")
	// The old <db>/unprotect word is no code.
	wantReason(t, g.checkUnprotect(old, lifted, "app/unprotect"), output.CodeProtectCodeRejected, "code_invalid")

	code := codeFor(t, secret, clock.t, 0)
	if err := g.checkUnprotect(old, lifted, code); err != nil {
		t.Fatalf("valid code: %v", err)
	}
	// The same code is refused the second time.
	wantReason(t, g.checkUnprotect(old, lifted, code), output.CodeProtectCodeRejected, "code_reused")

	// Removing a protected entry needs a code too; the next step's works.
	removed := config.Cluster{Clusters: old.Clusters[1:]}
	clock.t = clock.t.Add(totp.Period)
	wantReason(t, g.checkUnprotect(old, removed, ""), output.CodeProtectCodeRejected, "code_required")
	if err := g.checkUnprotect(old, removed, codeFor(t, secret, clock.t, 0)); err != nil {
		t.Fatalf("removed entry with code: %v", err)
	}
}

func TestCheckUnprotectSkew(t *testing.T) {
	g, clock := newTestGuard(t)
	old, lifted := liftedClusters()
	secret := setUp(t, g, clock)
	clock.t = clock.t.Add(10 * totp.Period)
	if err := g.checkUnprotect(old, lifted, codeFor(t, secret, clock.t, 2)); err == nil {
		t.Fatal("a code two steps ahead was accepted")
	}
	if err := g.checkUnprotect(old, lifted, codeFor(t, secret, clock.t, -1)); err != nil {
		t.Fatalf("one step behind: %v", err)
	}
	if err := g.checkUnprotect(old, lifted, codeFor(t, secret, clock.t, 1)); err != nil {
		t.Fatalf("one step ahead: %v", err)
	}
}

func TestProtectLockout(t *testing.T) {
	g, clock := newTestGuard(t)
	old, lifted := liftedClusters()
	secret := setUp(t, g, clock)
	clock.t = clock.t.Add(totp.Period)
	wrong := codeFor(t, secret, clock.t, 10)
	for i := 1; i < protectMaxFailures; i++ {
		wantReason(t, g.checkUnprotect(old, lifted, wrong), output.CodeProtectCodeRejected, "code_invalid")
	}
	wantReason(t, g.checkUnprotect(old, lifted, wrong), output.CodeProtectCodeRejected, "locked")

	// While locked, even the right code is refused, and the lockout
	// survives a reload: it is on disk.
	wantReason(t, g.checkUnprotect(old, lifted, codeFor(t, secret, clock.t, 0)), output.CodeProtectCodeRejected, "locked")
	res, err := g.status(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if st := res.Data.(protectStatus); st.LockedUntil == "" || !st.Configured {
		t.Fatalf("status while locked: %+v", st)
	}

	clock.t = clock.t.Add(protectLockout)
	if err := g.checkUnprotect(old, lifted, codeFor(t, secret, clock.t, 0)); err != nil {
		t.Fatalf("after the lockout: %v", err)
	}
	if st, _ := g.load(); st.Failures != 0 {
		t.Fatalf("failures after success: %d", st.Failures)
	}
	res, _ = g.status(filepath.Join(t.TempDir(), "missing.toml"))
	if st := res.Data.(protectStatus); st.LockedUntil != "" {
		t.Fatalf("status after the lockout: %+v", st)
	}
}

func TestCheckProtect(t *testing.T) {
	g, clock := newTestGuard(t)
	before := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Size: "medium"}}}
	after := protectedCluster()
	names := newlyProtected(before, after)
	if len(names) != 1 || names[0] != "app" {
		t.Fatalf("newlyProtected = %v", names)
	}
	if got := newlyProtected(after, after); len(got) != 0 {
		t.Fatalf("unchanged: %v", got)
	}
	if err := g.checkProtect(nil); err != nil {
		t.Fatalf("nothing protected: %v", err)
	}
	wantReason(t, g.checkProtect(names), output.CodePrecondition, "protect_not_initialized")
	setUp(t, g, clock)
	if err := g.checkProtect(names); err != nil {
		t.Fatalf("after setup: %v", err)
	}
}
