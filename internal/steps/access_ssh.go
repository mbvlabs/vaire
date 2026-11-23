package steps

import (
	"errors"
	"net/netip"

	"github.com/mbvlabs/vaire/internal/config"
)

// RenderAccessSSHAuthorizedKey renders one newline-terminated authorized_keys
// entry. The caller is responsible for installing it in a root-owned file and
// configuring sshd without disturbing existing SSH access.
//
// source must be an IPv4 /32 prefix and publicKey must be a canonical, bare
// ssh-ed25519 public key (without options, a comment, or a trailing newline).
func RenderAccessSSHAuthorizedKey(source, publicKey string) (string, error) {
	prefix, err := netip.ParsePrefix(source)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 {
		return "", errors.New("access SSH source must be an IPv4 /32 prefix")
	}

	if err := config.ValidateBareSSHEd25519PublicKey(publicKey); err != nil {
		return "", err
	}

	return `restrict,from="` + prefix.String() + `",command="/usr/local/bin/vaire node access-ssh" ` + publicKey + "\n", nil
}
