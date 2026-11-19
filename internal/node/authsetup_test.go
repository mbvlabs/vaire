package node

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/totp"
)

func TestFactorSetupFirstPassword(t *testing.T) {
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: time.Now}
	ctx := t.Context()

	const pw = "a sufficiently long password"

	if err := g.check(ctx, pw, ""); err == nil {
		t.Fatal("approved with no enrolled factor")
	}

	var wg sync.WaitGroup

	results := make(chan error, 2)

	for range 2 {
		wg.Go(func() { results <- g.setup(ctx, "password", "", "", pw, "", "") })
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

	if err := g.check(ctx, pw, ""); err != nil {
		t.Fatal(err)
	}
}

func TestFactorSetupTOTPAndReplacement(t *testing.T) {
	now := time.Unix(1800000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: func() time.Time { return now }}
	ctx := t.Context()

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	key, err := totp.DecodeSecret(secret)
	if err != nil {
		t.Fatal(err)
	}

	if err := g.setup(ctx, "totp", "", "", "", secret, "wrong"); err == nil {
		t.Fatal("accepted invalid confirmation")
	}

	if err := g.setup(ctx, "totp", "", "", "", secret, totp.Code(key, now)); err != nil {
		t.Fatal(err)
	}

	if err := g.check(ctx, "", totp.Code(key, now)); err == nil {
		t.Fatal("accepted confirmation replay")
	}

	now = now.Add(totp.Period)

	const pw = "a sufficiently long password"

	if err := g.setup(ctx, "password", "", "", pw, "", ""); err == nil {
		t.Fatal("added password without an existing factor")
	}

	if err := g.setup(ctx, "password", "", totp.Code(key, now), pw, "", ""); err != nil {
		t.Fatal(err)
	}

	if err := g.check(ctx, pw, ""); err != nil {
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
