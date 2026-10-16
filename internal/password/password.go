// Package password hashes and verifies the master password that gates
// eru secrets export. Only the argon2id hash is stored, never the password.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
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
	params, salt, want, ok := parseHash(encoded)
	if !ok {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, params[1], params[0], uint8(params[2]), uint32(len(want))) //nolint:gosec // G115: parseHash bounds all parameters and lengths.
	return subtle.ConstantTimeCompare(got, want) == 1
}

var hashParams = regexp.MustCompile(`^m=([1-9][0-9]*),t=([1-9][0-9]*),p=([1-9][0-9]*)$`)

// Valid checks an encoded verifier without deriving a key. Imported hashes
// must have bounded costs too: they are data, not resource-allocation policy.
func Valid(encoded string) bool {
	_, _, _, ok := parseHash(encoded)
	return ok
}

func parseHash(encoded string) (params [3]uint32, salt, sum []byte, ok bool) {
	if len(encoded) > 256 {
		return params, nil, nil, false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return params, nil, nil, false
	}
	m := hashParams.FindStringSubmatch(parts[3])
	if m == nil {
		return params, nil, nil, false
	}
	// At most 64 MiB, three passes and four lanes. Raise these bounds
	// deliberately if a future release starts generating stronger hashes.
	limits := [3]uint64{64 * 1024, 3, 4}
	for i := range params {
		n, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil || n == 0 || n > limits[i] {
			return params, nil, nil, false
		}
		params[i] = uint32(n)
	}
	if params[0] < 8*params[2] {
		return params, nil, nil, false
	}
	var err error
	salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < saltLen || len(salt) > 32 || base64.RawStdEncoding.EncodeToString(salt) != parts[4] {
		return params, nil, nil, false
	}
	sum, err = base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(sum) < keyLen || len(sum) > 64 || base64.RawStdEncoding.EncodeToString(sum) != parts[5] {
		return params, nil, nil, false
	}
	return params, salt, sum, true
}
