package secretsdoc

import (
	"crypto/rand"
	_ "embed"
	"math/big"
	"strings"
	"sync"
)

// eff_large_wordlist.txt is the EFF large diceware list (7776 words,
// https://www.eff.org/files/2016/07/18/eff_large_wordlist.txt), kept verbatim.
//
//go:embed eff_large_wordlist.txt
var wordlistRaw string

// PassphraseWords is the passphrase length: 8 words, about 103 bits.
const PassphraseWords = 8

var (
	wordsOnce sync.Once
	words     []string
)

func wordlist() []string {
	wordsOnce.Do(func() {
		for _, line := range strings.Split(wordlistRaw, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				words = append(words, fields[1])
			}
		}
		if len(words) != 7776 {
			panic("embedded EFF wordlist is damaged")
		}
	})
	return words
}

// NewPassphrase returns 8 EFF words joined with "-", drawn with crypto/rand.
func NewPassphrase() (string, error) {
	list := wordlist()
	max := big.NewInt(int64(len(list)))
	out := make([]string, PassphraseWords)
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = list[n.Int64()]
	}
	return strings.Join(out, "-"), nil
}
