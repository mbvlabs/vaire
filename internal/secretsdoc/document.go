// Package secretsdoc is the age-encrypted secrets document (§10.1): its
// schema, encryption with a generated EFF-wordlist passphrase, the
// break-glass text, and turning a document into a local context.
package secretsdoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"filippo.io/age"
	"github.com/mbvlabs/eru/internal/cli/output"
)

// Schema is the plaintext document schema.
const Schema = "eru-secrets/v1"

// ScryptWorkFactor is the age scrypt work factor (§10.1).
const ScryptWorkFactor = 18

// maxDocument caps the decrypted plaintext.
const maxDocument = 4 << 20

// Document is the plaintext of the secrets document. Database, backup and
// metrics are filled from Phase 2 on.
type Document struct {
	Schema     string    `json:"schema"`
	CreatedAt  string    `json:"created_at"`
	EruVersion string    `json:"eru_version"`
	Cluster    Cluster   `json:"cluster"`
	Nodes      []Node    `json:"nodes"`
	Operator   Operator  `json:"operator"`
	Database   *Database `json:"database,omitempty"`
	Backup     *Backup   `json:"backup,omitempty"`
	Metrics    *Metrics  `json:"metrics,omitempty"`
	BreakGlass string    `json:"break_glass"`
}

// Cluster is the cluster identity.
type Cluster struct {
	Name   string `json:"name"`
	Domain string `json:"domain,omitempty"`
	Stanza string `json:"stanza"`
}

// Node is one node and its pinned identity.
type Node struct {
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	PublicAddress string   `json:"public_address"`
	ListenPort    int      `json:"listen_port"`
	WGAddress     string   `json:"wg_address"`
	WGPublicKey   string   `json:"wg_public_key"`
	SSHHostKeys   []string `json:"ssh_host_keys"`
}

// Operator is the operator identity the document grants.
type Operator struct {
	Name          string `json:"name"`
	WGAddress     string `json:"wg_address"`
	WGPrivateKey  string `json:"wg_private_key"`
	SSHUser       string `json:"ssh_user"`
	SSHPrivateKey string `json:"ssh_private_key"`
	SudoPassword  string `json:"sudo_password"`
}

// Database holds database credentials (Phase 2).
type Database struct {
	Admin     Role       `json:"admin"`
	Databases []DBSecret `json:"databases"`
}

// Role is a user and password.
type Role struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// DBSecret is one database's roles and URLs (Phase 2).
type DBSecret struct {
	Name  string            `json:"name"`
	Roles map[string]Role   `json:"roles"`
	URLs  map[string]string `json:"urls"`
}

// Backup is the recovery kit (Phases 3 and 4).
type Backup struct {
	Provider        string `json:"provider"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix"`
	URIStyle        string `json:"uri_style"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	PgBackRest      struct {
		Stanza     string `json:"stanza"`
		CipherPass string `json:"cipher_pass"`
	} `json:"pgbackrest"`
	Restic struct {
		Password string `json:"password"`
	} `json:"restic"`
}

// Metrics is the metrics endpoint credential (Phase 6).
type Metrics struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// SecretValues returns every secret in the document, for the redaction
// registry.
func (d Document) SecretValues() []string {
	out := []string{d.Operator.WGPrivateKey, d.Operator.SSHPrivateKey, d.Operator.SudoPassword}
	if d.Database != nil {
		out = append(out, d.Database.Admin.Password)
		for _, db := range d.Database.Databases {
			for _, r := range db.Roles {
				out = append(out, r.Password)
			}
			for _, u := range db.URLs {
				out = append(out, u)
			}
		}
	}
	if d.Backup != nil {
		out = append(out, d.Backup.SecretAccessKey, d.Backup.PgBackRest.CipherPass, d.Backup.Restic.Password)
	}
	if d.Metrics != nil {
		out = append(out, d.Metrics.Password)
	}
	return out
}

// Encrypt returns the age file for doc, encrypted to passphrase.
func Encrypt(doc Document, passphrase string) ([]byte, error) {
	plain, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	recipient.SetWorkFactor(ScryptWorkFactor)
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipient)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Decrypt opens an age file with passphrase. A wrong passphrase or a damaged
// file returns secrets_error.
func Decrypt(raw []byte, passphrase string) (Document, error) {
	identity, err := age.NewScryptIdentity(strings.TrimSpace(passphrase))
	if err != nil {
		return Document{}, secretsError("the passphrase is empty", err)
	}
	r, err := age.Decrypt(bytes.NewReader(raw), identity)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return Document{}, secretsError("the passphrase does not open this secrets document", err)
		}
		return Document{}, secretsError("the file is not an eru secrets document or is damaged", err)
	}
	plain, err := io.ReadAll(io.LimitReader(r, maxDocument))
	if err != nil {
		return Document{}, secretsError("the passphrase does not open this secrets document", err)
	}
	var doc Document
	if err := json.Unmarshal(plain, &doc); err != nil || doc.Schema != Schema {
		return Document{}, secretsError("the decrypted document is not "+Schema, err)
	}
	return doc, nil
}

func secretsError(message string, cause error) *output.CLIError {
	e := output.NewError(output.CodeSecrets, message, output.ExitSecrets, "Check the passphrase and the file, then retry.")
	e.Cause = cause
	return e
}
