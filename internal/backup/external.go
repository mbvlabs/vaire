package backup

// External PostgreSQL copy support (C02): the secure source model, source
// preflight and export, strict archive TOC selection and the strict
// owner-privileged loader. These are copy-only primitives. The tolerant
// restore path in scratch.go is deliberately not reused or changed.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/secrets"
)

// CopyExecutor runs one fixed PostgreSQL client command. engine.Shell
// implements it; tests substitute a recorder. Commands built here never run
// through a shell and never carry a source secret in argv.
type CopyExecutor interface {
	Exec(ctx context.Context, stepID string, c engine.Command, report engine.Reporter) error
}

// Source TLS modes. Plaintext and downgrade modes are deliberately absent.
const (
	SourceTLSVerifyFull = "verify-full"
	SourceTLSVerifyCA   = "verify-ca"
	SourceTLSRequire    = "require"
)

const (
	sourceConnectTimeout = 15 * time.Second
	sourcePreflightLimit = 2 * time.Minute
	archiveListLimit     = 5 * time.Minute
	archiveInspectLimit  = 2 * time.Minute
	maxSourceURLLength   = 64 << 10
	maxPreflightBytes    = 8 << 20
	maxSourceCASize      = 4 << 20
	maxTOCBytes          = 64 << 20
	maxInspectBytes      = 1 << 20
	maxTOCLineBytes      = 4 << 20
)

// CopyRoot is the fixed, copy-owned root for temporary copy workspaces.
// OpenCopyWorkspace creates it mode 0711 so the postgres account can traverse
// to a job directory, while each job directory itself is 0700 and owned by
// postgres.
const CopyRoot = "/var/lib/vaire-copies"

// CopyWorkspaceMarkerName is the private operation-ownership marker in every
// C03 copy workspace. It is written before any source access or archive work.
const CopyWorkspaceMarkerName = "operation"

// CopyWorkspaceLeaseName is the private lifetime lease in every C03 copy
// workspace. The operation and its descendants hold it shared; lifecycle
// pruning takes it exclusive before deleting anything.
const CopyWorkspaceLeaseName = "lease"

// ErrCopyWorkspaceBusy is returned by PruneCopyWorkspace when a live lease
// holder still uses the workspace. It is not an error in a copy operation:
// the workspace must be retained.
var ErrCopyWorkspaceBusy = errors.New("copy workspace is still in use")

// Fixed names inside a copy workspace.
const (
	copyArchiveName   = "source.dump"
	copyPassfileName  = "source.pgpass"
	copyCAName        = "source-ca.pem"
	copyUseListName   = "restore.list"
	copyInspectName   = "inspect.list"
	copyHomeName      = "home"
	copySystemCALabel = "system-ca-bundle"
)

const (
	envPath         = "/usr/bin/env"
	sourcePGAppName = "vaire-copy"
)

// systemCABundlePath is the Debian/Ubuntu system CA bundle used when
// verify-ca is requested without an explicit CA. sslrootcert=system promotes
// the mode to verify-full, so it is not used for verify-ca.
var systemCABundlePath = "/etc/ssl/certs/ca-certificates.crt"

// externalCheckSpace is CheckSpace with a test seam for a short filesystem.
var externalCheckSpace = CheckSpace

// copyPreflightLimitBytes, copyTOCLimitBytes and copyInspectLimitBytes are
// the bounded capture limits with a rootless test seam for oversized-output
// paths.
var (
	copyPreflightLimitBytes = maxPreflightBytes
	copyTOCLimitBytes       = maxTOCBytes
	copyInspectLimitBytes   = maxInspectBytes
)

// copyAfterSourcePassfile is a rootless test seam between the passfile write
// and the optional CA write, so a partial source-access setup can be exercised
// without weakening the production path.
var copyAfterSourcePassfile = func(*CopyWorkspace) {}

// copyAfterJobMkdir and copyAfterJobRootOpen are rootless race-injection seams
// around workspace creation. A test can replace the new directory to prove the
// constructor never chowns or removes a replacement or an outside path.
var (
	copyAfterJobMkdir    = func(string) {}
	copyAfterJobRootOpen = func(string) {}
	copyWorkspaceOwnerOf = workspaceFileOwner
)

// copyToolsDir is the PostgreSQL 18 binaries directory with a test seam.
var copyToolsDir = pgBin

// sourceTool names the fixed client binaries.
func sourceTool(name string) string { return filepath.Join(pgBin, name) }

// SourceSpec is the non-secret-plus-password input for one external source.
// Password is registered for redaction by NewSource and is never put in argv,
// environment, result structures or logs.
type SourceSpec struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"-"`
	TLSMode  string `json:"tls_mode"`
	CACert   []byte `json:"-"`
}

// Source is one validated external PostgreSQL source. Its unexported fields
// are secrets; Format always prints the redacted form.
type Source struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	TLSMode  string `json:"tls_mode"`

	password string
	caPEM    []byte
}

// ParseSourceURL parses a PostgreSQL TCP URL with the process-wide redaction
// registry. The raw URL is registered before parsing so even a malformed URL
// is redacted from later errors.
func ParseSourceURL(raw string) (Source, error) {
	return ParseSourceURLWithRegistry(secrets.Default, raw)
}

// ParseSourceURLWithRegistry is ParseSourceURL with an explicit registry.
func ParseSourceURLWithRegistry(reg *secrets.Registry, raw string) (Source, error) {
	if reg == nil {
		reg = secrets.Default
	}
	original := raw
	reg.Add(original)
	raw = strings.TrimRight(raw, "\r\n")
	reg.Add(raw)
	if raw == "" {
		return Source{}, sourceInputError("invalid_source_url", "the source URL is empty", "Enter a postgres:// or postgresql:// URL.")
	}
	if len(raw) > maxSourceURLLength {
		return Source{}, sourceInputError("invalid_source_url", "the source URL is too long", "Enter a shorter source URL.")
	}
	if strings.ContainsAny(raw, "\x00\r\n") {
		return Source{}, sourceInputError("invalid_source_url", "the source URL contains a line break or control character", "Paste the URL as a single line.")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Never wrap url.Error: its text includes the raw URL.
		return Source{}, sourceInputError("invalid_source_url", "the source URL is not a valid PostgreSQL URL", "Use a postgres:// or postgresql:// URL with one host and one database.")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "postgres" && scheme != "postgresql" {
		return Source{}, sourceInputError("invalid_source_url", "the source URL scheme must be postgres or postgresql", "Use a postgres:// or postgresql:// URL.")
	}
	if u.Opaque != "" || u.Fragment != "" {
		return Source{}, sourceInputError("invalid_source_url", "the source URL must not use an opaque form or fragment", "Use postgres://user:password@host:port/database.")
	}
	if u.User == nil {
		return Source{}, sourceInputError("invalid_source_url", "the source URL must name the source user", "Use postgres://user:password@host:port/database.")
	}
	username := u.User.Username()
	password, hasPassword := u.User.Password()
	if hasPassword {
		reg.Add(password)
	}
	if username == "" {
		return Source{}, sourceInputError("invalid_source_url", "the source URL must name the source user", "Use postgres://user:password@host:port/database.")
	}
	if u.Host == "" {
		return Source{}, sourceInputError("invalid_source_url", "the source URL must use a TCP host, not a Unix socket", "Use postgres://user:password@host:port/database.")
	}
	if strings.HasSuffix(u.Host, ":") {
		return Source{}, sourceInputError("invalid_source_url", "the source URL has an empty port", "Use a port between 1 and 65535, or omit it.")
	}
	host := u.Hostname()
	if host == "" {
		return Source{}, sourceInputError("invalid_source_url", "the source URL has no host", "Use postgres://user:password@host:port/database.")
	}
	port := 5432
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Source{}, sourceInputError("invalid_source_url", "the source URL port must be between 1 and 65535", "Use a port between 1 and 65535.")
		}
		port = n
	}
	escapedPath := u.EscapedPath()
	if !strings.HasPrefix(escapedPath, "/") {
		return Source{}, sourceInputError("invalid_source_url", "the source URL must name one database in its path", "Use postgres://user:password@host:port/database.")
	}
	segment := strings.TrimPrefix(escapedPath, "/")
	if segment == "" || strings.Contains(segment, "/") {
		return Source{}, sourceInputError("invalid_source_url", "the source URL must name exactly one database", "Use postgres://user:password@host:port/database without extra path segments.")
	}
	database, err := url.PathUnescape(segment)
	if err != nil {
		return Source{}, sourceInputError("invalid_source_url", "the source database name is not valid percent-encoding", "Percent-encode the database name once.")
	}
	tlsMode := SourceTLSVerifyFull
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Source{}, sourceInputError("invalid_source_url", "the source URL query is not valid percent-encoding", "Use only sslmode=verify-full, verify-ca or require.")
	}
	for key := range query {
		if key != "sslmode" {
			return Source{}, sourceInputError("invalid_source_url", "the source URL has an unsupported query parameter", "Use only sslmode=verify-full, verify-ca or require; vaire sets the rest itself.")
		}
	}
	if values, ok := query["sslmode"]; ok {
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return Source{}, sourceInputError("invalid_source_url", "the source URL needs exactly one sslmode value", "Use sslmode=verify-full, verify-ca or require.")
		}
		tlsMode = strings.ToLower(values[0])
	}
	return NewSourceWithRegistry(reg, SourceSpec{
		Host:     host,
		Port:     port,
		Database: database,
		User:     username,
		Password: password,
		TLSMode:  tlsMode,
	})
}

// NewSource validates a source supplied as individual fields.
func NewSource(spec SourceSpec) (Source, error) {
	return NewSourceWithRegistry(secrets.Default, spec)
}

// NewSourceWithRegistry is NewSource with an explicit registry. The
// password is registered before validation so it cannot appear in any error.
func NewSourceWithRegistry(reg *secrets.Registry, spec SourceSpec) (Source, error) {
	if reg == nil {
		reg = secrets.Default
	}
	reg.Add(spec.Password)
	port := spec.Port
	if port == 0 {
		port = 5432
	}
	if err := validateSourceHost(spec.Host); err != nil {
		return Source{}, err
	}
	if port < 1 || port > 65535 {
		return Source{}, sourceInputError("invalid_source_url", "the source port must be between 1 and 65535", "Check the source port.")
	}
	if err := validateSourceDatabase(spec.Database); err != nil {
		return Source{}, err
	}
	if err := validateSourceUser(spec.User); err != nil {
		return Source{}, err
	}
	if strings.ContainsAny(spec.Password, "\x00\r\n") {
		return Source{}, sourceInputError("invalid_source_password", "the source password cannot contain a line break or NUL byte", "A PostgreSQL password file cannot carry those characters.")
	}
	tlsMode := strings.ToLower(strings.TrimSpace(spec.TLSMode))
	if tlsMode == "" {
		tlsMode = SourceTLSVerifyFull
	}
	switch tlsMode {
	case SourceTLSVerifyFull, SourceTLSVerifyCA, SourceTLSRequire:
	case "disable", "allow", "prefer":
		return Source{}, sourceInputError("source_tls_weak", "the source URL asks for an unencrypted or downgradeable TLS mode", "Use sslmode=verify-full, verify-ca or require; vaire never falls back to plaintext.")
	default:
		return Source{}, sourceInputError("invalid_source_tls", "the source TLS mode must be verify-full, verify-ca or require", "Use sslmode=verify-full, verify-ca or require.")
	}
	if len(spec.CACert) > maxSourceCASize {
		return Source{}, sourceInputError("invalid_source_ca", "the source CA certificate is too large", "Supply one small PEM CA certificate.")
	}
	if len(spec.CACert) > 0 {
		if tlsMode == SourceTLSRequire {
			return Source{}, sourceInputError("invalid_source_ca", "a source CA certificate cannot be used with sslmode=require", "Use verify-full or verify-ca, or remove the CA.")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(spec.CACert) {
			return Source{}, sourceInputError("invalid_source_ca", "the source CA certificate is not valid PEM", "Supply a PEM-encoded CA certificate.")
		}
	}
	return Source{
		Host:     spec.Host,
		Port:     port,
		Database: spec.Database,
		User:     spec.User,
		TLSMode:  tlsMode,
		password: spec.Password,
		caPEM:    append([]byte(nil), spec.CACert...),
	}, nil
}

// TLSWarning returns the warning to show for a weaker explicit TLS mode.
func (s Source) TLSWarning() (output.Warning, bool) {
	switch s.TLSMode {
	case SourceTLSRequire:
		return output.Warning{
			Code:    "source_tls_unverified",
			Message: "The source connection is encrypted but its certificate is not verified; a network attacker could impersonate the source.",
		}, true
	case SourceTLSVerifyCA:
		return output.Warning{
			Code:    "source_tls_hostname_unverified",
			Message: "The source certificate is verified against the CA, but the source hostname is not checked.",
		}, true
	default:
		return output.Warning{}, false
	}
}

// CACert returns a copy of the explicitly supplied CA certificate.
func (s Source) CACert() []byte { return append([]byte(nil), s.caPEM...) }

// WithCACert returns a validated copy of s with an explicitly supplied CA
// certificate. It is for sources whose URL carries no certificate, such as
// the CLI's separate --source-ca-file secret; the CA content is registered
// for redaction before validation. A source that already has one refuses a
// second certificate rather than silently replacing it.
func (s Source) WithCACert(caPEM []byte) (Source, error) {
	secrets.Add(string(caPEM))
	if len(s.caPEM) > 0 {
		return Source{}, sourceInputError("invalid_source_ca",
			"the source already has a CA certificate",
			"Supply the source CA certificate once.")
	}
	s.caPEM = append([]byte(nil), caPEM...)
	return s.validated()
}

// validated applies the safe defaults and rechecks fields in case a caller
// constructed Source directly instead of using NewSource.
func (s Source) validated() (Source, error) {
	secrets.Add(s.password)
	if s.Port == 0 {
		s.Port = 5432
	}
	if s.TLSMode == "" {
		s.TLSMode = SourceTLSVerifyFull
	}
	s.TLSMode = strings.ToLower(strings.TrimSpace(s.TLSMode))
	if err := validateSourceHost(s.Host); err != nil {
		return Source{}, err
	}
	if s.Port < 1 || s.Port > 65535 {
		return Source{}, sourceInputError("invalid_source_url", "the source port must be between 1 and 65535", "Check the source port.")
	}
	if err := validateSourceDatabase(s.Database); err != nil {
		return Source{}, err
	}
	if err := validateSourceUser(s.User); err != nil {
		return Source{}, err
	}
	if strings.ContainsAny(s.password, "\x00\r\n") {
		return Source{}, sourceInputError("invalid_source_password", "the source password cannot contain a line break or NUL byte", "A PostgreSQL password file cannot carry those characters.")
	}
	switch s.TLSMode {
	case SourceTLSVerifyFull, SourceTLSVerifyCA, SourceTLSRequire:
	case "disable", "allow", "prefer":
		return Source{}, sourceInputError("source_tls_weak", "the source URL asks for an unencrypted or downgradeable TLS mode", "Use sslmode=verify-full, verify-ca or require; vaire never falls back to plaintext.")
	default:
		return Source{}, sourceInputError("invalid_source_tls", "the source TLS mode must be verify-full, verify-ca or require", "Use sslmode=verify-full, verify-ca or require.")
	}
	if len(s.caPEM) > 0 {
		if len(s.caPEM) > maxSourceCASize {
			return Source{}, sourceInputError("invalid_source_ca", "the source CA certificate is too large", "Supply one small PEM CA certificate.")
		}
		if s.TLSMode == SourceTLSRequire {
			return Source{}, sourceInputError("invalid_source_ca", "a source CA certificate cannot be used with sslmode=require", "Use verify-full or verify-ca, or remove the CA.")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(s.caPEM) {
			return Source{}, sourceInputError("invalid_source_ca", "the source CA certificate is not valid PEM", "Supply a PEM-encoded CA certificate.")
		}
	}
	return s, nil
}

// RedactedEndpoint is the source host, port and database without credentials.
func (s Source) RedactedEndpoint() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) + "/" + s.Database
}

// RedactedURL is a loggable URL: it never contains the password.
func (s Source) RedactedURL() string {
	u := &url.URL{
		Scheme:   "postgresql",
		Host:     net.JoinHostPort(s.Host, strconv.Itoa(s.Port)),
		Path:     "/" + s.Database,
		RawQuery: url.Values{"sslmode": []string{s.TLSMode}}.Encode(),
	}
	if s.User != "" {
		u.User = url.User(s.User)
	}
	u.RawPath = "/" + url.PathEscape(s.Database)
	return u.String()
}

// String returns the redacted URL.
func (s Source) String() string { return s.RedactedURL() }

// GoString returns the redacted URL, so %#v cannot print the password.
func (s Source) GoString() string { return s.RedactedURL() }

// Format makes every fmt verb print the redacted URL.
func (s Source) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.RedactedURL())
}

func validateSourceHost(host string) error {
	if host == "" {
		return sourceInputError("invalid_source_host", "the source host is empty", "Use a DNS name or IP address.")
	}
	if len(host) > 253 {
		return sourceInputError("invalid_source_host", "the source host is too long", "Use a DNS name or IP address.")
	}
	for _, r := range host {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return sourceInputError("invalid_source_host", "the source host contains whitespace or a control character", "Use a DNS name or IP address.")
		}
	}
	if strings.ContainsAny(host, `/\ ,@%=?#*`) {
		return sourceInputError("invalid_source_host", "the source host is not a plain DNS name or IP address", "Do not use a Unix socket path, a comma-separated host list or a connection parameter in the host.")
	}
	if strings.Contains(host, ":") {
		if net.ParseIP(host) == nil {
			return sourceInputError("invalid_source_host", "the source IPv6 address is not valid", "Use a valid IPv6 address without a zone.")
		}
		return nil
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	name := strings.TrimSuffix(host, ".")
	if name == "" {
		return sourceInputError("invalid_source_host", "the source host is not valid", "Use a DNS name or IP address.")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || !hostLabelPattern.MatchString(label) {
			return sourceInputError("invalid_source_host", "the source host is not a valid DNS name", "Use a DNS name or IP address.")
		}
	}
	return nil
}

var hostLabelPattern = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?$`)

func validateSourceDatabase(database string) error {
	if database == "" {
		return sourceInputError("invalid_source_url", "the source database is empty", "Name one database in the URL path.")
	}
	if len(database) > 63 {
		return sourceInputError("invalid_source_url", "the source database name is too long", "PostgreSQL database names are at most 63 bytes.")
	}
	if hasControl(database) {
		return sourceInputError("invalid_source_url", "the source database name contains a control character", "Use the database name as PostgreSQL stores it.")
	}
	if strings.TrimSpace(database) != database {
		return sourceInputError("invalid_source_url", "the source database name has leading or trailing whitespace", "Use the database name without surrounding whitespace.")
	}
	if strings.Contains(database, "=") {
		return sourceInputError("invalid_source_url", "the source database name contains =", "vaire does not pass database names that libpq could read as a connection string.")
	}
	if strings.Contains(database, "*") {
		return sourceInputError("invalid_source_url", "the source database name contains a passfile wildcard", "vaire keeps the source password scoped to the exact database and user.")
	}
	lower := strings.ToLower(database)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		return sourceInputError("invalid_source_url", "the source database name looks like a connection URL", "vaire does not pass database names that libpq could read as a connection string.")
	}
	return nil
}

func validateSourceUser(username string) error {
	if username == "" {
		return sourceInputError("invalid_source_url", "the source user is empty", "Name the source role in the URL userinfo.")
	}
	if len(username) > 63 {
		return sourceInputError("invalid_source_url", "the source user name is too long", "PostgreSQL role names are at most 63 bytes.")
	}
	if hasControl(username) {
		return sourceInputError("invalid_source_url", "the source user name contains a control character", "Use the role name as PostgreSQL stores it.")
	}
	if strings.Contains(username, "*") {
		return sourceInputError("invalid_source_url", "the source user name contains a passfile wildcard", "vaire keeps the source password scoped to the exact database and user.")
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// SourceExtension is one extension inventory row.
type SourceExtension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
}

// SourcePreflight is the read-only source metadata an actual copy collects
// before any destination is provisioned.
type SourcePreflight struct {
	ServerVersion     int               `json:"server_version"`
	ServerVersionText string            `json:"server_version_text"`
	Database          string            `json:"database"`
	SizeBytes         int64             `json:"size_bytes"`
	ConnectOK         bool              `json:"connect_ok"`
	CatalogOK         bool              `json:"catalog_ok"`
	TLSMode           string            `json:"tls_mode"`
	Extensions        []SourceExtension `json:"extensions"`
	Warnings          []output.Warning  `json:"warnings,omitempty"`
}

const sourcePreflightSQL = `SET default_transaction_read_only = on;
COPY (
  SELECT current_setting('server_version_num'),
         current_setting('server_version'),
         current_database(),
         pg_database_size(current_database()),
         has_database_privilege(current_user, current_database(), 'CONNECT'),
         has_schema_privilege('pg_catalog', 'USAGE')
) TO STDOUT WITH CSV;
COPY (
  SELECT e.extname, e.extversion, n.nspname
  FROM pg_extension e
  JOIN pg_namespace n ON n.oid = e.extnamespace
  ORDER BY e.extname
) TO STDOUT WITH CSV;
`

// PreflightSource reads the source server version, database size, current
// database and extension inventory. It is read-only and does not promise that
// pg_dump will be able to read every object.
func PreflightSource(ctx context.Context, exec CopyExecutor, stepID string, report engine.Reporter, src Source, ws *CopyWorkspace) (pre SourcePreflight, err error) {
	if exec == nil {
		return SourcePreflight{}, copyInternalError("source_preflight_failed", "no command executor is configured")
	}
	src, err = src.validated()
	if err != nil {
		return SourcePreflight{}, err
	}
	access, err := prepareSourceAccess(ws, src)
	if err != nil {
		return SourcePreflight{}, err
	}
	defer func() { err = joinCleanupError(err, access.remove()) }()
	if err := probeSourceAccess(ctx, exec, stepID, access, src); err != nil {
		return SourcePreflight{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, sourcePreflightLimit)
	defer cancel()
	var out limitedBuffer
	out.limit = copyPreflightLimitBytes
	cmd := sourceCommand(access, sourceTool("psql"), sourcePSQLArgs(src))
	cmd.Stdin = strings.NewReader(sourcePreflightSQL)
	cmd.Stdout = &out
	execErr := exec.Exec(ctx, stepID, cmd, copyReporter(report))
	if out.overflow {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned more preflight data than vaire accepts",
			"Check the source and retry; do not point the copy at an untrusted server.", nil)
	}
	if execErr != nil {
		return SourcePreflight{}, sourcePreflightError(execErr)
	}
	reader := csv.NewReader(strings.NewReader(out.String()))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned invalid preflight data",
			"Check the source version and that nothing modifies the source's catalog.", nil)
	}
	// psql -q suppresses command tags, but tolerate SET/COPY tags if a psql
	// version prints them anyway.
	records = dropPSQLCommandTags(records)
	if len(records) == 0 {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned no preflight data",
			"Check the source connection and that the source user can read the database catalog.", nil)
	}
	server := records[0]
	if len(server) != 6 {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned an unexpected preflight row",
			"Check the source version and that nothing modifies the source's catalog.", nil)
	}
	versionNum, err := strconv.Atoi(server[0])
	if err != nil {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned an invalid server version",
			"Check the source version and that nothing modifies the source's catalog.", nil)
	}
	sizeBytes, err := strconv.ParseInt(server[3], 10, 64)
	if err != nil || sizeBytes < 0 {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned an invalid database size",
			"Retry the preflight, or check the source catalog.", nil)
	}
	connectOK, err := parsePreflightBool(server[4])
	if err != nil {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned an invalid connection permission",
			"Check the source catalog.", nil)
	}
	catalogOK, err := parsePreflightBool(server[5])
	if err != nil {
		return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
			"the source returned an invalid catalog permission",
			"Check the source catalog.", nil)
	}
	rawDatabase := server[2]
	if versionNum >= 190000 {
		return SourcePreflight{}, copyPreconditionError("source_version_unsupported",
			"the source is PostgreSQL "+majorVersionText(versionNum)+"; PostgreSQL 18 tools cannot export a newer major version",
			"Upgrade vaire to a release with newer PostgreSQL tools before copying this source.", nil)
	}
	if versionNum < 90200 {
		return SourcePreflight{}, copyPreconditionError("source_version_unsupported",
			"the source is PostgreSQL "+majorVersionText(versionNum)+"; PostgreSQL 18 tools support 9.2 and newer sources",
			"Upgrade the source to a supported PostgreSQL release before copying it.", nil)
	}
	if rawDatabase != src.Database {
		return SourcePreflight{}, copyPreconditionError("source_database_mismatch",
			"the source reported a different current database than the one requested",
			"Check the source URL database name.", nil)
	}
	pre = SourcePreflight{
		ServerVersion:     versionNum,
		ServerVersionText: server[1],
		Database:          rawDatabase,
		SizeBytes:         sizeBytes,
		ConnectOK:         connectOK,
		CatalogOK:         catalogOK,
		TLSMode:           src.TLSMode,
	}
	if pre.ServerVersionText == "" {
		pre.ServerVersionText = majorVersionText(versionNum)
	}
	for _, record := range records[1:] {
		if len(record) != 3 {
			return SourcePreflight{}, copyPreconditionError("source_preflight_failed",
				"the source returned an unexpected extension row",
				"Check the source extension catalog.", nil)
		}
		ext := SourceExtension{Name: record[0], Version: record[1], Schema: record[2]}
		if err := validateExtensionRow(ext); err != nil {
			return SourcePreflight{}, err
		}
		pre.Extensions = append(pre.Extensions, ext)
	}
	sortSourceExtensions(pre.Extensions)
	if warning, ok := src.TLSWarning(); ok {
		pre.Warnings = append(pre.Warnings, warning)
	}
	if !pre.CatalogOK {
		pre.Warnings = append(pre.Warnings, output.Warning{
			Code:    "source_permissions_unverified",
			Message: "The source user may not be able to read the full database schema; the export can still fail on permissions.",
		})
	}
	return pre, nil
}

func sourcePSQLArgs(src Source) []string {
	return []string{
		"-X", "-q", "--no-password", "-v", "ON_ERROR_STOP=1", "-A", "-t",
		"-h", src.Host,
		"-p", strconv.Itoa(src.Port),
		"-U", src.User,
		"-d", src.Database,
		"-f", "-",
	}
}

func sourceDumpArgs(src Source) []string {
	return []string{
		"-Fc", "--quote-all-identifiers", "--no-password",
		"-h", src.Host,
		"-p", strconv.Itoa(src.Port),
		"-U", src.User,
		"-d", src.Database,
	}
}

func majorVersionText(versionNum int) string {
	if versionNum <= 0 {
		return "an unknown version"
	}
	return strconv.Itoa(versionNum / 10000)
}

func sortSourceExtensions(exts []SourceExtension) {
	for i := 1; i < len(exts); i++ {
		for j := i; j > 0 && exts[j].Name < exts[j-1].Name; j-- {
			exts[j], exts[j-1] = exts[j-1], exts[j]
		}
	}
}

// SourceArchive is a completed private source export.
type SourceArchive struct {
	Size int64      `json:"size_bytes"`
	TOC  ArchiveTOC `json:"-"`
}

// ExportSource writes a custom-format PG18 archive to the private workspace,
// checks its readability, and returns its TOC. The source access files are
// removed before it returns. The archive stays until the caller cleans the
// workspace.
func ExportSource(ctx context.Context, exec CopyExecutor, stepID string, report engine.Reporter, src Source, ws *CopyWorkspace) (archive SourceArchive, err error) {
	if exec == nil {
		return SourceArchive{}, copyInternalError("source_export_failed", "no command executor is configured")
	}
	src, err = src.validated()
	if err != nil {
		return SourceArchive{}, err
	}
	access, err := prepareSourceAccess(ws, src)
	if err != nil {
		return SourceArchive{}, err
	}
	defer func() { err = joinCleanupError(err, access.remove()) }()
	if err := probeSourceAccess(ctx, exec, stepID, access, src); err != nil {
		return SourceArchive{}, err
	}

	f, err := ws.Create(copyArchiveName)
	if err != nil {
		return SourceArchive{}, err
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		_ = f.Close()
		err = joinCleanupError(err, ws.RemoveFile(copyArchiveName))
	}()

	cmd := sourceCommand(access, sourceTool("pg_dump"), sourceDumpArgs(src))
	cmd.Stdout = f
	if err := exec.Exec(ctx, stepID, cmd, copyReporter(report)); err != nil {
		return SourceArchive{}, sourceExportError(err)
	}
	if err := f.Sync(); err != nil {
		return SourceArchive{}, copyPreconditionError("source_export_failed",
			"the source archive could not be written to disk",
			"Check free space and the copy workspace, then retry with a new job.", nil)
	}
	if err := f.Close(); err != nil {
		return SourceArchive{}, copyPreconditionError("source_export_failed",
			"the source archive could not be closed cleanly",
			"Check free space and the copy workspace, then retry with a new job.", nil)
	}

	r, err := ws.Open(copyArchiveName)
	if err != nil {
		return SourceArchive{}, err
	}
	toc, listErr := ReadArchiveTOC(ctx, exec, stepID+"-list", report, r)
	_ = r.Close()
	if listErr != nil {
		return SourceArchive{}, listErr
	}
	if toc.Database != src.Database {
		return SourceArchive{}, copyPreconditionError("archive_database_mismatch",
			"the source archive was not created from the selected database",
			"Retry the copy with a new job; do not restore this archive.", nil)
	}
	info, err := ws.Stat(copyArchiveName)
	if err != nil {
		return SourceArchive{}, err
	}
	if info.Size() == 0 {
		return SourceArchive{}, copyPreconditionError("archive_invalid",
			"the source archive is empty",
			"Retry the copy with a new job.", nil)
	}
	keep = true
	return SourceArchive{Size: info.Size(), TOC: toc}, nil
}

// ArchiveEntry is one safely parsed line of pg_restore --list. The text after
// the numeric header is opaque on purpose: pg_restore does not quote object
// names, so only classification prefixes are trusted.
type ArchiveEntry struct {
	ID       int
	TableOID uint32
	OID      uint32
	Rest     string
}

// ArchiveTOC is the parsed TOC listing.
type ArchiveTOC struct {
	Database string
	Entries  []ArchiveEntry
}

// ReadArchiveTOC lists an archive with PG18 pg_restore. It uses --create so
// the database-definition entries are included in the listing.
func ReadArchiveTOC(ctx context.Context, exec CopyExecutor, stepID string, report engine.Reporter, archive io.Reader) (ArchiveTOC, error) {
	if exec == nil {
		return ArchiveTOC{}, copyInternalError("archive_unreadable", "no command executor is configured")
	}
	ctx, cancel := context.WithTimeout(ctx, archiveListLimit)
	defer cancel()
	var out limitedBuffer
	out.limit = copyTOCLimitBytes
	cmd := engine.Command{
		Name:   sourceTool("pg_restore"),
		Args:   []string{"--list", "--create"},
		Stdin:  archive,
		Stdout: &out,
	}
	err := exec.Exec(ctx, stepID, cmd, copyReporter(report))
	if out.overflow {
		return ArchiveTOC{}, copyPreconditionError("archive_invalid",
			"the source archive TOC is too large for vaire to inspect safely",
			"The source has too many objects for this copy implementation.", nil)
	}
	if err != nil {
		return ArchiveTOC{}, archiveToolError("the source archive could not be read", err)
	}
	return parseArchiveTOCList(out.String())
}

func parseArchiveTOCList(raw string) (ArchiveTOC, error) {
	var toc ArchiveTOC
	seen := map[int]bool{}
	formatCustom := false
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), maxTOCLineBytes)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ";") {
			field := strings.TrimSpace(strings.TrimPrefix(line, ";"))
			switch {
			case strings.HasPrefix(field, "dbname: "):
				toc.Database = strings.TrimSpace(strings.TrimPrefix(field, "dbname: "))
			case field == "Format: CUSTOM":
				formatCustom = true
			}
			continue
		}
		entry, err := parseTOCLine(line)
		if err != nil {
			return ArchiveTOC{}, err
		}
		if seen[entry.ID] {
			return ArchiveTOC{}, copyPreconditionError("archive_invalid",
				"the source archive TOC repeats an entry id",
				"Do not restore this archive; retry the copy with a new job.", nil)
		}
		seen[entry.ID] = true
		toc.Entries = append(toc.Entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return ArchiveTOC{}, copyPreconditionError("archive_invalid",
			"the source archive TOC could not be parsed safely",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	if toc.Database == "" {
		return ArchiveTOC{}, copyPreconditionError("archive_invalid",
			"the source archive has no database name in its TOC",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	if !formatCustom {
		return ArchiveTOC{}, copyPreconditionError("archive_invalid",
			"the source archive is not a PostgreSQL custom-format archive",
			"Retry the copy with the PG18 pg_dump export.", nil)
	}
	if len(toc.Entries) == 0 {
		return ArchiveTOC{}, copyPreconditionError("archive_invalid",
			"the source archive TOC is empty",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	return toc, nil
}

func parseTOCLine(line string) (ArchiveEntry, error) {
	semi := strings.IndexByte(line, ';')
	if semi <= 0 {
		return ArchiveEntry{}, copyPreconditionError("archive_invalid",
			"the source archive TOC contains a line with no entry id",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	id, err := parsePositiveInt(line[:semi])
	if err != nil {
		return ArchiveEntry{}, copyPreconditionError("archive_invalid",
			"the source archive TOC contains an invalid entry id",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	rest := strings.TrimPrefix(line[semi+1:], " ")
	tableOID, oid, tail, err := parseCatalogHeader(rest)
	if err != nil {
		return ArchiveEntry{}, err
	}
	return ArchiveEntry{ID: id, TableOID: tableOID, OID: oid, Rest: tail}, nil
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not numeric")
		}
		n = n*10 + int(r-'0')
		if n <= 0 || n > 1<<30 {
			return 0, errors.New("out of range")
		}
	}
	return n, nil
}

func parseCatalogHeader(s string) (uint32, uint32, string, error) {
	first := strings.IndexByte(s, ' ')
	if first <= 0 {
		return 0, 0, "", copyPreconditionError("archive_invalid",
			"the source archive TOC has an incomplete entry header",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	tableOID, err := strconv.ParseUint(s[:first], 10, 32)
	if err != nil {
		return 0, 0, "", copyPreconditionError("archive_invalid",
			"the source archive TOC has an invalid catalog id",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	rest := s[first+1:]
	second := strings.IndexByte(rest, ' ')
	if second <= 0 {
		return 0, 0, "", copyPreconditionError("archive_invalid",
			"the source archive TOC has an incomplete entry header",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	oid, err := strconv.ParseUint(rest[:second], 10, 32)
	if err != nil {
		return 0, 0, "", copyPreconditionError("archive_invalid",
			"the source archive TOC has an invalid object id",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	return uint32(tableOID), uint32(oid), rest[second+1:], nil
}

// ExtensionMatch is one source extension paired with a destination plan.
type ExtensionMatch struct {
	Source      SourceExtension `json:"source"`
	Destination SourceExtension `json:"destination"`
}

// MatchSourceExtensions requires every source extension to exist in the
// destination plan with exactly the same version and schema. Destination rows
// must describe what the destination will actually install (the node's
// installed extension metadata before provisioning, or the actual pg_extension
// rows after it) -- not a pg_available_extensions default that may differ from
// the installed version. Extra destination extensions are fine (they are
// vaire defaults).
func MatchSourceExtensions(source, destination []SourceExtension) ([]ExtensionMatch, error) {
	want := map[string]SourceExtension{}
	for _, d := range destination {
		if err := validateExtensionRow(d); err != nil {
			return nil, err
		}
		if _, ok := want[d.Name]; ok {
			return nil, copyPreconditionError("extension_incompatible",
				"the destination extension plan repeats "+safeDiag(d.Name),
				"Report this as a vaire bug; do not retry the copy.", nil)
		}
		want[d.Name] = d
	}
	seen := map[string]bool{}
	var matches []ExtensionMatch
	for _, s := range source {
		if err := validateExtensionRow(s); err != nil {
			return nil, err
		}
		if seen[s.Name] {
			return nil, copyPreconditionError("extension_incompatible",
				"the source reports extension "+safeDiag(s.Name)+" more than once",
				"Do not copy from this source until its catalog is consistent.", nil)
		}
		seen[s.Name] = true
		d, ok := want[s.Name]
		if !ok {
			return nil, copyPreconditionError("extension_missing",
				"the source extension "+safeDiag(s.Name)+" is not supported or not available on the destination",
				"Remove the extension from the source, or install a compatible vaire support for it.", nil)
		}
		if s.Version != d.Version {
			return nil, copyPreconditionError("extension_version_incompatible",
				"extension "+safeDiag(s.Name)+" is version "+safeDiag(s.Version)+" on the source but version "+safeDiag(d.Version)+" on the destination",
				"Align the extension versions before copying; vaire does not convert extension versions automatically.", nil)
		}
		if s.Schema != d.Schema {
			return nil, copyPreconditionError("extension_schema_incompatible",
				"extension "+safeDiag(s.Name)+" is in schema "+safeDiag(s.Schema)+" on the source but in schema "+safeDiag(d.Schema)+" on the destination",
				"Install the destination extension in the same schema; vaire does not rewrite extension schemas.", nil)
		}
		matches = append(matches, ExtensionMatch{Source: s, Destination: d})
	}
	return matches, nil
}

func validateExtensionRow(e SourceExtension) error {
	if e.Name == "" || e.Version == "" || e.Schema == "" {
		return copyPreconditionError("extension_incompatible",
			"the extension inventory has an empty name, version or schema",
			"Do not copy from this source until its extension catalog is complete.", nil)
	}
	if hasControl(e.Name) || hasControl(e.Version) || hasControl(e.Schema) {
		return copyPreconditionError("extension_incompatible",
			"the extension inventory contains a control character",
			"Do not copy from this source until its extension catalog is consistent.", nil)
	}
	return nil
}

// SelectionOptions describes the only destination differences the selector
// may make to a source archive. Extensions are the installed source extensions
// that the destination will preprovision, already checked for exact
// name/version/schema compatibility; destination-only defaults must not be
// included. They are also the only extension definitions the archive may
// contain. PostgreSQL's built-in plpgsql is installed on every database but
// modern pg_dump omits its definition, so it is the one extension whose
// definition is optional; see archiveExtensionDefinitionRequired.
type SelectionOptions struct {
	SourceDatabase string
	Extensions     []SourceExtension
}

// Omitted TOC kinds.
const (
	omittedDatabase              = "database"
	omittedDatabaseProperties    = "database_properties"
	omittedDatabaseComment       = "database_comment"
	omittedDatabaseSecurityLabel = "database_security_label"
	omittedExtension             = "extension"
	omittedExtensionComment      = "extension_comment"
)

// OmittedEntry records one deliberately skipped TOC entry.
type OmittedEntry struct {
	ID   int    `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// TOCSelection is a private pg_restore --use-list selection. Lines contain
// only entry ids, never source-controlled names, so a name cannot inject a
// line into the list.
type TOCSelection struct {
	Lines   []string       `json:"-"`
	Omitted []OmittedEntry `json:"omitted"`
}

// WriteList writes the pg_restore --use-list content.
func (s *TOCSelection) WriteList(w io.Writer) error {
	if s == nil {
		return errors.New("nil TOC selection")
	}
	for _, line := range s.Lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// SelectArchiveTOC verifies the archive's database-definition entries and
// preprovisioned extension entries, then returns a selection that omits only
// those verified entries and their comments. Ordinary objects, extension
// configuration data and user comments are preserved.
func SelectArchiveTOC(ctx context.Context, exec CopyExecutor, stepID string, report engine.Reporter, ws *CopyWorkspace, archive SourceArchive, opts SelectionOptions) (*TOCSelection, error) {
	if exec == nil {
		return nil, copyInternalError("archive_selection_failed", "no command executor is configured")
	}
	if ws == nil {
		return nil, copyInternalError("archive_selection_failed", "no copy workspace is configured")
	}
	if err := validateSelectionOptions(opts); err != nil {
		return nil, err
	}
	if archive.TOC.Database != opts.SourceDatabase {
		return nil, copyPreconditionError("archive_database_mismatch",
			"the source archive database does not match the selected source database",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	extNames := map[string]bool{}
	extPlan := map[string]SourceExtension{}
	for _, ext := range opts.Extensions {
		extNames[ext.Name] = true
		extPlan[ext.Name] = ext
	}

	var sel TOCSelection
	dbSeen, dbPropsSeen, dbCommentSeen, dbLabelsSeen := 0, 0, 0, 0
	extDefSeen := map[string]bool{}
	extCommentSeen := map[string]bool{}
	for _, entry := range archive.TOC.Entries {
		kind, after, candidate, err := classifyTOCEntry(entry)
		if err != nil {
			return nil, err
		}
		if !candidate {
			sel.Lines = append(sel.Lines, tocIDLine(entry.ID))
			continue
		}
		switch kind {
		case omittedDatabase:
			stmt, err := inspectTOCEntry(ctx, exec, stepID, report, ws, entry.ID)
			if err != nil {
				return nil, err
			}
			name, ok := parseCreateDatabase(stmt)
			if !ok || name != opts.SourceDatabase {
				return nil, copyPreconditionError("archive_invalid",
					"the source archive's database definition does not match the selected source database",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			dbSeen++
			if dbSeen > 1 {
				return nil, copyPreconditionError("archive_ambiguous",
					"the source archive has more than one database definition",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			sel.Omitted = append(sel.Omitted, OmittedEntry{ID: entry.ID, Kind: omittedDatabase, Name: opts.SourceDatabase})
		case omittedDatabaseProperties:
			stmt, err := inspectTOCEntry(ctx, exec, stepID, report, ws, entry.ID)
			if err != nil {
				return nil, err
			}
			name, ok := parseAlterDatabase(stmt)
			if !ok || name != opts.SourceDatabase {
				return nil, copyPreconditionError("archive_invalid",
					"the source archive's database properties do not match the selected source database",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			dbPropsSeen++
			if dbPropsSeen > 1 {
				return nil, copyPreconditionError("archive_ambiguous",
					"the source archive has more than one database-properties entry",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			sel.Omitted = append(sel.Omitted, OmittedEntry{ID: entry.ID, Kind: omittedDatabaseProperties, Name: opts.SourceDatabase})
		case omittedExtension:
			name, ok := extensionDefinitionName(after)
			if !ok || !extNames[name] {
				return nil, copyPreconditionError("archive_unexpected_extension",
					"the source archive contains an extension definition that was not in the approved destination extension plan",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			stmt, err := inspectTOCEntry(ctx, exec, stepID, report, ws, entry.ID)
			if err != nil {
				return nil, err
			}
			got, schema, ok := parseCreateExtension(stmt)
			if !ok || got != name || schema != extPlan[name].Schema {
				return nil, copyPreconditionError("archive_invalid",
					"the source archive's extension definition does not match its approved name or schema",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			if extDefSeen[name] {
				return nil, copyPreconditionError("archive_ambiguous",
					"the source archive has more than one definition for extension "+safeDiag(name),
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			extDefSeen[name] = true
			sel.Omitted = append(sel.Omitted, OmittedEntry{ID: entry.ID, Kind: omittedExtension, Name: name})
		case "comment":
			stmt, err := inspectTOCEntry(ctx, exec, stepID, report, ws, entry.ID)
			if err != nil {
				return nil, err
			}
			if name, ok := parseCommentOnExtension(stmt); ok {
				if !extNames[name] {
					return nil, copyPreconditionError("archive_ambiguous",
						"the source archive has an extension comment for an extension outside the approved plan",
						"Do not restore this archive; retry the copy with a new job.", nil)
				}
				if extCommentSeen[name] {
					return nil, copyPreconditionError("archive_ambiguous",
						"the source archive has more than one comment for extension "+safeDiag(name),
						"Do not restore this archive; retry the copy with a new job.", nil)
				}
				extCommentSeen[name] = true
				sel.Omitted = append(sel.Omitted, OmittedEntry{ID: entry.ID, Kind: omittedExtensionComment, Name: name})
				continue
			}
			if name, ok := parseCommentOnDatabase(stmt); ok {
				if name != opts.SourceDatabase {
					return nil, copyPreconditionError("archive_ambiguous",
						"the source archive has a database comment for a different database",
						"Do not restore this archive; retry the copy with a new job.", nil)
				}
				dbCommentSeen++
				if dbCommentSeen > 1 {
					return nil, copyPreconditionError("archive_ambiguous",
						"the source archive has more than one database comment",
						"Do not restore this archive; retry the copy with a new job.", nil)
				}
				sel.Omitted = append(sel.Omitted, OmittedEntry{ID: entry.ID, Kind: omittedDatabaseComment, Name: name})
				continue
			}
			if !strings.HasPrefix(stmt, "COMMENT ON ") {
				return nil, copyPreconditionError("archive_invalid",
					"the source archive's comment entry does not contain a COMMENT statement",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			sel.Lines = append(sel.Lines, tocIDLine(entry.ID))
		case "security_label":
			stmt, err := inspectTOCEntry(ctx, exec, stepID, report, ws, entry.ID)
			if err != nil {
				return nil, err
			}
			if name, ok := parseSecurityLabelOnDatabase(stmt); ok {
				if name != opts.SourceDatabase {
					return nil, copyPreconditionError("archive_ambiguous",
						"the source archive has a database security label for a different database",
						"Do not restore this archive; retry the copy with a new job.", nil)
				}
				dbLabelsSeen++
				if dbLabelsSeen > 1 {
					return nil, copyPreconditionError("archive_ambiguous",
						"the source archive has more than one database security-label entry",
						"Do not restore this archive; retry the copy with a new job.", nil)
				}
				sel.Omitted = append(sel.Omitted, OmittedEntry{ID: entry.ID, Kind: omittedDatabaseSecurityLabel, Name: name})
				continue
			}
			if !strings.HasPrefix(stmt, "SECURITY LABEL ") {
				return nil, copyPreconditionError("archive_invalid",
					"the source archive's security-label entry does not contain a SECURITY LABEL statement",
					"Do not restore this archive; retry the copy with a new job.", nil)
			}
			sel.Lines = append(sel.Lines, tocIDLine(entry.ID))
		}
	}
	if dbSeen != 1 {
		return nil, copyPreconditionError("archive_invalid",
			"the source archive does not have exactly one database definition",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	for name := range extNames {
		if !extDefSeen[name] && archiveExtensionDefinitionRequired(name) {
			return nil, copyPreconditionError("archive_invalid",
				"the source archive does not contain the approved extension definition for "+safeDiag(name),
				"Do not restore this archive; retry the copy with a new job.", nil)
		}
	}
	return &sel, nil
}

// archiveExtensionDefinitionRequired reports whether an installed extension is
// expected to have its own creation entry in a pg_dump archive. plpgsql is
// built into PostgreSQL and modern pg_dump omits its definition; its installed
// catalog row is still checked for exact version/schema compatibility, and a
// definition that is present is still verified and omitted. No other
// extension may silently lack a definition.
func archiveExtensionDefinitionRequired(name string) bool {
	return name != "plpgsql"
}

func validateSelectionOptions(opts SelectionOptions) error {
	if opts.SourceDatabase == "" {
		return copyInternalError("archive_selection_failed", "the source database name is empty")
	}
	if err := validateSourceDatabase(opts.SourceDatabase); err != nil {
		return copyInternalError("archive_selection_failed", "the source database name is not safe")
	}
	seen := map[string]bool{}
	for _, ext := range opts.Extensions {
		if err := validateExtensionRow(ext); err != nil {
			return err
		}
		if seen[ext.Name] {
			return copyInternalError("archive_selection_failed", "the approved extension plan repeats an extension")
		}
		seen[ext.Name] = true
	}
	return nil
}

// classifyTOCEntry recognises only the entry shapes the selector may touch.
// Everything else is kept untouched. after is the text after the description
// and the leading "- " schema field. Global COMMENT and SECURITY LABEL entries
// are inspected only when their shape can be an extension/database entry;
// their SQL target decides, because pg_restore does not quote TOC tags.
func classifyTOCEntry(entry ArchiveEntry) (kind, after string, candidate bool, err error) {
	rest := entry.Rest
	switch {
	case strings.HasPrefix(rest, "DATABASE PROPERTIES "):
		kind, rest = omittedDatabaseProperties, rest[len("DATABASE PROPERTIES "):]
	case strings.HasPrefix(rest, "DATABASE "):
		kind, rest = omittedDatabase, rest[len("DATABASE "):]
	case strings.HasPrefix(rest, "EXTENSION "):
		kind, rest = omittedExtension, rest[len("EXTENSION "):]
	case strings.HasPrefix(rest, "COMMENT "):
		kind, rest = "comment", rest[len("COMMENT "):]
	case strings.HasPrefix(rest, "SECURITY LABEL "):
		kind, rest = "security_label", rest[len("SECURITY LABEL "):]
	default:
		return "", "", false, nil
	}
	if !strings.HasPrefix(rest, "- ") {
		// A COMMENT or SECURITY LABEL on a namespaced object is ordinary
		// user data and must be kept.
		return "", "", false, nil
	}
	after = rest[2:]
	if hasTOCControl(after) {
		return "", "", false, copyPreconditionError("archive_invalid",
			"the source archive TOC contains a control character in an entry vaire must inspect",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	switch kind {
	case "comment":
		// Extension comments have an empty owner (a trailing space);
		// database comments start with the DATABASE label. Other global
		// comments are ordinary user data and are kept without inspection.
		if strings.HasPrefix(after, "DATABASE ") || strings.HasSuffix(after, " ") {
			return kind, after, true, nil
		}
		return "", "", false, nil
	case "security_label":
		// Only a database security label is removed with the database
		// definition. Extension and other global labels are kept.
		if strings.HasPrefix(after, "DATABASE ") {
			return kind, after, true, nil
		}
		return "", "", false, nil
	default:
		return kind, after, true, nil
	}
}

func hasTOCControl(s string) bool {
	return hasControl(s)
}

func extensionDefinitionName(after string) (string, bool) {
	if !strings.HasSuffix(after, " ") {
		return "", false
	}
	name := strings.TrimSuffix(after, " ")
	if name == "" {
		return "", false
	}
	return name, true
}

func tocIDLine(id int) string { return strconv.Itoa(id) + ";" }

func inspectTOCEntry(ctx context.Context, exec CopyExecutor, stepID string, report engine.Reporter, ws *CopyWorkspace, id int) (stmt string, err error) {
	if err := ws.WriteFile(copyInspectName, []byte(strconv.Itoa(id)+";\n")); err != nil {
		return "", err
	}
	defer func() { err = joinCleanupError(err, ws.RemoveFile(copyInspectName)) }()
	archive, err := ws.Open(copyArchiveName)
	if err != nil {
		return "", err
	}
	defer func() { _ = archive.Close() }()
	ctx, cancel := context.WithTimeout(ctx, archiveInspectLimit)
	defer cancel()
	var out limitedBuffer
	out.limit = copyInspectLimitBytes
	cmd := engine.Command{
		Name:   sourceTool("pg_restore"),
		Args:   []string{"--create", "--use-list=" + ws.FilePath(copyInspectName), "--file=-"},
		Stdin:  archive,
		Stdout: &out,
	}
	execErr := exec.Exec(ctx, stepID+"-inspect", cmd, copyReporter(report))
	if out.overflow {
		return "", copyPreconditionError("archive_invalid",
			"a source archive TOC entry is too large for vaire to inspect safely",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	if execErr != nil {
		return "", archiveToolError("the source archive TOC entry could not be inspected", execErr)
	}
	stmt = firstSQLStatement(out.String())
	if stmt == "" {
		return "", copyPreconditionError("archive_invalid",
			"a source archive TOC entry contains no SQL statement",
			"Do not restore this archive; retry the copy with a new job.", nil)
	}
	return stmt, nil
}

func firstSQLStatement(raw string) string {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), maxTOCLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "",
			strings.HasPrefix(line, "--"),
			strings.HasPrefix(line, "\\"),
			strings.HasPrefix(line, "SET "),
			strings.HasPrefix(line, "SELECT pg_catalog.set_config"),
			strings.HasPrefix(line, "SELECT pg_catalog.setval"),
			line == "BEGIN;",
			line == "COMMIT;":
			continue
		default:
			return line
		}
	}
	return ""
}

// parseCreateDatabase reads CREATE DATABASE <ident> from the first statement.
func parseCreateDatabase(stmt string) (string, bool) {
	const prefix = "CREATE DATABASE "
	if !strings.HasPrefix(stmt, prefix) {
		return "", false
	}
	return parseSQLIdentifier(strings.TrimPrefix(stmt, prefix))
}

// parseAlterDatabase reads ALTER DATABASE <ident> from the first statement.
func parseAlterDatabase(stmt string) (string, bool) {
	const prefix = "ALTER DATABASE "
	if !strings.HasPrefix(stmt, prefix) {
		return "", false
	}
	return parseSQLIdentifier(strings.TrimPrefix(stmt, prefix))
}

// parseCreateExtension handles CREATE EXTENSION [IF NOT EXISTS] <ident> and
// returns the optional WITH SCHEMA identifier. PG18 pg_dump always emits it.
func parseCreateExtension(stmt string) (name, schema string, ok bool) {
	const prefix = "CREATE EXTENSION "
	if !strings.HasPrefix(stmt, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(stmt, prefix), "IF NOT EXISTS ")
	name, rest, ok = parseSQLIdentifierPrefix(rest)
	if !ok {
		return "", "", false
	}
	if !strings.HasPrefix(rest, "WITH SCHEMA ") {
		return "", "", false
	}
	schema, _, ok = parseSQLIdentifierPrefix(strings.TrimPrefix(rest, "WITH SCHEMA "))
	if !ok || schema == "" {
		return "", "", false
	}
	return name, schema, true
}

func parseCommentOnExtension(stmt string) (string, bool) {
	const prefix = "COMMENT ON EXTENSION "
	if !strings.HasPrefix(stmt, prefix) {
		return "", false
	}
	return parseSQLIdentifier(strings.TrimPrefix(stmt, prefix))
}

func parseCommentOnDatabase(stmt string) (string, bool) {
	const prefix = "COMMENT ON DATABASE "
	if !strings.HasPrefix(stmt, prefix) {
		return "", false
	}
	return parseSQLIdentifier(strings.TrimPrefix(stmt, prefix))
}

func parseSecurityLabelOnDatabase(stmt string) (string, bool) {
	if !strings.HasPrefix(stmt, "SECURITY LABEL ") {
		return "", false
	}
	idx := strings.LastIndex(stmt, " ON DATABASE ")
	if idx < 0 {
		return "", false
	}
	return parseSQLIdentifier(stmt[idx+len(" ON DATABASE "):])
}

// parseSQLIdentifier reads one PostgreSQL identifier and returns its value.
func parseSQLIdentifier(s string) (string, bool) {
	name, _, ok := parseSQLIdentifierPrefix(s)
	return name, ok
}

// parseSQLIdentifierPrefix reads one PostgreSQL identifier and also returns
// the remaining text. It accepts the always-quoted form produced by
// --quote-all-identifiers and the unquoted simple form.
func parseSQLIdentifierPrefix(s string) (string, string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if s[0] != '"' {
		end := strings.IndexAny(s, " \t;")
		if end < 0 {
			return "", "", false
		}
		name := s[:end]
		if name == "" {
			return "", "", false
		}
		return name, strings.TrimSpace(s[end:]), true
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		if i+1 >= len(s) {
			return "", "", false
		}
		if next := s[i+1]; next != ' ' && next != '\t' && next != ';' {
			return "", "", false
		}
		return b.String(), strings.TrimSpace(s[i+1:]), true
	}
	return "", "", false
}

// StrictRestoreOptions describes one restore into an explicitly selected,
// already provisioned vaire database.
type StrictRestoreOptions struct {
	Local     pg.Local
	Target    string
	Owner     string
	Selection *TOCSelection
}

// RestoreStrict runs PG18 pg_restore with the owner role and no clean/create
// or superuser fallback, over the archive on stdin and a private use-list. A
// non-zero exit is an error; pg_restore output is never downgraded to warnings.
func RestoreStrict(ctx context.Context, exec CopyExecutor, stepID string, report engine.Reporter, ws *CopyWorkspace, opts StrictRestoreOptions) (err error) {
	if exec == nil {
		return copyInternalError("restore_failed", "no command executor is configured")
	}
	if ws == nil {
		return copyInternalError("restore_failed", "no copy workspace is configured")
	}
	if !config.DatabasePattern.MatchString(opts.Target) {
		return copyInternalError("restore_failed", "the destination database name is not valid")
	}
	if err := validateRestoreOwner(opts.Owner); err != nil {
		return err
	}
	if opts.Local.User != "" && opts.Local.User != "postgres" {
		return copyInternalError("restore_failed", "the strict loader must connect as the postgres local role")
	}
	if opts.Local.AsRoot {
		return copyInternalError("restore_failed", "the strict loader cannot run psql as root")
	}
	if opts.Local.Port < 0 || opts.Local.Port > 65535 {
		return copyInternalError("restore_failed", "the destination cluster port is not valid")
	}
	if opts.Local.Host != "" && !filepath.IsAbs(opts.Local.Host) {
		return copyInternalError("restore_failed", "the strict loader requires the destination cluster's local Unix socket")
	}
	if opts.Selection == nil {
		return copyInternalError("restore_failed", "the restore selection is empty")
	}
	var list bytes.Buffer
	if err := opts.Selection.WriteList(&list); err != nil {
		return copyInternalError("restore_failed", "the restore selection could not be written")
	}
	if !validUseList(list.Bytes()) {
		return copyInternalError("restore_failed", "the restore selection contains an unsafe line")
	}
	if err := ws.WriteFile(copyUseListName, list.Bytes()); err != nil {
		return err
	}
	defer func() { err = joinCleanupError(err, ws.RemoveFile(copyUseListName)) }()

	archive, err := ws.Open(copyArchiveName)
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close() }()

	args := []string{
		"-u", "postgres", "--", sourceTool("pg_restore"),
		"--no-owner", "--no-privileges", "--no-tablespaces", "--exit-on-error", "--no-password",
		"--role=" + opts.Owner,
		"--use-list=" + ws.FilePath(copyUseListName),
	}
	args = append(args, connArgs(opts.Local)...)
	args = append(args, "-d", opts.Target)
	cmd := engine.Command{Name: "runuser", Args: args, Stdin: archive}
	if err := exec.Exec(ctx, stepID, cmd, copyReporter(report)); err != nil {
		return restoreError(err)
	}
	return nil
}

var useListLinePattern = regexp.MustCompile(`^[1-9][0-9]*;$`)

func validUseList(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if !useListLinePattern.MatchString(line) {
			return false
		}
	}
	return true
}

func validateRestoreOwner(owner string) error {
	if owner == "" || len(owner) > 63 || hasControl(owner) {
		return copyInternalError("restore_failed", "the destination owner role is not valid")
	}
	if owner == "postgres" || pg.IsVaireRole(owner) {
		return copyInternalError("restore_failed", "the strict loader must run as the destination database owner, not an administrative role")
	}
	return nil
}

// CheckCopySpace checks the workspace and data-directory filesystems for the
// source-size estimate. It is advisory beyond the host safety floors, which
// CheckSpace enforces.
func CheckCopySpace(ws *CopyWorkspace, dataDir string, sourceSize int64) ([]SpaceNeed, error) {
	if ws == nil || dataDir == "" {
		return nil, copyInternalError("copy_space_check_failed", "the copy space check is missing a path")
	}
	need := CopyNeed(sourceSize)
	return externalCheckSpace([]SpaceNeed{
		{Path: ws.Dir, Need: need, Purpose: "temporary source archive"},
		{Path: dataDir, Need: need, Purpose: "imported database"},
	})
}

// CheckPG18Tools checks the fixed PostgreSQL 18 client binaries.
func CheckPG18Tools() error { return CheckPG18ToolsAt(copyToolsDir) }

// CheckPG18ToolsAt checks pg_dump, pg_restore and psql under dir.
func CheckPG18ToolsAt(dir string) error {
	if dir == "" {
		return copyMissingToolError("")
	}
	for _, name := range []string{"pg_dump", "pg_restore", "psql"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return copyMissingToolError(path)
		}
	}
	return nil
}

// CopyWorkspace is one private copy job directory. Its root handle confines
// every file operation to the job directory.
type CopyWorkspace struct {
	Dir      string
	owner    WorkspaceOwner
	root     *os.Root
	parent   *os.Root
	jobID    string
	identity fs.FileInfo
}

// WorkspaceOwner is the account that must be able to read source access files
// and write the archive while the PostgreSQL client runs as postgres.
type WorkspaceOwner struct {
	UID int
	GID int
}

// copyEffectiveUID and copyEffectiveGID are seams so rootless tests can model
// an effective root process and its ancestry rules. Production uses the
// process's real effective account.
var (
	copyEffectiveUID = os.Geteuid
	copyEffectiveGID = os.Getegid
)

// CurrentWorkspaceOwner returns the current effective account, for tests and
// developer runs.
func CurrentWorkspaceOwner() WorkspaceOwner {
	return WorkspaceOwner{UID: copyEffectiveUID(), GID: copyEffectiveGID()}
}

var copyJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// CopyWorkspaceJobID reports whether name is a valid copy workspace directory
// name. It is exported so the node's lifecycle pruning accepts exactly the
// names OpenCopyWorkspace can create.
func CopyWorkspaceJobID(name string) bool { return copyJobIDPattern.MatchString(name) }

// PostgresWorkspaceOwner is the account that owns copy workspaces: the
// postgres OS account whose clients read the source access files and write the
// archive.
func PostgresWorkspaceOwner() (WorkspaceOwner, error) {
	u, err := user.Lookup("postgres")
	if err != nil {
		return WorkspaceOwner{}, copyInternalError("copy_workspace_failed", "the postgres account is required to create a copy workspace")
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return WorkspaceOwner{}, copyInternalError("copy_workspace_failed", "the postgres account has an invalid uid")
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return WorkspaceOwner{}, copyInternalError("copy_workspace_failed", "the postgres account has an invalid gid")
	}
	return WorkspaceOwner{UID: uid, GID: gid}, nil
}

// workspaceFileOwner returns a file's numeric owner. It is a seam so rootless
// tests can model a foreign-owned copy root.
func workspaceFileOwner(info fs.FileInfo) (uid, gid int, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}

// verifyCopyAncestors rejects a copy root reached through a symlink or an
// ancestor that an unrelated account could use to replace an entry. The
// directory owner controls replacement even in a sticky directory, so the
// owner must always be root or the effective vaire process, including when
// vaire runs as root. A sticky directory (such as a root-owned /tmp) is
// accepted once its owner is trusted; its write bits then only let unrelated
// users create their own entries. A non-sticky directory must not be
// other-writable, and group-writable only to the process's own group.
func verifyCopyAncestors(rootDir string, owner WorkspaceOwner) error {
	euid, egid := copyEffectiveUID(), copyEffectiveGID()
	for parent := filepath.Dir(rootDir); ; {
		info, err := os.Lstat(parent)
		if errors.Is(err, fs.ErrNotExist) {
			return copyWorkspaceUnsafe("a copy root ancestor is missing")
		}
		if err != nil {
			return copyWorkspaceUnsafe("a copy root ancestor could not be inspected")
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return copyWorkspaceUnsafe("a copy root ancestor is not a plain directory")
		}
		perm := info.Mode().Perm()
		uid, gid, owned := copyWorkspaceOwnerOf(info)
		switch {
		case !owned:
			return copyWorkspaceUnsafe("a copy root ancestor owner could not be verified")
		case uid != 0 && uid != euid:
			return copyWorkspaceUnsafe("a copy root ancestor is owned by an unrelated user")
		case info.Mode()&fs.ModeSticky != 0:
			// A trusted owner controls replacement in its own sticky
			// directory; other users can only add entries they own.
		case perm&0o002 != 0:
			return copyWorkspaceUnsafe("a copy root ancestor is writable by other users")
		case perm&0o020 != 0 && (uid != euid || gid != egid):
			return copyWorkspaceUnsafe("a copy root ancestor is writable by another group")
		}
		if !workspaceModeAllows(perm, uid, gid, owner, 0o100) {
			return copyWorkspaceAccessDenied("ancestor", "directory")
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil
		}
		parent = next
	}
}

// openCopyRoot verifies the copy root and its ancestors, creates the root
// privately when requested, and returns a handle pinned to the verified
// directory. The returned found flag is false only for a missing root when
// create is false.
func openCopyRoot(rootDir string, owner WorkspaceOwner, create bool) (root *os.Root, found bool, err error) {
	if rootDir == "" || !filepath.IsAbs(rootDir) {
		return nil, false, copyInternalError("copy_workspace_unsafe", "the copy root must be an absolute path")
	}
	rootDir = filepath.Clean(rootDir)
	parentDir := filepath.Dir(rootDir)
	name := filepath.Base(rootDir)
	if name == "." || name == string(filepath.Separator) {
		return nil, false, copyWorkspaceUnsafe("the copy root is not a named directory")
	}
	if err := verifyCopyAncestors(rootDir, owner); err != nil {
		return nil, false, err
	}
	parentInfo, err := os.Lstat(parentDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, copyWorkspaceUnsafe("the copy root parent is missing")
	}
	if err != nil || parentInfo.Mode()&fs.ModeSymlink != 0 || !parentInfo.IsDir() {
		return nil, false, copyWorkspaceUnsafe("the copy root parent is not a plain directory")
	}
	parentRoot, err := os.OpenRoot(parentDir)
	if err != nil {
		return nil, false, copyWorkspaceUnsafe("the copy root parent could not be opened")
	}
	defer func() { _ = parentRoot.Close() }()
	openedParent, err := parentRoot.Stat(".")
	if err != nil || !openedParent.IsDir() || !os.SameFile(parentInfo, openedParent) {
		return nil, false, copyWorkspaceUnsafe("the copy root parent changed while it was being inspected")
	}
	created := false
	entry, err := parentRoot.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, false, nil
		}
		if err := parentRoot.Mkdir(name, 0o711); err != nil && !errors.Is(err, fs.ErrExist) { //nolint:gosec // G301: traversal for postgres, no write for group/others.
			return nil, false, copyWorkspaceError("the copy root could not be created")
		} else {
			created = err == nil
		}
		entry, err = parentRoot.Lstat(name)
		if err != nil {
			return nil, false, copyWorkspaceUnsafe("the copy root could not be inspected after creation")
		}
	} else if err != nil {
		return nil, false, copyWorkspaceUnsafe("the copy root could not be inspected")
	}
	if entry.Mode()&fs.ModeSymlink != 0 || !entry.IsDir() {
		return nil, false, copyWorkspaceUnsafe("the copy root is not a plain directory")
	}
	root, err = parentRoot.OpenRoot(name)
	if err != nil {
		return nil, false, copyWorkspaceUnsafe("the copy root could not be opened")
	}
	cleanup := func() {
		if root != nil {
			_ = root.Close()
			root = nil
		}
	}
	opened, err := root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(entry, opened) {
		cleanup()
		return nil, false, copyWorkspaceUnsafe("the copy root changed while it was being inspected")
	}
	if created {
		if err := root.Chmod(".", 0o711); err != nil { //nolint:gosec // G302: traversal for postgres, no write for group/others.
			cleanup()
			return nil, false, copyWorkspaceError("the copy root permissions could not be set")
		}
		opened, err = root.Stat(".")
		if err != nil {
			cleanup()
			return nil, false, copyWorkspaceUnsafe("the copy root could not be inspected after setting permissions")
		}
	}
	if uid, _, ok := copyWorkspaceOwnerOf(opened); !ok || uid != copyEffectiveUID() {
		cleanup()
		return nil, false, copyWorkspaceUnsafe("the copy root is not owned by the vaire process")
	}
	perm := opened.Mode().Perm()
	if perm&0o022 != 0 {
		cleanup()
		return nil, false, copyWorkspaceUnsafe("the copy root is writable by group or others")
	}
	if rootDir == CopyRoot && perm != 0o711 {
		cleanup()
		return nil, false, copyWorkspaceUnsafe("the copy root does not have mode 0711")
	}
	if !workspaceModeAllows(perm, copyEffectiveUID(), copyEffectiveGID(), owner, 0o100) {
		cleanup()
		return nil, false, copyWorkspaceAccessDenied("ancestor", "copy-root")
	}
	return root, true, nil
}

// OpenCopyWorkspace creates a new workspace under CopyRoot for jobID. It never
// reuses an existing directory.
func OpenCopyWorkspace(jobID string) (*CopyWorkspace, error) {
	owner, err := PostgresWorkspaceOwner()
	if err != nil {
		return nil, err
	}
	return NewCopyWorkspace(CopyRoot, jobID, owner, nil)
}

// PruneCopyWorkspace removes one copy workspace only when it is still the
// copy-owned directory for jobID. It is the abrupt-death cleanup seam, so it
// is deliberately stricter than CopyWorkspace.Remove:
//
//   - rootDir must be an existing plain directory owned by the effective user
//     and not writable by group or others; ownership or permission evidence
//     that could let another user replace entries fails closed;
//   - jobID must name a plain directory owned by owner with mode 0700; a
//     symlink, another owner, another mode or a missing directory is refused
//     or reported as already gone;
//   - the operation marker must name the exact job id, lease device:inode and
//     random token; the lease opened through the verified job handle must
//     match both identity and content;
//   - a nonblocking exclusive flock on the verified lease must be acquired
//     and held through removal; a live shared holder returns
//     ErrCopyWorkspaceBusy and the workspace is retained;
//   - the removal runs through os.Root handles opened on rootDir and jobID, so
//     no name can traverse or follow a link outside the copy root, and the
//     opened job directory's identity is compared with the directory entry
//     before anything is removed.
//
// It never removes a database, a data directory outside the copy root, or a
// workspace whose ownership it cannot verify. The returned removed flag is
// false with a nil error only when the directory was already absent.
func PruneCopyWorkspace(rootDir, jobID string, owner WorkspaceOwner) (removed bool, err error) {
	root, jobRoot, _, lease, found, err := openVerifiedCopyWorkspace(rootDir, jobID, owner)
	if err != nil || !found {
		return false, err
	}
	defer func() { _ = jobRoot.Close() }()
	defer func() { _ = root.Close() }()

	leaseInfo, err := jobRoot.Lstat(CopyWorkspaceLeaseName)
	if err != nil || leaseInfo.Mode()&fs.ModeSymlink != 0 || !leaseInfo.Mode().IsRegular() || leaseInfo.Size() > 1024 {
		return false, copyWorkspaceUnsafe("the copy workspace has no verified lifetime lease")
	}
	leaseFile, err := jobRoot.OpenFile(CopyWorkspaceLeaseName, os.O_RDWR, 0)
	if err != nil {
		return false, copyWorkspaceUnsafe("the copy workspace lease could not be opened")
	}
	defer func() { _ = leaseFile.Close() }()
	openedInfo, err := leaseFile.Stat()
	if err != nil || !os.SameFile(leaseInfo, openedInfo) {
		return false, copyWorkspaceUnsafe("the copy workspace lease changed while it was being inspected")
	}
	identity, ok := copyLeaseIdentityOf(openedInfo)
	if !ok || identity != lease.identity {
		return false, copyWorkspaceUnsafe("the copy workspace lease identity does not match its marker")
	}
	token, err := io.ReadAll(io.LimitReader(leaseFile, 1025))
	if err != nil || len(token) > 1024 || strings.TrimSuffix(string(token), "\n") != lease.token {
		return false, copyWorkspaceUnsafe("the copy workspace lease content does not match its marker")
	}
	if err := syscall.Flock(int(leaseFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, ErrCopyWorkspaceBusy
		}
		return false, copyWorkspaceUnsafe("the copy workspace lease could not be locked")
	}
	if err := root.RemoveAll(jobID); err != nil {
		return false, copyWorkspaceUnsafe("the copy workspace could not be removed")
	}
	return true, nil
}

// CopyWorkspaceOperation reads a copy workspace's verified operation marker.
// It is the read-only counterpart of PruneCopyWorkspace, used to recognize a
// stale preflight workspace that has no job record yet.
func CopyWorkspaceOperation(rootDir, jobID string, owner WorkspaceOwner) (string, error) {
	root, jobRoot, kind, _, found, err := openVerifiedCopyWorkspace(rootDir, jobID, owner)
	if err != nil {
		return "", err
	}
	if !found {
		return "", copyInternalError("copy_workspace_gone", "the copy workspace is gone")
	}
	_ = jobRoot.Close()
	_ = root.Close()
	return kind, nil
}

// CopyWorkspaceLeaseIdentity returns the device:inode identity a copy
// workspace marker records for its lease file.
func CopyWorkspaceLeaseIdentity(f *os.File) (string, error) {
	if f == nil {
		return "", copyInternalError("copy_workspace_failed", "the copy workspace lease is missing")
	}
	info, err := f.Stat()
	if err != nil {
		return "", copyWorkspaceUnsafe("the copy workspace lease could not be inspected")
	}
	identity, ok := copyLeaseIdentityOf(info)
	if !ok {
		return "", copyWorkspaceUnsafe("the copy workspace lease identity could not be read")
	}
	return identity, nil
}

func copyLeaseIdentityOf(info fs.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return strconv.FormatUint(uint64(stat.Dev), 10) + ":" + strconv.FormatUint(uint64(stat.Ino), 10), true
}

type copyLeaseMarker struct {
	identity string
	token    string
}

var copyLeaseTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// openVerifiedCopyWorkspace validates and opens one copy workspace. On
// success the caller owns both handles. found is false only when the root or
// job directory does not exist.
func openVerifiedCopyWorkspace(rootDir, jobID string, owner WorkspaceOwner) (root, jobRoot *os.Root, kind string, lease copyLeaseMarker, found bool, err error) {
	if !copyJobIDPattern.MatchString(jobID) {
		return nil, nil, "", copyLeaseMarker{}, false, copyInternalError("copy_workspace_unsafe", "the copy job id is not a safe directory name")
	}
	if rootDir == "" || !filepath.IsAbs(rootDir) {
		return nil, nil, "", copyLeaseMarker{}, false, copyInternalError("copy_workspace_unsafe", "the copy root must be an absolute path")
	}
	rootDir = filepath.Clean(rootDir)
	root, found, err = openCopyRoot(rootDir, owner, false)
	if err != nil || !found {
		return nil, nil, "", copyLeaseMarker{}, false, err
	}
	cleanup := func() {
		if jobRoot != nil {
			_ = jobRoot.Close()
		}
		_ = root.Close()
		root, jobRoot = nil, nil
	}
	entryInfo, err := root.Lstat(jobID)
	if errors.Is(err, fs.ErrNotExist) {
		_ = root.Close()
		return nil, nil, "", copyLeaseMarker{}, false, nil
	}
	if err != nil {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace could not be inspected")
	}
	if entryInfo.Mode()&fs.ModeSymlink != 0 || !entryInfo.IsDir() {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace is not a plain directory")
	}
	jobRoot, err = root.OpenRoot(jobID)
	if err != nil {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace could not be opened")
	}
	openedInfo, err := jobRoot.Stat(".")
	if err != nil || !openedInfo.IsDir() || !os.SameFile(entryInfo, openedInfo) {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace changed while it was being inspected")
	}
	if uid, gid, ok := copyWorkspaceOwnerOf(openedInfo); !ok || uid != owner.UID || gid != owner.GID {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace is not owned by the postgres account")
	}
	if openedInfo.Mode().Perm() != 0o700 {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace does not have private permissions")
	}
	markerInfo, err := jobRoot.Lstat(CopyWorkspaceMarkerName)
	if err != nil || markerInfo.Mode()&fs.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() || markerInfo.Size() > 4096 {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace has no verified operation-ownership marker")
	}
	raw, err := jobRoot.ReadFile(CopyWorkspaceMarkerName)
	if err != nil {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace has no operation-ownership marker")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 || lines[1] != jobID ||
		(lines[0] != "db-copy" && lines[0] != "db-copy-preflight") {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace operation marker does not match this job")
	}
	if !copyLeaseTokenPattern.MatchString(lines[3]) {
		cleanup()
		return nil, nil, "", copyLeaseMarker{}, false, copyWorkspaceUnsafe("the copy workspace operation marker has an invalid lease token")
	}
	return root, jobRoot, lines[0], copyLeaseMarker{identity: lines[2], token: lines[3]}, true, nil
}

func copyWorkspaceUnsafe(message string) error {
	return copyPreconditionError("copy_workspace_unsafe", message,
		"Leave the copy workspace untouched; inspect /var/lib/vaire-copies as root before retrying.", nil)
}

// workspaceModeAllows models the owner and primary group; the runtime probe
// additionally checks the actual postgres credentials and filesystem ACLs.
func workspaceModeAllows(mode fs.FileMode, uid, gid int, owner WorkspaceOwner, bit fs.FileMode) bool {
	if owner.UID == 0 {
		return true
	}
	if owner.UID == uid {
		return mode&bit != 0
	}
	if owner.GID == gid {
		return mode&(bit>>3) != 0
	}
	return mode&(bit>>6) != 0
}

func copyWorkspaceAccessDenied(stage, label string) error {
	if label == copySystemCALabel {
		return copyPreconditionError("copy_workspace_access_denied",
			"the PostgreSQL service account cannot read the system CA bundle",
			"Inspect the system CA bundle permissions before retrying.",
			map[string]any{"stage": stage, "file": label})
	}
	return copyPreconditionError("copy_workspace_access_denied",
		"the PostgreSQL service account cannot access the copy workspace",
		"Inspect the copy workspace ownership and permissions before retrying.",
		map[string]any{"stage": stage, "file": label})
}

// NewCopyWorkspace creates root/jobID with mode 0700. root is created mode
// 0711 if missing so postgres can traverse to the job directory. Ownership is
// always applied through opened handles; the final argument is accepted for
// source compatibility with early C02 call sites and ignored. An existing job
// directory fails closed.
func NewCopyWorkspace(rootDir, jobID string, owner WorkspaceOwner, _ func(string, int, int) error) (*CopyWorkspace, error) {
	if !copyJobIDPattern.MatchString(jobID) {
		return nil, copyInternalError("copy_workspace_failed", "the copy job id is not a safe directory name")
	}
	if rootDir == "" || !filepath.IsAbs(rootDir) {
		return nil, copyInternalError("copy_workspace_failed", "the copy root must be an absolute path")
	}
	rootDir = filepath.Clean(rootDir)
	dir := filepath.Join(rootDir, jobID)
	if filepath.Dir(dir) != rootDir {
		return nil, copyInternalError("copy_workspace_failed", "the copy job path escapes the copy root")
	}
	parent, found, err := openCopyRoot(rootDir, owner, true)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, copyWorkspaceUnsafe("the copy root could not be created")
	}
	var created fs.FileInfo
	var jobRoot *os.Root
	fail := func(cause error) (*CopyWorkspace, error) {
		var cleanupErrs []error
		if jobRoot != nil {
			cleanupErrs = append(cleanupErrs, jobRoot.Close())
		}
		if created != nil {
			cleanupErrs = append(cleanupErrs, removeCreatedCopyJob(parent, jobID, created))
		}
		cleanupErrs = append(cleanupErrs, parent.Close())
		return nil, joinCleanupError(cause, errors.Join(cleanupErrs...))
	}
	if err := parent.Mkdir(jobID, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fail(copyInternalError("copy_workspace_exists", "a workspace for this copy job already exists"))
		}
		return fail(copyWorkspaceError("the copy workspace could not be created"))
	}
	created, err = parent.Lstat(jobID)
	if err != nil || created.Mode()&fs.ModeSymlink != 0 || !created.IsDir() {
		return fail(copyWorkspaceUnsafe("the copy workspace was not created as a plain directory"))
	}
	copyAfterJobMkdir(dir)
	entry, err := parent.Lstat(jobID)
	if err != nil || entry.Mode()&fs.ModeSymlink != 0 || !entry.IsDir() || !os.SameFile(created, entry) {
		return fail(copyWorkspaceUnsafe("the copy workspace changed while it was being created"))
	}
	jobRoot, err = parent.OpenRoot(jobID)
	if err != nil {
		return fail(copyWorkspaceUnsafe("the copy workspace could not be opened"))
	}
	opened, err := jobRoot.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(entry, opened) {
		return fail(copyWorkspaceUnsafe("the copy workspace changed while it was being opened"))
	}
	if err := jobRoot.Chmod(".", 0o700); err != nil { //nolint:gosec // G302: the opened job directory is private.
		return fail(copyWorkspaceError("the copy workspace permissions could not be set"))
	}
	if owner != CurrentWorkspaceOwner() {
		if err := jobRoot.Chown(".", owner.UID, owner.GID); err != nil {
			return fail(copyWorkspaceError("the copy workspace ownership could not be set"))
		}
	}
	copyAfterJobRootOpen(dir)
	finalInfo, err := jobRoot.Stat(".")
	if err != nil {
		return fail(copyWorkspaceUnsafe("the copy workspace could not be verified"))
	}
	if uid, gid, ok := copyWorkspaceOwnerOf(finalInfo); !ok || uid != owner.UID || gid != owner.GID {
		return fail(copyWorkspaceUnsafe("the copy workspace is not owned by the postgres account"))
	}
	if finalInfo.Mode().Perm() != 0o700 {
		return fail(copyWorkspaceUnsafe("the copy workspace does not have private permissions"))
	}
	entryNow, err := parent.Lstat(jobID)
	if err != nil || entryNow.Mode()&fs.ModeSymlink != 0 || !entryNow.IsDir() || !os.SameFile(entryNow, finalInfo) {
		return fail(copyWorkspaceUnsafe("the copy workspace changed while ownership was being set"))
	}
	ws := &CopyWorkspace{
		Dir:      dir,
		owner:    owner,
		root:     jobRoot,
		parent:   parent,
		jobID:    jobID,
		identity: finalInfo,
	}
	if err := ws.mkdir(copyHomeName); err != nil {
		cleanupErr := ws.Remove()
		return nil, joinCleanupError(err, cleanupErr)
	}
	return ws, nil
}

// removeCreatedCopyJob removes a just-created job directory only when the
// parent entry still names the exact directory that was created. A
// replacement or symlink is left untouched.
func removeCreatedCopyJob(parent *os.Root, jobID string, created fs.FileInfo) error {
	current, err := parent.Lstat(jobID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return copyWorkspaceUnsafe("the copy workspace could not be inspected for cleanup")
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(created, current) {
		return nil
	}
	if err := parent.RemoveAll(jobID); err != nil {
		return copyWorkspaceError("the copy workspace could not be removed after a failed creation")
	}
	return nil
}

func copyWorkspaceError(message string) error {
	return copyPreconditionError("copy_workspace_failed", message,
		"Check /var/lib/vaire-copies and its ownership and permissions, then retry with a new job.", nil)
}

func copyMissingToolError(path string) error {
	details := map[string]any{"reason": "missing_pg18_tool"}
	if path != "" {
		details["path"] = path
	}
	return output.NewError(output.CodeMissingTool, "a PostgreSQL 18 client tool is missing", output.ExitMissingTool,
		"Install the PostgreSQL 18 client tools on the node and retry.").WithDetails(details)
}

func copyInternalError(reason, message string) error {
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, "").
		WithDetails(map[string]any{"reason": reason})
}

func copyPreconditionError(reason, message, hint string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	if _, ok := details["reason"]; !ok {
		details["reason"] = reason
	}
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, hint).WithDetails(details)
}

func sourceInputError(reason, message, hint string) error {
	return output.NewError(output.CodeValidation, message, output.ExitConfig, hint).
		WithDetails(map[string]any{"reason": reason})
}

func copyReporter(report engine.Reporter) engine.Reporter {
	if report == nil {
		return func(output.Event) {}
	}
	return func(ev output.Event) {
		// A subprocess log line is arbitrary server/database text. It can
		// contain SQL, row values or continuation data, so it never leaves
		// this package. Only structured progress events are forwarded.
		if ev.Type == "log" {
			return
		}
		report(ev)
	}
}

func joinCleanupError(err, cleanup error) error {
	if cleanup == nil {
		return err
	}
	if err == nil {
		return cleanup
	}
	return errors.Join(err, cleanup)
}

// mkdir creates a private directory and transfers it to the workspace owner
// through an opened directory handle.
func (ws *CopyWorkspace) mkdir(name string) error {
	if !validCopyName(name) {
		return copyInternalError("copy_workspace_failed", "the workspace file name is not safe")
	}
	if err := ws.root.Mkdir(name, 0o700); err != nil {
		return copyWorkspaceError("the copy workspace directory could not be created")
	}
	fail := func(cause error) error {
		var removeErr error
		if err := ws.root.RemoveAll(name); err != nil {
			removeErr = copyWorkspaceError("the copy workspace directory could not be removed after a failed creation")
		}
		return joinCleanupError(cause, removeErr)
	}
	sub, err := ws.root.OpenRoot(name)
	if err != nil {
		return fail(copyWorkspaceError("the copy workspace directory could not be opened"))
	}
	defer func() { _ = sub.Close() }()
	if err := sub.Chmod(".", 0o700); err != nil { //nolint:gosec // G302: the opened workspace directory is private.
		return fail(copyWorkspaceError("the copy workspace directory permissions could not be set"))
	}
	if ws.owner != CurrentWorkspaceOwner() {
		if err := sub.Chown(".", ws.owner.UID, ws.owner.GID); err != nil {
			return fail(copyWorkspaceError("the copy workspace directory ownership could not be set"))
		}
	}
	info, err := sub.Stat(".")
	if err != nil {
		return fail(copyWorkspaceError("the copy workspace directory could not be verified"))
	}
	if uid, gid, ok := copyWorkspaceOwnerOf(info); !ok || uid != ws.owner.UID || gid != ws.owner.GID {
		return fail(copyWorkspaceUnsafe("the copy workspace directory is not owned by the postgres account"))
	}
	return nil
}

// Create creates a new exclusive 0600 file in the workspace and applies
// ownership through the opened file handle.
func (ws *CopyWorkspace) Create(name string) (*os.File, error) {
	if ws == nil || ws.root == nil {
		return nil, copyInternalError("copy_workspace_failed", "the copy workspace is closed")
	}
	if !validCopyName(name) {
		return nil, copyInternalError("copy_workspace_failed", "the workspace file name is not safe")
	}
	f, err := ws.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, copyWorkspaceError("the workspace file could not be created exclusively")
	}
	fail := func() (*os.File, error) {
		_ = f.Close()
		removeErr := ws.RemoveFile(name)
		return nil, joinCleanupError(copyWorkspaceError("the workspace file could not be prepared safely"), removeErr)
	}
	if err := f.Chmod(0o600); err != nil {
		return fail()
	}
	if ws.owner != CurrentWorkspaceOwner() {
		if err := f.Chown(ws.owner.UID, ws.owner.GID); err != nil {
			return fail()
		}
	}
	info, err := f.Stat()
	if err != nil {
		return fail()
	}
	if uid, gid, ok := copyWorkspaceOwnerOf(info); !ok || uid != ws.owner.UID || gid != ws.owner.GID {
		return fail()
	}
	return f, nil
}

// WriteFile writes a new exclusive 0600 file.
func (ws *CopyWorkspace) WriteFile(name string, data []byte) error {
	f, err := ws.Create(name)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		removeErr := ws.RemoveFile(name)
		return joinCleanupError(copyWorkspaceError("the workspace file could not be written"), removeErr)
	}
	return nil
}

// Open opens a workspace file for reading.
func (ws *CopyWorkspace) Open(name string) (*os.File, error) {
	if ws == nil || ws.root == nil {
		return nil, copyInternalError("copy_workspace_failed", "the copy workspace is closed")
	}
	if !validCopyName(name) {
		return nil, copyInternalError("copy_workspace_failed", "the workspace file name is not safe")
	}
	return ws.root.Open(name)
}

// Stat returns a workspace file's metadata.
func (ws *CopyWorkspace) Stat(name string) (fs.FileInfo, error) {
	if ws == nil || ws.root == nil {
		return nil, copyInternalError("copy_workspace_failed", "the copy workspace is closed")
	}
	if !validCopyName(name) {
		return nil, copyInternalError("copy_workspace_failed", "the workspace file name is not safe")
	}
	return ws.root.Stat(name)
}

// FilePath returns the absolute path of a validated workspace file.
func (ws *CopyWorkspace) FilePath(name string) string {
	if ws == nil || !validCopyName(name) {
		return ""
	}
	return filepath.Join(ws.Dir, name)
}

// RemoveFile removes a workspace file, ignoring a missing file.
func (ws *CopyWorkspace) RemoveFile(name string) error {
	if ws == nil || ws.root == nil {
		return nil
	}
	if !validCopyName(name) {
		return copyInternalError("copy_workspace_failed", "the workspace file name is not safe")
	}
	if err := ws.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return copyPreconditionError("workspace_cleanup_failed",
			"a temporary copy file could not be removed",
			"Check /var/lib/vaire-copies and retry after the copy job has stopped.", nil)
	}
	return nil
}

// Cleanup removes the temporary archive, access files and home directory but
// leaves the workspace directory itself.
func (ws *CopyWorkspace) Cleanup() error {
	if ws == nil || ws.root == nil {
		return nil
	}
	var errs []error
	for _, name := range []string{copyArchiveName, copyUseListName, copyInspectName, copyPassfileName, copyCAName} {
		if err := ws.RemoveFile(name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := ws.root.RemoveAll(copyHomeName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, copyPreconditionError("workspace_cleanup_failed",
			"the temporary copy home directory could not be removed",
			"Check /var/lib/vaire-copies and retry after the copy job has stopped.", nil))
	}
	return errors.Join(errs...)
}

// Remove removes the whole workspace only while the verified parent entry
// still names the directory created by NewCopyWorkspace. A replaced directory
// or symlink is left untouched.
func (ws *CopyWorkspace) Remove() error {
	if ws == nil {
		return nil
	}
	closeHandles := func() error {
		var errs []error
		if ws.root != nil {
			errs = append(errs, ws.root.Close())
			ws.root = nil
		}
		if ws.parent != nil {
			errs = append(errs, ws.parent.Close())
			ws.parent = nil
		}
		return errors.Join(errs...)
	}
	if ws.parent == nil || ws.identity == nil {
		_, err := os.Lstat(ws.Dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return joinCleanupError(copyWorkspaceUnsafe("the copy workspace has no verified parent handle"), closeHandles())
	}
	entry, err := ws.parent.Lstat(ws.jobID)
	if errors.Is(err, fs.ErrNotExist) {
		return closeHandles()
	}
	if err != nil || entry.Mode()&fs.ModeSymlink != 0 || !entry.IsDir() || !os.SameFile(entry, ws.identity) {
		return joinCleanupError(copyWorkspaceUnsafe("the copy workspace changed before removal"), closeHandles())
	}
	if ws.root != nil {
		_ = ws.root.Close()
		ws.root = nil
	}
	removeErr := ws.parent.RemoveAll(ws.jobID)
	parentErr := ws.parent.Close()
	ws.parent = nil
	if removeErr != nil {
		return joinCleanupError(copyWorkspaceError("the copy workspace could not be removed"), parentErr)
	}
	return parentErr
}

// Close releases the workspace directory handles.
func (ws *CopyWorkspace) Close() error {
	if ws == nil {
		return nil
	}
	var errs []error
	if ws.root != nil {
		errs = append(errs, ws.root.Close())
		ws.root = nil
	}
	if ws.parent != nil {
		errs = append(errs, ws.parent.Close())
		ws.parent = nil
	}
	return errors.Join(errs...)
}

func validCopyName(name string) bool {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return !hasControl(name)
}

// sourceAccess is the private file set used for one source operation.
type sourceAccess struct {
	ws       *CopyWorkspace
	passfile string
	caFile   string
	env      []string
}

func prepareSourceAccess(ws *CopyWorkspace, src Source) (*sourceAccess, error) {
	if ws == nil || ws.root == nil {
		return nil, copyInternalError("copy_workspace_failed", "the copy workspace is closed")
	}
	mode := src.TLSMode
	if mode == "" {
		mode = SourceTLSVerifyFull
	}
	caPath := ""
	if len(src.caPEM) > 0 {
		caPath = ws.FilePath(copyCAName)
	}
	var env []string
	if mode == SourceTLSVerifyCA && caPath == "" {
		info, err := os.Stat(systemCABundlePath)
		if err != nil || info.IsDir() {
			return nil, copyPreconditionError("source_tls_ca_unavailable",
				"sslmode=verify-ca needs a system CA bundle, and this node does not have one",
				"Use sslmode=verify-full, or supply the source CA explicitly.", nil)
		}
		caPath = systemCABundlePath
	}
	if err := ws.WriteFile(copyPassfileName, []byte(src.passfileLine())); err != nil {
		return nil, err
	}
	copyAfterSourcePassfile(ws)
	if len(src.caPEM) > 0 {
		if err := ws.WriteFile(copyCAName, src.caPEM); err != nil {
			cleanupErr := ws.RemoveFile(copyPassfileName)
			return nil, joinCleanupError(err, cleanupErr)
		}
	}
	env = []string{
		"HOME=" + ws.FilePath(copyHomeName),
		"PGPASSFILE=" + ws.FilePath(copyPassfileName),
		"PGCONNECT_TIMEOUT=" + strconv.Itoa(int(sourceConnectTimeout/time.Second)),
		"PGGSSENCMODE=disable",
		"PGSSLMODE=" + mode,
		"PGAPPNAME=" + sourcePGAppName,
		"PGCLIENTENCODING=UTF8",
	}
	if mode == SourceTLSVerifyFull && caPath == "" {
		caPath = "system"
	}
	if caPath != "" {
		env = append(env, "PGSSLROOTCERT="+caPath)
	}
	return &sourceAccess{ws: ws, passfile: copyPassfileName, caFile: copyCAName, env: env}, nil
}

func (a *sourceAccess) remove() error {
	if a == nil || a.ws == nil {
		return nil
	}
	var errs []error
	if err := a.ws.RemoveFile(a.passfile); err != nil {
		errs = append(errs, err)
	}
	if a.ws.FilePath(a.caFile) != "" {
		if err := a.ws.RemoveFile(a.caFile); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return output.NewError(output.CodeSecrets, "the temporary source access file could not be removed", output.ExitSecrets,
			"Check /var/lib/vaire-copies before retrying; the copy job must not continue with source secrets on disk.").
			WithDetails(map[string]any{"reason": "secret_cleanup_failed"})
	}
	return nil
}

// The marker is emitted by a shell running as postgres, only after test has
// actually run. A runuser or lease-wrapper failure cannot impersonate a
// negative test merely by returning the same exit status.
const sourceAccessProbeScript = `if /usr/bin/test -r "$1"; then printf readable; else status=$?; if [ "$status" -eq 1 ]; then printf denied; else exit "$status"; fi; fi`

// Probe as the same service identity used by libpq without reading contents.
func probeSourceAccess(ctx context.Context, exec CopyExecutor, step string, a *sourceAccess, src Source) error {
	type probe struct{ label, path string }
	probes := []probe{{copyPassfileName, a.ws.FilePath(copyPassfileName)}}
	if len(src.caPEM) > 0 {
		probes = append(probes, probe{copyCAName, a.ws.FilePath(copyCAName)})
	} else if src.TLSMode == SourceTLSVerifyCA {
		probes = append(probes, probe{copySystemCALabel, systemCABundlePath})
	}
	for _, p := range probes {
		probeCtx, cancel := context.WithTimeout(ctx, sourceConnectTimeout)
		var out limitedBuffer
		out.limit = 32
		err := exec.Exec(probeCtx, step+"-workspace-probe", engine.Command{
			Name: "runuser", Args: []string{"-u", "postgres", "--", "/bin/sh", "-c", sourceAccessProbeScript, "vaire-copy-probe", p.path},
			Stdout: &out,
		}, copyReporter(nil))
		cancel()
		if err != nil {
			if errors.Is(err, engine.ErrInterrupted) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			var cliErr *output.CLIError
			if errors.As(err, &cliErr) {
				return cliErr
			}
			return output.NewError(output.CodeExternalCommand, "the copy workspace access probe failed", output.ExitExternal,
				"Check the node's copy process prerequisites before retrying.").
				WithDetails(map[string]any{"reason": "copy_workspace_probe_failed"})
		}
		if !out.overflow && out.String() == "denied" {
			return copyWorkspaceAccessDenied("source-access", p.label)
		}
		if out.overflow || out.String() != "readable" {
			return output.NewError(output.CodeExternalCommand, "the copy workspace access probe returned an invalid result", output.ExitExternal,
				"Check the node's copy process prerequisites before retrying.").
				WithDetails(map[string]any{"reason": "copy_workspace_probe_failed"})
		}
	}
	return nil
}

func (s Source) passfileLine() string {
	fields := []string{s.Host, strconv.Itoa(s.Port), s.Database, s.User, s.password}
	for i := range fields {
		fields[i] = escapePassfileField(fields[i])
	}
	return strings.Join(fields, ":") + "\n"
}

func escapePassfileField(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, ":", `\:`)
}

func sourceCommand(access *sourceAccess, tool string, args []string) engine.Command {
	argv := []string{"-u", "postgres", "--", envPath}
	argv = append(argv, access.env...)
	argv = append(argv, tool)
	argv = append(argv, args...)
	return engine.Command{Name: "runuser", Args: argv}
}

func (a *sourceAccess) envValues() map[string]string {
	out := map[string]string{}
	for _, kv := range a.env {
		key, value, _ := strings.Cut(kv, "=")
		out[key] = value
	}
	return out
}

func sourcePreflightError(err error) error {
	if errors.Is(err, engine.ErrInterrupted) {
		return err
	}
	var scriptErr *engine.ScriptError
	if errors.As(err, &scriptErr) {
		if label := localSourceFileLabel(boundedDiagnosticText(scriptErr.Tail)); label != "" {
			return copyWorkspaceAccessDenied("source-access", label)
		}
		reason, message, hint := classifySourceFailure(scriptErr.Tail)
		return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, hint).
			WithDetails(map[string]any{"reason": reason, "exit_status": scriptErr.Status})
	}
	return output.NewError(output.CodeExternalCommand,
		"the source preflight command could not be started", output.ExitExternal,
		"Check that the PostgreSQL 18 client tools are installed and the source is reachable.").
		WithDetails(map[string]any{"reason": "source_preflight_failed"})
}

func sourceExportError(err error) error {
	if errors.Is(err, engine.ErrInterrupted) {
		return err
	}
	var scriptErr *engine.ScriptError
	if !errors.As(err, &scriptErr) {
		return output.NewError(output.CodeExternalCommand,
			"the source export command could not be started", output.ExitExternal,
			"Check that the PostgreSQL 18 client tools are installed and the copy workspace is writable.").
			WithDetails(map[string]any{"reason": "source_export_failed"})
	}
	if label := localSourceFileLabel(boundedDiagnosticText(scriptErr.Tail)); label != "" {
		return copyWorkspaceAccessDenied("source-access", label)
	}
	if reason, message, hint, ok := classifySourceFailureStrict(scriptErr.Tail); ok {
		return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, hint).
			WithDetails(map[string]any{"reason": reason, "exit_status": scriptErr.Status})
	}
	return output.NewError(output.CodeExternalCommand,
		"the source database export failed", output.ExitExternal,
		"Check the source and the run log, then retry with a new copy job.").
		WithDetails(map[string]any{
			"reason":      "source_export_failed",
			"exit_status": scriptErr.Status,
		})
}

func classifySourceFailure(tail []string) (reason, message, hint string) {
	if reason, message, hint, ok := classifySourceFailureStrict(tail); ok {
		return reason, message, hint
	}
	return "source_preflight_failed",
		"the source preflight query failed",
		"Check the source connection details and that the source user can read the database catalog."
}

// boundedDiagnosticText produces bounded lowercase text for fixed failure
// classification. It is never returned, logged or stored.
func boundedDiagnosticText(tail []string) string {
	const limit = 64 << 10
	var b strings.Builder
	for _, line := range tail {
		if b.Len() >= limit {
			break
		}
		remaining := limit - b.Len()
		if len(line) > remaining {
			line = line[:remaining]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.ToLower(b.String())
}

func classifySourceFailureStrict(tail []string) (reason, message, hint string, ok bool) {
	text := boundedDiagnosticText(tail)
	if sourceSQLFailure(text) {
		return "", "", "", false
	}
	switch {
	case localSourceFileLabel(text) != "":
		return "copy_workspace_access_denied",
			"the PostgreSQL service account cannot access the copy workspace",
			"Inspect the copy workspace ownership and permissions before retrying.", true
	case strings.Contains(text, "password authentication failed"),
		strings.Contains(text, "no password supplied"),
		strings.Contains(text, "authentication failed"),
		strings.Contains(text, "pg_hba.conf rejects"):
		return "source_auth_failed",
			"the source database rejected the username or password",
			"Check the source username and password and whether the source allows this vaire server.", true
	case strings.Contains(text, "certificate verify failed"),
		strings.Contains(text, "ssl error"),
		strings.Contains(text, "ssl connection has been closed"),
		strings.Contains(text, "server certificate"),
		strings.Contains(text, "root certificate"),
		strings.Contains(text, "self-signed certificate"),
		strings.Contains(text, "unable to get local issuer certificate"),
		strings.Contains(text, "does not match host name"),
		strings.Contains(text, "hostname verification"):
		return "source_tls_failed",
			"the source TLS connection could not be verified",
			"Check the source hostname, TLS mode and CA certificate; vaire never falls back to plaintext.", true
	case strings.Contains(text, "could not translate host name"),
		strings.Contains(text, "connection refused"),
		strings.Contains(text, "connection timed out"),
		strings.Contains(text, "timeout expired"),
		strings.Contains(text, "could not connect"),
		strings.Contains(text, "no route to host"),
		strings.Contains(text, "network is unreachable"):
		return "source_unreachable",
			"the vaire server could not reach the source database",
			"Check the source host and port and whether its firewall allows the vaire server.", true
	default:
		return "", "", "", false
	}
}

var sourcePSQLSQLFailure = regexp.MustCompile(`^(?:psql:[^:\n]*:[0-9]+: )?error:`)

func sourceSQLFailure(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pg_dump: error: query failed:") || sourcePSQLSQLFailure.MatchString(line) {
			return true
		}
	}
	return false
}

func localSourceFileLabel(text string) string {
	if sourceSQLFailure(text) {
		return ""
	}
	root := regexp.QuoteMeta(strings.ToLower(CopyRoot))
	systemCA := regexp.QuoteMeta(strings.ToLower(systemCABundlePath))
	privatePath := root + `/[a-z0-9][a-z0-9._-]{0,127}/`
	clientPrefix := `^(?:psql|pg_dump): (?:error|warning): (?:connection to server at [^\n]* failed: )?`
	ca := regexp.MustCompile(clientPrefix + `could not (?:open|read) root certificate file "(` + privatePath + regexp.QuoteMeta(copyCAName) + `|` + systemCA + `)": permission denied\.?$`)
	pass := regexp.MustCompile(clientPrefix + `could not (?:open|read) password file "(` + privatePath + regexp.QuoteMeta(copyPassfileName) + `)": permission denied\.?$`)
	for _, line := range strings.Split(text, "\n") {
		if match := ca.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			if match[1] == strings.ToLower(systemCABundlePath) {
				return copySystemCALabel
			}
			return copyCAName
		}
		if pass.MatchString(strings.TrimSpace(line)) {
			return copyPassfileName
		}
	}
	return ""
}

func archiveToolError(message string, err error) error {
	if errors.Is(err, engine.ErrInterrupted) {
		return err
	}
	details := map[string]any{"reason": "archive_unreadable"}
	var scriptErr *engine.ScriptError
	if errors.As(err, &scriptErr) {
		details["exit_status"] = scriptErr.Status
	}
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition,
		"The export may be incomplete; retry the copy with a new job.").WithDetails(details)
}

func restoreError(err error) error {
	if errors.Is(err, engine.ErrInterrupted) {
		return err
	}
	var scriptErr *engine.ScriptError
	if !errors.As(err, &scriptErr) {
		return output.NewError(output.CodeExternalCommand,
			"the pg_restore command could not be started", output.ExitExternal,
			"Check the destination cluster and the run log; do not retry into the same database.").
			WithDetails(map[string]any{"reason": "restore_failed"})
	}
	reason, message, hint := classifyRestoreFailure(scriptErr.Tail)
	return output.NewError(output.CodeExternalCommand, message, output.ExitExternal, hint).
		WithDetails(map[string]any{"reason": reason, "exit_status": scriptErr.Status})
}

// classifyRestoreFailure maps known pg_restore failure families to fixed
// messages. It never returns server text, SQL, object names or row values.
func classifyRestoreFailure(tail []string) (reason, message, hint string) {
	text := boundedDiagnosticText(tail)
	switch {
	case strings.Contains(text, "permission denied"),
		strings.Contains(text, "must be owner"),
		strings.Contains(text, "insufficient privilege"):
		return "restore_permission_failed",
			"the destination role lacked a privilege required by the source archive; the copy is incomplete",
			"Fix the destination provisioning or the identified privilege and retry with a new database name; do not retry into this one."
	case strings.Contains(text, "already exists"),
		strings.Contains(text, "duplicate key"):
		return "restore_conflict",
			"the destination database already had an object or row that the source archive tried to create; the copy is incomplete",
			"Inspect the partial database; do not retry into the same database."
	default:
		return "restore_failed",
			"pg_restore into the new database failed; the copy is incomplete",
			"Inspect the partial database and the run log; do not retry the import into the same database."
	}
}

func safeDiag(value string) string {
	if len(value) > 120 {
		value = value[:120] + "..."
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func parsePreflightBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "t", "true", "on", "1":
		return true, nil
	case "f", "false", "off", "0":
		return false, nil
	default:
		return false, errors.New("invalid boolean")
	}
}

// dropPSQLCommandTags removes one-field SET/COPY command-status records that a
// psql version might print despite -q. DATA rows always have three columns.
func dropPSQLCommandTags(records [][]string) [][]string {
	out := records[:0]
	for _, record := range records {
		if len(record) == 1 {
			value := strings.TrimSpace(record[0])
			upper := strings.ToUpper(value)
			if strings.EqualFold(value, "SET") || strings.HasPrefix(upper, "COPY ") {
				continue
			}
		}
		out = append(out, record)
	}
	return out
}

// limitedBuffer bounds a diagnostic or TOC capture.
type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.overflow {
		return 0, errors.New("output limit exceeded")
	}
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	if len(p) > remaining {
		b.overflow = true
		_, _ = b.Buffer.Write(p[:remaining])
		return remaining, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}
