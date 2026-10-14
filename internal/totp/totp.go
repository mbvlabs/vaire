// Package totp implements RFC 6238 time-based one-time passwords as
// authenticator apps use them: HMAC-SHA1, six digits, a 30-second step.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 and every authenticator app use HMAC-SHA1.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Digits is the code length.
	Digits = 6
	// Period is the time step.
	Period = 30 * time.Second
	// SecretBytes is the secret length: 160 bits, as RFC 4226 recommends.
	SecretBytes = 20
	// Skew is how many steps before and after the current one are accepted.
	Skew = 1
)

var (
	// ErrInvalid is a code that matches no step in the window.
	ErrInvalid = errors.New("the code is wrong or expired")
	// ErrReused is a code for a step at or before the last accepted one.
	ErrReused = errors.New("the code was already used")
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random 160-bit secret in unpadded base32.
func NewSecret() (string, error) {
	key := make([]byte, SecretBytes)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return encoding.EncodeToString(key), nil
}

// DecodeSecret decodes a base32 secret, ignoring case, spaces and padding.
func DecodeSecret(secret string) ([]byte, error) {
	clean := strings.ToUpper(strings.ReplaceAll(strings.TrimRight(strings.TrimSpace(secret), "="), " ", ""))
	key, err := encoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("the TOTP secret is not base32: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("the TOTP secret is empty")
	}
	return key, nil
}

// Step returns the time step t falls in.
func Step(t time.Time) int64 {
	return t.Unix() / int64(Period/time.Second)
}

// HOTP returns the RFC 4226 code for key at counter with digits digits.
func HOTP(key []byte, counter int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter)) //nolint:gosec // G115: steps are positive.
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// Code returns the six-digit code for key at time t.
func Code(key []byte, t time.Time) string {
	return HOTP(key, Step(t), Digits)
}

// Verify checks code against key for the steps around now (Skew either
// side). A step at or before last was already used and is refused, so a code
// works once. It returns the step that matched, to be stored as the new last.
func Verify(key []byte, code string, now time.Time, last int64) (int64, error) {
	code = NormalizeCode(code)
	if !WellFormed(code) {
		return 0, ErrInvalid
	}
	current := Step(now)
	reused := false
	for d := int64(-Skew); d <= Skew; d++ {
		step := current + d
		if subtle.ConstantTimeCompare([]byte(HOTP(key, step, Digits)), []byte(code)) != 1 {
			continue
		}
		if step <= last {
			reused = true
			continue
		}
		return step, nil
	}
	if reused {
		return 0, ErrReused
	}
	return 0, ErrInvalid
}

// NormalizeCode drops the spaces and dashes people type inside a code.
func NormalizeCode(code string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(code))
}

// WellFormed reports whether code is exactly Digits decimal digits.
func WellFormed(code string) bool {
	if len(code) != Digits {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// URI returns the otpauth:// key URI authenticator apps scan.
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(int(Period/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
