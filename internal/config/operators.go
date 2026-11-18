package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// OperatorsPath is the node's operator registry: every operator is a
// WireGuard peer and an authorized SSH key for the admin user.
const OperatorsPath = "/etc/vaire/operators.toml"

// Operators is /etc/vaire/operators.toml.
type Operators struct {
	Operators []Operator `toml:"operators"`
}

// Operator is one operator identity. Only public halves live on the node.
type Operator struct {
	Name         string `toml:"name" json:"name"`
	WGAddress    string `toml:"wg_address" json:"wg_address"`
	WGPublicKey  string `toml:"wg_public_key" json:"wg_public_key"`
	SSHPublicKey string `toml:"ssh_public_key" json:"ssh_public_key"`
}

// LoadOperators reads path. A missing file is an empty registry.
func LoadOperators(path string) (Operators, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if IsNotExist(err) {
			return Operators{}, nil
		}

		return Operators{}, err
	}

	var ops Operators
	if err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&ops); err != nil {
		return Operators{}, configError(path, err)
	}

	return ops, nil
}

// Upsert adds op, or updates the operator with the same name when its public
// keys match. A different identity under that name is refused: a fresh init
// must never replace a foreign registration.
func (o *Operators) Upsert(op Operator) error {
	for i := range o.Operators {
		if o.Operators[i].Name != op.Name {
			continue
		}

		if !sameOperatorKeys(o.Operators[i], op) {
			return fmt.Errorf("operator %s is already registered with different public keys", op.Name)
		}

		o.Operators[i] = op

		return nil
	}

	o.Operators = append(o.Operators, op)

	return nil
}

// sameOperatorKeys reports whether a and b grant the same keys. The SSH
// authorized_keys comment is not part of the identity: it carries the server
// or workspace name, which can differ on a renamed workspace.
func sameOperatorKeys(a, b Operator) bool {
	return a.WGPublicKey == b.WGPublicKey && sameAuthorizedKey(a.SSHPublicKey, b.SSHPublicKey)
}

func sameAuthorizedKey(a, b string) bool {
	af, bf := strings.Fields(a), strings.Fields(b)
	if len(af) >= 2 && len(bf) >= 2 {
		return af[0] == bf[0] && af[1] == bf[1]
	}

	return a == b
}

// Encode returns operators.toml bytes.
func (o Operators) Encode() ([]byte, error) {
	raw, err := toml.Marshal(o)
	if err != nil {
		return nil, err
	}

	return append([]byte("# managed by vaire: operator WireGuard peers and SSH keys\n"), raw...), nil
}

// EncodeNode returns node.toml bytes.
func EncodeNode(n Node) ([]byte, error) {
	if n.InstalledAt.IsZero() {
		n.InstalledAt = time.Now().UTC().Truncate(time.Second)
	}

	return toml.Marshal(n)
}
