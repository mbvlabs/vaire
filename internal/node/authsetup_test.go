package node

import (
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/totp"
)

func TestFactorSetupFirstPassword(t *testing.T) {
	clock := &testClock{t: time.Now()}
	installWriteApproval(t, clock)
	ctx := t.Context()

	const pw = "a sufficiently long password"

	env := &Env{DeviceIdentity: writeIdentity(), Req: rpc.Request{Secrets: map[string]string{"exposure_password": pw}}}
	if err := verifyOperatorApproval(ctx, env); err == nil {
		t.Fatal("approved with no enrolled factor")
	}

	var wg sync.WaitGroup

	results := make(chan error, 2)

	for range 2 {
		wg.Go(func() {
			_, err := handleAuthSetup(ctx, &Env{DeviceIdentity: writeIdentity(), Req: rpc.Request{
				Params: []byte(`{"mode":"password"}`), Secrets: map[string]string{"new_password": pw},
			}})
			results <- err
		})
	}

	wg.Wait()
	close(results)

	successes := 0

	for err := range results {
		if err == nil {
			successes++
		}
	}

	if successes != 1 {
		t.Fatalf("concurrent first enrollments = %d, want 1", successes)
	}

	if err := verifyOperatorApproval(ctx, env); err != nil {
		t.Fatal(err)
	}
}

func TestFactorSetupTOTPAndReplacement(t *testing.T) {
	clock := &testClock{t: time.Unix(1800000000, 0)}
	installWriteApproval(t, clock)
	ctx := t.Context()

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	key, err := totp.DecodeSecret(secret)
	if err != nil {
		t.Fatal(err)
	}

	env := &Env{DeviceIdentity: writeIdentity(), Req: rpc.Request{Params: []byte(`{"mode":"totp"}`), Secrets: map[string]string{
		"new_totp_secret": secret, "new_totp_code": "wrong",
	}}}
	if _, err := handleAuthSetup(ctx, env); err == nil {
		t.Fatal("accepted invalid confirmation")
	}

	env.Req.Secrets["new_totp_code"] = totp.Code(key, clock.t)
	if _, err := handleAuthSetup(ctx, env); err != nil {
		t.Fatal(err)
	}

	env.Req.Secrets = map[string]string{"exposure_totp": totp.Code(key, clock.t)}
	if err := verifyOperatorApproval(ctx, env); err == nil {
		t.Fatal("accepted confirmation replay")
	}

	clock.t = clock.t.Add(totp.Period)

	const pw = "a sufficiently long password"

	env.Req.Params = []byte(`{"mode":"password"}`)
	env.Req.Secrets = map[string]string{"new_password": pw}

	if _, err := handleAuthSetup(ctx, env); err == nil {
		t.Fatal("added password without an existing factor")
	}

	env.Req.Secrets["exposure_totp"] = totp.Code(key, clock.t)
	if _, err := handleAuthSetup(ctx, env); err != nil {
		t.Fatal(err)
	}

	env.Req.Secrets = map[string]string{"exposure_password": pw}
	if err := verifyOperatorApproval(ctx, env); err != nil {
		t.Fatal(err)
	}
}

func TestFactorSetupMethodsRegistered(t *testing.T) {
	for _, name := range []string{"auth.setup", "auth.status"} {
		m, ok := methods()[name]
		if !ok || m.handler == nil || m.runLog != nil {
			t.Fatalf("%s missing or logged", name)
		}
	}
}
