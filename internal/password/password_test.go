package password

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRejectMalformedHashes(t *testing.T) {
	hash, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(hash, "$")
	with := func(i int, value string) string {
		copyParts := append([]string(nil), parts...)
		copyParts[i] = value
		return strings.Join(copyParts, "$")
	}
	bad := []string{"", "not a hash", hash + "$extra", hash + "\n", strings.Repeat("x", 257)}
	for _, params := range []string{
		"m=65536,t=0,p=4", "m=0,t=1,p=4", "m=65536,t=1,p=0",
		"m=4294967295,t=1,p=4", "m=65536,t=4294967295,p=4", "m=65536,t=1,p=255",
		"m=65536,t=1,p=4garbage", "m=65536,t=1,p=4,extra=1", "m=065536,t=1,p=4",
		"m=8,t=1,p=4", "m=-1,t=1,p=4", "m=99999999999999999999,t=1,p=4",
	} {
		bad = append(bad, with(3, params))
	}
	for _, i := range []int{4, 5} {
		for _, encoded := range []string{"", "!", "aGFzaA", base64.RawStdEncoding.EncodeToString(make([]byte, 65)), parts[i] + "="} {
			bad = append(bad, with(i, encoded))
		}
	}
	bad = append(bad, with(2, "v=19junk"), with(2, "v=18"), with(1, "argon2i"))
	for _, encoded := range bad {
		if Valid(encoded) || Verify(encoded, "anything") {
			t.Errorf("accepted malformed verifier %q", encoded)
		}
	}
	if !Valid(hash) {
		t.Fatal("generated verifier is invalid")
	}
}

func FuzzVerifyMalformedHash(f *testing.F) {
	f.Add("$argon2id$v=19$m=65536,t=0,p=4$c2FsdA$aGFzaA")
	f.Add("not a hash")
	f.Fuzz(func(t *testing.T, encoded string) {
		// Invalid encodings must be rejected without deriving a key or panicking.
		if !Valid(encoded) && Verify(encoded, "password") {
			t.Fatal("invalid encoding verified")
		}
	})
}

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
