package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/totp"
)

func TestExposureFactorEnrollmentAndReplay(t *testing.T) {
	now := time.Unix(1800000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: func() time.Time { return now }}
	ctx := context.Background()

	const pw = "a sufficiently long password"
	if err := g.check(ctx, pw, ""); err == nil {
		t.Fatal("unenrolled factor accepted")
	}

	if err := g.enroll(ctx, "password", "", "", pw, nil); err != nil {
		t.Fatal(err)
	}

	if err := g.check(ctx, pw, ""); err != nil {
		t.Fatal(err)
	}

	var secret string

	confirm := func(s string) (string, error) {
		secret = s

		key, err := totp.DecodeSecret(s)
		if err != nil {
			return "", err
		}

		return totp.Code(key, now), nil
	}
	if err := g.enroll(ctx, "totp", "wrong", "", "", confirm); err == nil {
		t.Fatal("added factor without existing factor")
	}

	if err := g.enroll(ctx, "totp", pw, "", "", confirm); err != nil {
		t.Fatal(err)
	}

	key, err := totp.DecodeSecret(secret)
	if err != nil {
		t.Fatal(err)
	}

	if err := g.check(ctx, "", totp.Code(key, now)); err == nil {
		t.Fatal("confirmation code replayed")
	}

	now = now.Add(totp.Period)
	code := totp.Code(key, now)

	var wg sync.WaitGroup

	results := make(chan error, 2)

	for range 2 {
		wg.Go(func() {
			results <- g.check(ctx, "", code)
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
		t.Fatalf("concurrent uses accepted %d times", successes)
	}

	raw, err := os.ReadFile(filepath.Join(g.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	var st exposureFactorState

	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}

	if st.PasswordHash == pw || st.PasswordHash == "" || st.Secret != secret {
		t.Fatal("factor state not stored correctly")
	}

	info, err := os.Stat(filepath.Join(g.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode: %v", info.Mode())
	}
}

func TestExposureFactorLockoutAndReplacement(t *testing.T) {
	now := time.Unix(1800000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: func() time.Time { return now }}
	ctx := context.Background()

	const old = "old password long enough"

	const newPW = "new password long enough"

	if err := g.enroll(ctx, "password", "", "", old, nil); err != nil {
		t.Fatal(err)
	}

	for range protectMaxFailures {
		if err := g.check(ctx, "bad", ""); err == nil {
			t.Fatal("bad password accepted")
		}
	}

	if err := g.enroll(ctx, "password", old, "", newPW, nil); err == nil {
		t.Fatal("enrollment bypassed lockout")
	}

	now = now.Add(protectLockout)

	if err := g.enroll(ctx, "password", old, "", newPW, nil); err != nil {
		t.Fatal(err)
	}

	if err := g.check(ctx, old, ""); err == nil {
		t.Fatal("old password accepted after replacement")
	}

	if err := g.check(ctx, newPW, ""); err != nil {
		t.Fatal(err)
	}
}

func TestExposureFactorConfirmationAndCancellation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	g := exposureFactorGuard{dir: filepath.Join(t.TempDir(), "factor"), now: func() time.Time { return now }}
	ctx := context.Background()

	if err := g.enroll(ctx, "totp", "", "", "", func(string) (string, error) { return "invalid", nil }); err == nil {
		t.Fatal("bad confirmation accepted")
	}

	if err := g.check(ctx, "", "000000"); err == nil {
		t.Fatal("unconfirmed factor accepted")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if err := g.enroll(cancelled, "password", "", "", "long enough password", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled enrollment: %v", err)
	}
}
