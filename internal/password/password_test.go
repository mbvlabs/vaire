package password

import (
	"strings"
	"testing"
)

func TestHashVerify(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash = %q", h)
	}
	if strings.Contains(h, pw) {
		t.Fatal("the hash carries the password")
	}
	if !Verify(h, pw) {
		t.Fatal("the right password did not verify")
	}
	if Verify(h, pw+"x") {
		t.Fatal("a wrong password verified")
	}
	if Verify("not a hash", pw) || Verify("", pw) {
		t.Fatal("a malformed hash verified")
	}
	// Two hashes of the same password differ (fresh salt).
	h2, _ := Hash(pw)
	if h == h2 {
		t.Fatal("two hashes are identical")
	}
}

func TestHashTooShort(t *testing.T) {
	if _, err := Hash(strings.Repeat("a", MinLength-1)); err == nil {
		t.Fatal("a short password was accepted")
	}
	if _, err := Hash(strings.Repeat("a", MinLength)); err != nil {
		t.Fatalf("MinLength password rejected: %v", err)
	}
}
