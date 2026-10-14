package totp

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// rfcKey is the SHA1 seed of RFC 4226 and RFC 6238's test vectors.
var rfcKey = []byte("12345678901234567890")

func TestHOTPRFC4226(t *testing.T) {
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for i, w := range want {
		if got := HOTP(rfcKey, int64(i), 6); got != w {
			t.Fatalf("counter %d: got %s, want %s", i, got, w)
		}
	}
}

func TestTOTPRFC6238(t *testing.T) {
	vectors := []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, v := range vectors {
		if got := HOTP(rfcKey, Step(time.Unix(v.unix, 0)), 8); got != v.code {
			t.Fatalf("T=%d: got %s, want %s", v.unix, got, v.code)
		}
		// Six digits are the last six of the eight.
		if got := Code(rfcKey, time.Unix(v.unix, 0)); got != v.code[2:] {
			t.Fatalf("T=%d six digits: got %s, want %s", v.unix, got, v.code[2:])
		}
	}
}

func TestVerifySkewWindow(t *testing.T) {
	now := time.Unix(1234567890, 0)
	step := Step(now)
	for d := int64(-1); d <= 1; d++ {
		code := HOTP(rfcKey, step+d, Digits)
		got, err := Verify(rfcKey, code, now, 0)
		if err != nil || got != step+d {
			t.Fatalf("offset %d: step %d, err %v", d, got, err)
		}
	}
	for _, d := range []int64{-2, 2} {
		if _, err := Verify(rfcKey, HOTP(rfcKey, step+d, Digits), now, 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("offset %d: got %v, want ErrInvalid", d, err)
		}
	}
}

func TestVerifyReplay(t *testing.T) {
	now := time.Unix(1234567890, 0)
	code := Code(rfcKey, now)
	step, err := Verify(rfcKey, code, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(rfcKey, code, now, step); !errors.Is(err, ErrReused) {
		t.Fatalf("replay: got %v, want ErrReused", err)
	}
	// A code from an earlier step than the last accepted one is refused too.
	earlier := HOTP(rfcKey, step-1, Digits)
	if _, err := Verify(rfcKey, earlier, now, step); !errors.Is(err, ErrReused) {
		t.Fatalf("earlier step: got %v, want ErrReused", err)
	}
	// The next step's code still works.
	next := HOTP(rfcKey, step+1, Digits)
	if got, err := Verify(rfcKey, next, now.Add(Period), step); err != nil || got != step+1 {
		t.Fatalf("next step: %d, %v", got, err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	now := time.Unix(1234567890, 0)
	for _, code := range []string{"", "12345", "1234567", "abcdef", "12 34 5x"} {
		if _, err := Verify(rfcKey, code, now, 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q: got %v", code, err)
		}
	}
	spaced := Code(rfcKey, now)
	spaced = spaced[:3] + " " + spaced[3:]
	if _, err := Verify(rfcKey, spaced, now, 0); err != nil {
		t.Fatalf("spaced code: %v", err)
	}
}

func TestSecretRoundTrip(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 32 || strings.Contains(secret, "=") {
		t.Fatalf("secret %q: want 32 base32 characters without padding", secret)
	}
	key, err := DecodeSecret(strings.ToLower(secret[:4]) + " " + secret[4:])
	if err != nil || len(key) != SecretBytes {
		t.Fatalf("decode: %d bytes, %v", len(key), err)
	}
	if _, err := DecodeSecret("not base32!"); err == nil {
		t.Fatal("want an error for a non-base32 secret")
	}
}

func TestURI(t *testing.T) {
	got := URI("eru", "prod", "JBSWY3DPEHPK3PXP")
	want := "otpauth://totp/eru:prod?algorithm=SHA1&digits=6&issuer=eru&period=30&secret=JBSWY3DPEHPK3PXP"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
