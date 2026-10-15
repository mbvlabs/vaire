package transport

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// KnownHosts is a known_hosts file of pinned host keys. Matching is eru's
// own: a presented key must equal one of the host's pinned keys, so a host
// pinned with several key types never trips a false mismatch.
type KnownHosts struct {
	path  string
	hosts map[string][]ssh.PublicKey
}

// HostKeyError is a presented host key that is not pinned.
type HostKeyError struct {
	Host    string
	Got     string
	Want    []string
	Unknown bool
}

func (e *HostKeyError) Error() string {
	if e.Unknown {
		return "no host key is pinned for " + e.Host
	}
	return "the host key of " + e.Host + " (" + e.Got + ") does not match the pinned key"
}

// HostID is the known_hosts name for addr: the host alone for port 22,
// [host]:port otherwise.
func HostID(addr string) string {
	return knownhosts.Normalize(addr)
}

// LoadKnownHosts reads path. A missing file is empty.
func LoadKnownHosts(path string) (*KnownHosts, error) {
	k := &KnownHosts{path: path, hosts: map[string][]ssh.PublicKey{}}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: known_hosts under the eru config directory.
	if err != nil {
		if os.IsNotExist(err) {
			return k, nil
		}
		return nil, err
	}
	rest := raw
	for len(bytes.TrimSpace(rest)) > 0 {
		_, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, output.NewError(output.CodeConfig, path+" does not parse: "+err.Error(), output.ExitConfig, "Restore it from the secrets document with eru login.")
		}
		for _, host := range hosts {
			k.add(host, key)
		}
		rest = next
	}
	return k, nil
}

func (k *KnownHosts) add(host string, key ssh.PublicKey) {
	for _, existing := range k.hosts[host] {
		if bytes.Equal(existing.Marshal(), key.Marshal()) {
			return
		}
	}
	k.hosts[host] = append(k.hosts[host], key)
}

// Keys returns the keys pinned for addr.
func (k *KnownHosts) Keys(addr string) []ssh.PublicKey {
	return k.hosts[HostID(addr)]
}

// Pin adds keys for addr.
func (k *KnownHosts) Pin(addr string, keys ...ssh.PublicKey) {
	for _, key := range keys {
		k.add(HostID(addr), key)
	}
}

// Replace pins exactly keys for addr, dropping what was pinned before: a
// rebuild puts the old server's host keys on a new VPS (Phase 5).
func (k *KnownHosts) Replace(addr string, keys ...ssh.PublicKey) {
	delete(k.hosts, HostID(addr))
	k.Pin(addr, keys...)
}

// Save writes the file with mode 0600.
func (k *KnownHosts) Save() error {
	hosts := make([]string, 0, len(k.hosts))
	for host := range k.hosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	var b strings.Builder
	for _, host := range hosts {
		for _, key := range k.hosts[host] {
			b.WriteString(knownhosts.Line([]string{host}, key) + "\n")
		}
	}
	return WriteSecretFile(k.path, []byte(b.String()))
}

// Callback accepts only a key pinned for addr.
func (k *KnownHosts) Callback(addr string) ssh.HostKeyCallback {
	host := HostID(addr)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		pinned := k.hosts[host]
		for _, want := range pinned {
			if bytes.Equal(want.Marshal(), key.Marshal()) {
				return nil
			}
		}
		e := &HostKeyError{Host: host, Got: ssh.FingerprintSHA256(key), Unknown: len(pinned) == 0}
		for _, want := range pinned {
			e.Want = append(e.Want, ssh.FingerprintSHA256(want))
		}
		return e
	}
}

// Algorithms returns the host key algorithms to offer for addr, so the server
// presents a key type that is pinned.
func (k *KnownHosts) Algorithms(addr string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(algs ...string) {
		for _, a := range algs {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	for _, key := range k.hosts[HostID(addr)] {
		switch key.Type() {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		default:
			add(key.Type())
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i] == ssh.KeyAlgoED25519 && out[j] != ssh.KeyAlgoED25519 })
	return out
}

// ParseHostKeys parses authorized_keys-style host key lines.
func ParseHostKeys(lines []string) ([]ssh.PublicKey, error) {
	keys := make([]ssh.PublicKey, 0, len(lines))
	for _, line := range lines {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// ContainsKey reports whether key is one of keys.
func ContainsKey(keys []ssh.PublicKey, key ssh.PublicKey) bool {
	for _, k := range keys {
		if bytes.Equal(k.Marshal(), key.Marshal()) {
			return true
		}
	}
	return false
}

// WriteSecretFile writes data with mode 0600 through a temp file and rename,
// creating the 0700 parent directory.
func WriteSecretFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
