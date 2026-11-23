package steps

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestRenderAccessSSHAuthorizedKey(t *testing.T) {
	key, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		t.Fatal(err)
	}

	bare := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(key)), "\n")

	line, err := RenderAccessSSHAuthorizedKey("10.23.4.5/32", bare)
	if err != nil {
		t.Fatal(err)
	}

	want := `restrict,from="10.23.4.5/32",command="/usr/local/bin/vaire node access-ssh" ` + bare + "\n"
	if line != want {
		t.Fatalf("got %q, want %q", line, want)
	}

	parsed, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || parsed.Type() != ssh.KeyAlgoED25519 || comment != "" || len(rest) != 0 {
		t.Fatalf("rendered entry did not parse: key=%v comment=%q rest=%q err=%v", parsed, comment, rest, err)
	}

	if strings.Join(options, ",") != `restrict,from="10.23.4.5/32",command="/usr/local/bin/vaire node access-ssh"` {
		t.Fatalf("unexpected options: %q", options)
	}
}

func TestRenderAccessSSHAuthorizedKeyRejectsInvalidInputs(t *testing.T) {
	key, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		t.Fatal(err)
	}

	bare := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(key)), "\n")

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	otherKey, err := ssh.NewPublicKey(other.Public())
	if err != nil {
		t.Fatal(err)
	}

	otherBare := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(otherKey)), "\n")

	tests := []struct {
		name, source, key string
	}{
		{"missing prefix", "10.0.0.1", bare},
		{"broad prefix", "10.0.0.0/24", bare},
		{"ipv6", "::1/128", bare},
		{"mapped ipv6", "::ffff:10.0.0.1/128", bare},
		{"source injection", "10.0.0.1/32\",command=\"evil", bare},
		{"source newline", "10.0.0.1/32\n", bare},
		{"empty key", "10.0.0.1/32", ""},
		{"comment", "10.0.0.1/32", bare + " comment"},
		{"options", "10.0.0.1/32", "permitopen=\"*:*\" " + bare},
		{"newline", "10.0.0.1/32", bare + "\n"},
		{"second key", "10.0.0.1/32", bare + "\n" + bare},
		{"control", "10.0.0.1/32", bare + "\x00"},
		{"extra space", "10.0.0.1/32", strings.Replace(bare, " ", "  ", 1)},
		{"unsupported type", "10.0.0.1/32", otherBare},
		{"mismatched type", "10.0.0.1/32", strings.Replace(bare, "ssh-ed25519", "ssh-rsa", 1)},
		{"malformed key", "10.0.0.1/32", "ssh-ed25519 AAAA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderAccessSSHAuthorizedKey(tt.source, tt.key)
			if err == nil || got != "" {
				t.Fatalf("got %q, %v; want empty result and error", got, err)
			}
		})
	}
}
