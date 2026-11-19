package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/totp"
)

func TestFactorSetupCodeLifecycle(t *testing.T) {
	now := time.Unix(1800000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: func() time.Time { return now }}
	ctx := context.Background()

	first, err := g.issueSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	code, err := g.issueSetupCode(ctx)
	if err != nil || code == first {
		t.Fatalf("reissue: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(g.dir, "state.json"))
	if err != nil || strings.Contains(string(raw), code) || strings.Contains(string(raw), first) {
		t.Fatalf("code exposed in state: %v", err)
	}

	if err := g.setup(ctx, "password", first, "", "", "a sufficiently long password", "", ""); err == nil {
		t.Fatal("superseded code accepted")
	}

	now = now.Add(factorSetupCodeLifetime)

	if err := g.setup(ctx, "password", code, "", "", "a sufficiently long password", "", ""); err == nil {
		t.Fatal("expired code accepted")
	}

	code, err = g.issueSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	results := make(chan error, 2)

	for range 2 {
		wg.Go(func() { results <- g.setup(ctx, "password", code, "", "", "a sufficiently long password", "", "") })
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
		t.Fatalf("concurrent setup successes: %d", successes)
	}

	st, err := g.load()
	if err != nil || st.SetupCodeHash != "" || !st.SetupCodeExpires.IsZero() {
		t.Fatalf("code not consumed: %v", err)
	}

	if _, err := g.issueSetupCode(ctx); err == nil {
		t.Fatal("issued code after enrollment")
	}
}

func TestFactorSetupTOTPAndReplacement(t *testing.T) {
	now := time.Unix(1800000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: func() time.Time { return now }}
	ctx := context.Background()

	code, err := g.issueSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	key, _ := totp.DecodeSecret(secret)
	if err := g.setup(ctx, "totp", code, "", "", "", secret, "wrong"); err == nil {
		t.Fatal("accepted invalid confirmation")
	}

	if err := g.setup(ctx, "totp", code, "", "", "", secret, totp.Code(key, now)); err != nil {
		t.Fatal(err)
	}

	if err := g.check(ctx, "", totp.Code(key, now)); err == nil {
		t.Fatal("accepted confirmation replay")
	}

	now = now.Add(totp.Period)

	if err := g.setup(ctx, "password", code, "", "", "a sufficiently long password", "", ""); err == nil {
		t.Fatal("accepted setup code in place of enrolled factor")
	}

	if err := g.setup(ctx, "password", "", "", totp.Code(key, now), "a sufficiently long password", "", ""); err != nil {
		t.Fatal(err)
	}

	if err := g.check(ctx, "a sufficiently long password", ""); err != nil {
		t.Fatal(err)
	}
}

func TestFactorSetupLockout(t *testing.T) {
	now := time.Unix(1800000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: func() time.Time { return now }}
	ctx := context.Background()

	code, err := g.issueSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for range protectMaxFailures {
		if err := g.setup(ctx, "password", "wrong", "", "", "a sufficiently long password", "", ""); err == nil {
			t.Fatal("accepted incorrect code")
		}
	}

	if err := g.setup(ctx, "password", code, "", "", "a sufficiently long password", "", ""); err == nil {
		t.Fatal("accepted code while locked")
	}

	if _, err := g.issueSetupCode(ctx); err == nil {
		t.Fatal("issued code while locked")
	}

	now = now.Add(protectLockout)

	code, err = g.issueSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := g.setup(ctx, "password", code, "", "", "a sufficiently long password", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestIssueFactorSetupCodeRequiresTrustedRoot(t *testing.T) {
	t.Setenv("SUDO_UID", "1000")

	if _, err := IssueFactorSetupCode(context.Background()); err == nil {
		t.Fatal("untrusted sudo session issued a code")
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
