package backup

import "filippo.io/age"

// NewIdentity generates the age X25519 identity master recovery bundles are
// encrypted to. The operator keeps it; the node gets only the recipient.
func NewIdentity() (identity, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}
