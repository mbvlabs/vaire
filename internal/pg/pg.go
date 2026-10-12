// Package pg talks to PostgreSQL: the node side through psql as the postgres
// OS user over the unix socket (§9.1), and the operator side through pgx over
// the WireGuard tunnel (D12).
package pg

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eru/internal/secrets"
)

// SCRAMIterations is the SCRAM-SHA-256 iteration count (§10.2).
const SCRAMIterations = 4096

// SCRAMVerifier computes a SCRAM-SHA-256 verifier for password, in the
// pg_authid.rolpassword form SCRAM-SHA-256$<iter>:<salt>$<stored>:<server>.
// The plaintext never reaches psql, the server log or pg_stat_statements. The
// verifier is registered for redaction.
func SCRAMVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, SCRAMIterations, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	verifier := fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		SCRAMIterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(storedKey[:]),
		base64.StdEncoding.EncodeToString(serverKey),
	)
	secrets.Add(verifier)
	return verifier, nil
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// Quote returns ident quoted for SQL, using pgx's identifier sanitizer.
func Quote(ident string) string {
	return pgx.Identifier{ident}.Sanitize()
}
