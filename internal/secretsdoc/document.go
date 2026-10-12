// Package secretsdoc is the age-encrypted secrets document (§10.1): its
// schema, encryption with a generated EFF-wordlist passphrase, the
// break-glass text, and turning a document into a local context. The document holds cluster access
// credentials only; application database passwords are never in it.
package secretsdoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
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

// ConnectionNotes explains the URLs eru db create and eru db credentials
// --rotate show (§10.1).
const ConnectionNotes = "The app URL goes through PgBouncer on port 6432 in transaction mode; use it for application traffic. " +
	"The migrate URL connects straight to PostgreSQL on port 5432 for session features that migrations need. " +
	"Port 5432 is reachable only over WireGuard (eru tunnel --target postgres) or when network.expose_postgres = true. " +
	"The app_direct_tls URL is the same PgBouncer through port 443; it does not bypass PgBouncer. It is for networks that only allow 443, " +
	"and the client must negotiate TLS directly (sslnegotiation=direct, PostgreSQL 17+ libpq or pgx 5.7+); other clients get \"server refused TLS\" and should use 6432. " +
	"It follows the same client allowlist as 6432."

// Cluster is the cluster identity.
type Cluster struct {
	Name    string `json:"name"`
	Domain  string `json:"domain,omitempty"`
	Stanza  string `json:"stanza"`
	TLSMode string `json:"tls_mode,omitempty"`
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

// Database holds the cluster-wide database credentials (D4): eru_admin and the
// service roles. It is the same shape the operator context stores.
type Database = contexts.DatabaseCreds

// Role is a user and password.
type Role = contexts.Role

// DatabaseURLs builds the connection strings for one database (§10.1). The app
// role uses PgBouncer on 6432, and also on 443 with direct TLS through nginx;
// the migrate role connects straight to Postgres on 5432, reachable only over
// WireGuard or when expose_postgres is set.
func DatabaseURLs(domain, tlsMode, db string, roles map[string]contexts.Role) map[string]string {
	query := "sslmode=verify-full&sslrootcert=system"
	if tlsMode == "self-signed" {
		query = "sslmode=require"
	}
	out := map[string]string{}
	if r, ok := roles["app"]; ok {
		out["app"] = fmt.Sprintf("postgres://%s:%s@%s:6432/%s?%s", r.User, r.Password, domain, db, query)
		out["app_direct_tls"] = fmt.Sprintf("postgres://%s:%s@%s:443/%s?%s&sslnegotiation=direct", r.User, r.Password, domain, db, query)
	}
	if r, ok := roles["migrate"]; ok {
		out["migrate"] = fmt.Sprintf("postgres://%s:%s@%s:5432/%s?%s", r.User, r.Password, domain, db, query)
	}
	return out
}

// Backup is the recovery kit (Phases 3 and 7).
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

// Metrics is the metrics endpoint credential (Phase 5).
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
		out = append(out, d.Database.SecretValues()...)
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

// Decrypt opens an age file, binary or ASCII-armored, with passphrase. A
// wrong passphrase or a damaged file returns secrets_error.
func Decrypt(raw []byte, passphrase string) (Document, error) {
	identity, err := age.NewScryptIdentity(strings.TrimSpace(passphrase))
	if err != nil {
		return Document{}, secretsError("the passphrase is empty", err)
	}
	var src io.Reader = bytes.NewReader(raw)
	if IsArmored(raw) {
		src = armor.NewReader(bytes.NewReader(bytes.TrimSpace(raw)))
	}
	r, err := age.Decrypt(src, identity)
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
	e := output.NewError(output.CodeSecrets, message, output.ExitSecrets, "Check the passphrase and the bundle, then retry.")
	e.Cause = cause
	return e
}
