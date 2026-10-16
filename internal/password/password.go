// Package password hashes and verifies the master password that gates
// eru secrets export. Only the argon2id hash is stored, never the password.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// MinLength is the shortest master password accepted. It is a long-lived
// secret that guards the whole secrets document, so it is longer than a
// login password.
const MinLength = 12

// Argon2id parameters. They are recorded in the encoded hash, so a later
// change still verifies old hashes.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	keyLen       = 32
	saltLen      = 16
)

// ErrTooShort is returned by Hash for a password under MinLength.
var ErrTooShort = fmt.Errorf("the master password must be at least %d characters", MinLength)

// Hash returns the encoded argon2id hash of pw, of the form
// $argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>.
func Hash(pw string) (string, error) {
	if len(pw) < MinLength {
		return "", ErrTooShort
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// Verify reports whether pw matches the encoded hash. A malformed hash
// verifies nothing.
func Verify(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || p == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want))) //nolint:gosec // G115: len fits uint32 for any stored hash.
	return subtle.ConstantTimeCompare(got, want) == 1
}
