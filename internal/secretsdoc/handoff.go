package secretsdoc

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"

	"filippo.io/age/armor"
	"golang.org/x/crypto/ssh"
)

// Field is one labelled value of the handoff. Value may span several lines
// (a private key or the break-glass text).
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Group is one password-manager entry of the handoff.
type Group struct {
	Title  string  `json:"title"`
	Fields []Field `json:"fields"`
}

// Groups returns every value of doc, grouped the way a password manager
// entry is laid out. Sections the document doesn't have yet are left out.
func (d Document) Groups() []Group {
	groups := []Group{{Title: "Cluster " + d.Cluster.Name, Fields: nonEmpty(
		Field{"Name", d.Cluster.Name},
		Field{"Domain", d.Cluster.Domain},
		Field{"Stanza", d.Cluster.Stanza},
		Field{"TLS mode", d.Cluster.TLSMode},
		Field{"eru version", d.EruVersion},
		Field{"Created at", d.CreatedAt},
	)}}
	for _, n := range d.Nodes {
		fields := nonEmpty(
			Field{"Role", n.Role},
			Field{"Public address", n.PublicAddress},
			Field{"WireGuard port", strconv.Itoa(n.ListenPort)},
			Field{"WireGuard address", n.WGAddress},
			Field{"WireGuard public key", n.WGPublicKey},
		)
		for _, line := range n.SSHHostKeys {
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				continue
			}
			fields = append(fields, Field{"SSH host key " + key.Type(), ssh.FingerprintSHA256(key)})
		}
		groups = append(groups, Group{Title: "Node " + n.Name, Fields: fields})
	}
	groups = append(groups, Group{Title: "Operator " + d.Operator.Name, Fields: nonEmpty(
		Field{"Admin user", d.Operator.SSHUser},
		Field{"Sudo password", d.Operator.SudoPassword},
		Field{"WireGuard address", d.Operator.WGAddress},
		Field{"WireGuard private key", d.Operator.WGPrivateKey},
		Field{"SSH private key", d.Operator.SSHPrivateKey},
	)})
	if db := d.Database; db != nil {
		groups = append(groups, Group{Title: "Database admin", Fields: roleFields("", db.Admin)})
		for _, s := range db.Databases {
			var fields []Field
			for _, name := range []string{"migrate", "app", "ro"} {
				if r, ok := s.Roles[name]; ok {
					fields = append(fields, roleFields(name+" ", r)...)
				}
			}
			for _, name := range sortedKeys(s.URLs) {
				fields = append(fields, Field{name + " URL", s.URLs[name]})
			}
			groups = append(groups, Group{Title: "Database " + s.Name, Fields: fields})
		}
		if len(db.Services) > 0 {
			var fields []Field
			for _, name := range sortedKeys(db.Services) {
				fields = append(fields, roleFields(name+" ", db.Services[name])...)
			}
			groups = append(groups, Group{Title: "Services", Fields: fields})
		}
	}
	if b := d.Backup; b != nil {
		groups = append(groups, Group{Title: "Backups", Fields: nonEmpty(
			Field{"Provider", b.Provider},
			Field{"Endpoint", b.Endpoint},
			Field{"Region", b.Region},
			Field{"Bucket", b.Bucket},
			Field{"Prefix", b.Prefix},
			Field{"Access key ID", b.AccessKeyID},
			Field{"Secret access key", b.SecretAccessKey},
			Field{"pgBackRest cipher pass", b.PgBackRest.CipherPass},
			Field{"restic password", b.Restic.Password},
		)})
	}
	if m := d.Metrics; m != nil {
		groups = append(groups, Group{Title: "Metrics", Fields: nonEmpty(
			Field{"Base URL", m.BaseURL},
			Field{"Username", m.Username},
			Field{"Password", m.Password},
		)})
	}
	if d.ConnectionNotes != "" || d.BreakGlass != "" {
		groups = append(groups, Group{Title: "Notes", Fields: nonEmpty(
			Field{"Connections", d.ConnectionNotes},
			Field{"Break-glass", d.BreakGlass},
		)})
	}
	return groups
}

func roleFields(prefix string, r Role) []Field {
	return nonEmpty(Field{prefix + "user", r.User}, Field{prefix + "password", r.Password})
}

func nonEmpty(fields ...Field) []Field {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		if f.Value != "" {
			out = append(out, f)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EncryptArmored returns doc as an ASCII-armored age file, the transfer
// bundle that is pasted into eru login.
func EncryptArmored(doc Document, passphrase string) ([]byte, error) {
	raw, err := Encrypt(doc, passphrase)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w := armor.NewWriter(&out)
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("armoring the bundle: %w", err)
	}
	return out.Bytes(), nil
}

// IsArmored reports whether raw starts with the age armor header.
func IsArmored(raw []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte(ArmorHeader))
}

// ArmorHeader starts an armored bundle.
const ArmorHeader = armor.Header

// ArmorFooter ends an armored bundle; a pasted bundle is read up to it.
const ArmorFooter = armor.Footer
