package secrets

import (
	"crypto/rand"
	"math/big"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Base62 returns n base62 characters from crypto/rand and registers the value
// for redaction (§10.2).
func Base62(n int) (string, error) {
	max := big.NewInt(int64(len(base62)))
	out := make([]byte, n)
	for i := range out {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = base62[k.Int64()]
	}
	value := string(out)
	Add(value)
	return value, nil
}
