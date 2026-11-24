package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/totp"
)

//nolint:gocognit // Exercise isolation and replay over one persistent fixture.
func TestOperatorFactorsIsolationAndReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factors"), now: func() time.Time { return now }}
	keys := make(map[string][]byte)

	for _, id := range []string{"operator-a", "operator-b"} {
		secret, err := totp.NewSecret()
		if err != nil {
			t.Fatal(err)
		}

		keys[id], err = totp.DecodeSecret(secret)
		if err != nil {
			t.Fatal(err)
		}

		err = g.forOperator(ctx, id, func(g exposureFactorGuard, st *exposureFactorState) error {
			if err := g.authorizeSetup(st, "", ""); err != nil {
				return err
			}

			if err := g.setupTOTP(st, secret, totp.Code(keys[id], now)); err != nil {
				return err
			}

			return g.save(*st)
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	now = now.Add(totp.Period)
	check := func(id, code string) error {
		return g.forOperator(ctx, id, func(g exposureFactorGuard, st *exposureFactorState) error {
			return g.verify(st, "", code)
		})
	}
	codeA := totp.Code(keys["operator-a"], now)

	if err := check("operator-b", codeA); err == nil {
		t.Fatal("another operator's code accepted")
	}

	if err := check("operator-a", codeA); err != nil {
		t.Fatal(err)
	}

	if err := check("operator-a", codeA); err == nil {
		t.Fatal("same operator replay accepted")
	}

	if err := check("operator-b", totp.Code(keys["operator-b"], now)); err != nil {
		t.Fatal(err)
	}

	if err := check("", codeA); err == nil {
		t.Fatal("missing identity accepted")
	}

	st, err := g.load()
	if err != nil {
		t.Fatal(err)
	}

	if len(st.Operators) != 2 || st.Operators["operator-a"].LastStep != totp.Step(now) || st.Operators["operator-b"].LastStep != totp.Step(now) {
		t.Fatalf("operator state not persisted independently: %+v", st)
	}
}

func TestOperatorIdentityIsVerifiedAndImmutableIDKeyed(t *testing.T) {
	env := &Env{DeviceIdentity: &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice, OperatorID: "immutable", Operator: "alice", DeviceID: "one"}}
	if got := verifiedOperator(env); got != "immutable" {
		t.Fatal(got)
	}

	env.DeviceIdentity.DeviceID = "two"
	env.DeviceIdentity.Operator = "renamed"

	if got := verifiedOperator(env); got != "immutable" {
		t.Fatal(got)
	}

	env.DeviceIdentity.IdentityKind = "unverified"
	if got := verifiedOperator(env); got != "" {
		t.Fatalf("unverified identity accepted: %q", got)
	}
}

func TestFirstOperatorEnrollmentWithSharedPassword(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: time.Unix(1800000000, 0)}
	installWriteApproval(t, clock)

	const pw = "a long enough approval password"

	password := &Env{Req: rpc.Request{Params: []byte(`{"mode":"password"}`), Secrets: map[string]string{"new_password": pw}}}
	if _, err := handleAuthSetup(ctx, password); err != nil {
		t.Fatal(err)
	}

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	env := &Env{DeviceIdentity: writeIdentity(), Req: rpc.Request{Params: []byte(`{"mode":"totp"}`), Secrets: map[string]string{
		"new_totp_secret": secret, "new_totp_code": "invalid",
	}}}
	if _, err := handleAuthSetup(ctx, env); err == nil {
		t.Fatal("accepted invalid confirmation without shared password")
	}

	env.Req.Secrets["new_totp_code"] = codeFor(t, secret, clock.t, 0)
	if _, err := handleAuthSetup(ctx, env); err != nil {
		t.Fatalf("first enrollment without shared password: %v", err)
	}

	replacement, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	env.Req.Secrets = map[string]string{"new_totp_secret": replacement, "new_totp_code": codeFor(t, replacement, clock.t, 0)}
	if _, err := handleAuthSetup(ctx, env); err == nil {
		t.Fatal("rotated authenticator without existing proof")
	}

	clock.t = clock.t.Add(totp.Period)
	env.Req.Secrets["new_totp_code"] = codeFor(t, replacement, clock.t, 0)

	env.Req.Secrets["exposure_totp"] = codeFor(t, secret, clock.t, 0)
	if _, err := handleAuthSetup(ctx, env); err != nil {
		t.Fatalf("rotation with current authenticator: %v", err)
	}

	secret, err = totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	env.Req.Secrets = map[string]string{"new_totp_secret": secret, "new_totp_code": codeFor(t, secret, clock.t, 0), "exposure_password": pw}
	if _, err := handleAuthSetup(ctx, env); err != nil {
		t.Fatalf("rotation with shared password: %v", err)
	}
}

func TestOperatorStatusAndLegacyState(t *testing.T) {
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factors"), now: time.Now}
	old := defaultExposureFactorGuard
	defaultExposureFactorGuard = g

	t.Cleanup(func() { defaultExposureFactorGuard = old })

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	if err := g.locked(context.Background(), func() error {
		return g.save(exposureFactorState{Secret: secret, Operators: map[string]operatorFactor{"id-a": {Secret: secret}}})
	}); err != nil {
		t.Fatal(err)
	}

	env := &Env{DeviceIdentity: &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice, OperatorID: "id-a", Operator: "alice"}}
	status, err := operatorFactorStatus(env)

	if err != nil || !status.TOTP || status.Operator != "alice" {
		t.Fatalf("own status: %+v, %v", status, err)
	}

	env.DeviceIdentity.OperatorID = "id-b"
	status, err = operatorFactorStatus(env)

	if err != nil || status.TOTP {
		t.Fatalf("other operator or legacy factor visible: %+v, %v", status, err)
	}

	env.DeviceIdentity.IdentityKind = "unverified"
	status, err = operatorFactorStatus(env)

	if err != nil || status.TOTP || status.Operator != "" {
		t.Fatalf("unverified status: %+v, %v", status, err)
	}
}

func TestNonDevicePasswordOnlyApprovalAndSetup(t *testing.T) {
	_, clock := newTestGuard(t)
	installWriteApproval(t, clock)

	ctx := context.Background()

	const password = "a long enough approval password"

	env := &Env{Req: rpc.Request{Params: []byte(`{"mode":"password"}`), Secrets: map[string]string{"new_password": password}}}
	if _, err := handleAuthSetup(ctx, env); err != nil {
		t.Fatalf("initial shared password: %v", err)
	}

	env.Req.Secrets = map[string]string{"exposure_password": password}
	if _, err := handleProtectVerify(ctx, env); err != nil {
		t.Fatalf("non-device password: %v", err)
	}

	secret := setupWriteApprovalWithPassword(t, clock, password)
	clock.t = clock.t.Add(totp.Period)
	env.Req.Secrets = map[string]string{"exposure_totp": codeFor(t, secret, clock.t, 0)}

	if _, err := handleProtectVerify(ctx, env); err == nil {
		t.Fatal("non-device caller used an operator code")
	}

	env.DeviceIdentity = &rpc.WhoamiReply{IdentityKind: "unverified", OperatorID: "op-1"}
	env.Req.Secrets["exposure_password"] = password

	if _, err := handleProtectVerify(ctx, env); err == nil {
		t.Fatal("unverified identity accepted")
	}

	env.DeviceIdentity = nil
	env.Req.Params = []byte(`{"mode":"totp"}`)

	if _, err := handleAuthSetup(ctx, env); err == nil {
		t.Fatal("non-device caller enrolled a TOTP")
	}
}

func setupWriteApprovalWithPassword(t *testing.T, clock *testClock, password string) string {
	t.Helper()

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	env := &Env{DeviceIdentity: writeIdentity(), Req: rpc.Request{
		Params:  []byte(`{"mode":"totp"}`),
		Secrets: map[string]string{"new_totp_secret": secret, "new_totp_code": codeFor(t, secret, clock.t, 0), "exposure_password": password},
	}}
	if _, err := handleAuthSetup(context.Background(), env); err != nil {
		t.Fatal(err)
	}

	return secret
}

func TestSharedPasswordRotationRequiresSharedPassword(t *testing.T) {
	_, clock := newTestGuard(t)
	installWriteApproval(t, clock)

	ctx := context.Background()

	const password = "original shared approval password"

	const replacement = "replacement shared approval password"

	seed := &Env{Req: rpc.Request{Params: []byte(`{"mode":"password"}`), Secrets: map[string]string{"new_password": password}}}
	if _, err := handleAuthSetup(ctx, seed); err != nil {
		t.Fatal(err)
	}

	secret := setupWriteApprovalWithPassword(t, clock, password)
	clock.t = clock.t.Add(totp.Period)

	env := &Env{DeviceIdentity: writeIdentity(), Req: rpc.Request{Params: []byte(`{"mode":"password"}`), Secrets: map[string]string{
		"new_password": replacement, "exposure_totp": codeFor(t, secret, clock.t, 0),
	}}}
	if _, err := handleAuthSetup(ctx, env); err == nil {
		t.Fatal("personal authenticator rotated shared password")
	}

	env.Req.Secrets["exposure_password"] = password
	if _, err := handleAuthSetup(ctx, env); err != nil {
		t.Fatalf("password rotation: %v", err)
	}

	env.Req.Secrets = map[string]string{"exposure_password": replacement}
	if _, err := handleProtectVerify(ctx, env); err != nil {
		t.Fatalf("replacement password: %v", err)
	}
}

func TestProtectStatusIgnoresLegacyState(t *testing.T) {
	g, clock := newTestGuard(t)
	installWriteApproval(t, clock)
	secret := setupWriteApproval(t, clock)
	old := defaultProtectGuard
	defaultProtectGuard = g

	t.Cleanup(func() { defaultProtectGuard = old })

	if err := os.WriteFile(g.path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := handleProtectStatus(context.Background(), &Env{DeviceIdentity: writeIdentity()})
	if err != nil {
		t.Fatal(err)
	}

	status := mustAs[protectStatus](t, result.Data)
	if status.ApprovalFactors == nil || !status.ApprovalFactors.TOTP || secret == "" {
		t.Fatalf("status: %+v", status)
	}
}
