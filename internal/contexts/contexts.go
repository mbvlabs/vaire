// Package contexts manages the operator's files under $XDG_CONFIG_HOME/eru
// (§6.3): the current context, one directory of credentials per server, and
// the pending state of an unfinished server init.
package contexts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/pelletier/go-toml/v2"
)

// Context file names inside contexts/<name>/.
const (
	ContextFile     = "context.toml"
	WGKeyFile       = "wg.key"
	SSHKeyFile      = "id_ed25519"
	KnownHostsFile  = "known_hosts"
	CredentialsFile = "credentials.json"
)

// EnvContext overrides the current context.
const EnvContext = "ERU_CONTEXT"

// NamePattern is a context name. Server names always match it.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Context is contexts/<name>/context.toml. It points at the master by its
// hostname. Nodes and Databases are a cache of the master's view, refreshed
// from it.
type Context struct {
	Name    string `toml:"-" json:"name"`
	Server  string `toml:"server" json:"server"`
	Domain  string `toml:"domain,omitempty" json:"domain,omitempty"`
	TLSMode string `toml:"tls_mode,omitempty" json:"tls_mode,omitempty"`
	Nodes   []Node `toml:"nodes" json:"nodes"`
	// Databases is the placement map: database name to node name.
	Databases map[string]string `toml:"databases" json:"databases"`
	Operator  Operator          `toml:"operator" json:"operator"`
	// AutoUpgradeNodes installs the operator's build on a node that runs a
	// different one instead of failing with node_version_mismatch.
	AutoUpgradeNodes bool `toml:"auto_upgrade_nodes" json:"auto_upgrade_nodes"`
	// LockdownConfirmedAt is set once this machine has confirmed the SSH
	// lockdown over WireGuard (§8.5). Until then the first remote command
	// calls ssh.confirm.
	LockdownConfirmedAt string `toml:"lockdown_confirmed_at,omitempty" json:"lockdown_confirmed_at,omitempty"`
	// TunnelPersist is how long the context's tunnel daemon stays up with
	// no eru command using it, as a Go duration ("10m", "0" exits with the
	// last command). ERU_TUNNEL_PERSIST overrides it.
	TunnelPersist string `toml:"tunnel_persist,omitempty" json:"tunnel_persist,omitempty"`
	CreatedAt     string `toml:"created_at" json:"created_at"`
	EruVersion    string `toml:"eru_version" json:"eru_version"`
}

// Node is one node the context can reach: its name, hostname and WireGuard
// address. The WireGuard endpoint (public address, port and key) is what the
// tunnel to the master needs.
type Node struct {
	Name          string `toml:"name" json:"name"`
	Role          string `toml:"role" json:"role"`
	Hostname      string `toml:"hostname" json:"hostname"`
	PublicAddress string `toml:"public_address" json:"public_address"`
	WGAddress     string `toml:"wg_address" json:"wg_address"`
	WGPublicKey   string `toml:"wg_public_key" json:"wg_public_key"`
	ListenPort    int    `toml:"listen_port" json:"listen_port"`
}

// Operator is this machine's identity on the server.
type Operator struct {
	Name      string `toml:"name" json:"name"`
	WGAddress string `toml:"wg_address" json:"wg_address"`
	SSHUser   string `toml:"ssh_user" json:"ssh_user"`
}

// Credentials is contexts/<name>/credentials.json: the secrets document's
// values that are not key files.
type Credentials struct {
	SudoPassword string         `json:"sudo_password,omitempty"`
	Database     *DatabaseCreds `json:"database,omitempty"`
	Backup       *BackupCreds   `json:"backup,omitempty"`
	// Metrics is the /metrics endpoint credential (Phase 4). The node keeps
	// only its bcrypt hash.
	Metrics *MetricsCreds `json:"metrics,omitempty"`
	// MasterPasswordHash gates eru secrets export: an argon2id hash of the
	// master password, never the password. It travels in the secrets
	// document so eru login carries it to another machine.
	MasterPasswordHash string `json:"master_password_hash,omitempty"`
}

// RoleMaster is the master node's role.
const RoleMaster = "master"

// Master returns the master node.
func (c *Context) Master() (Node, bool) {
	for _, n := range c.Nodes {
		if n.Role == RoleMaster {
			return n, true
		}
	}
	if len(c.Nodes) > 0 {
		return c.Nodes[0], true
	}
	return Node{}, false
}

// DatabaseNode returns the node the placement cache puts database db on, and
// whether the cache knows db at all. An unknown database gets the master.
func (c *Context) DatabaseNode(db string) (Node, bool) {
	if name, ok := c.Databases[db]; ok {
		for _, n := range c.Nodes {
			if n.Name == name {
				return n, true
			}
		}
	}
	n, _ := c.Master()
	return n, false
}

// MasterHostname is the master's hostname, which its certificate carries.
func (c *Context) MasterHostname() string {
	n, _ := c.Master()
	return n.Hostname
}

// Root is $XDG_CONFIG_HOME/eru, default ~/.config/eru.
func Root() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "eru")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "eru-config", "eru")
	}
	return filepath.Join(home, ".config", "eru")
}

// Dir is contexts/<name>.
func Dir(name string) string { return filepath.Join(Root(), "contexts", name) }

// Path is a file inside contexts/<name>.
func Path(name, file string) string { return filepath.Join(Dir(name), file) }

func ensureRoot() error {
	root := Root()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.Chmod(root, 0o700) //nolint:gosec // G302: §6.3 sets the config directory to 0700.
}

// ValidateName checks a context name.
func ValidateName(name string) error {
	if NamePattern.MatchString(name) {
		return nil
	}
	hint := "Use lower-case letters, digits, '.', '_' and '-'."
	if strings.ContainsAny(name, " \t") {
		// Most likely psql's -c landing on eru's global -c (--context).
		hint = "-c is eru's --context. To pass arguments to psql, put them after --: " +
			"eru psql --db <db> -- -c \"select 1\"."
	}
	return output.NewError(
		output.CodeValidation,
		"context name "+name+" is invalid",
		output.ExitConfig,
		hint,
	).WithDetails(map[string]any{"name": name})
}

// Exists reports whether contexts/<name>/context.toml exists.
func Exists(name string) bool {
	_, err := os.Stat(Path(name, ContextFile))
	return err == nil
}

// List returns the context names, sorted.
func List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(Root(), "contexts"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && Exists(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Load reads contexts/<name>/context.toml.
func Load(name string) (*Context, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(Path(name, ContextFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, notFound(name)
		}
		return nil, err
	}
	var c Context
	if err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&c); err != nil {
		return nil, output.NewError(output.CodeConfig, Path(name, ContextFile)+" does not parse: "+err.Error(), output.ExitConfig,
			"Recreate the context with eru login.")
	}
	c.Name = name
	return &c, nil
}

func notFound(name string) *output.CLIError {
	return output.NewError(
		output.CodeContextNotFound,
		"context "+name+" does not exist",
		output.ExitContext,
		contextHint("List contexts with eru context list, or create one with eru login <secrets.age>."),
	).WithDetails(map[string]any{"context": name})
}

// nodeMarker is the file whose presence makes this machine an eru node.
var nodeMarker = config.ClusterPath

// contextHint returns hint, or on an eru node (which holds no contexts) a
// hint to run the command from the operator machine instead.
func contextHint(hint string) string {
	if _, err := os.Stat(nodeMarker); err == nil {
		return "This machine is an eru node (" + nodeMarker + " exists) and holds no contexts. " +
			"Run this command from your own machine; on the node, use sudo eru node run or sudo eru node selfcheck."
	}
	return hint
}

// Save writes context.toml.
func Save(c *Context) error {
	if err := ValidateName(c.Name); err != nil {
		return err
	}
	if err := ensureRoot(); err != nil {
		return err
	}
	raw, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return transport.WriteSecretFile(Path(c.Name, ContextFile), raw)
}

// WriteFile writes a 0600 file inside contexts/<name>.
func WriteFile(name, file string, data []byte) error {
	if err := ensureRoot(); err != nil {
		return err
	}
	return transport.WriteSecretFile(Path(name, file), data)
}

// ReadFile reads a file inside contexts/<name>.
func ReadFile(name, file string) ([]byte, error) {
	return os.ReadFile(Path(name, file))
}

// SaveCredentials writes credentials.json.
func SaveCredentials(name string, creds Credentials) error {
	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(name, CredentialsFile, append(raw, '\n'))
}

// LoadCredentials reads credentials.json.
func LoadCredentials(name string) (Credentials, error) {
	raw, err := ReadFile(name, CredentialsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, nil
		}
		return Credentials{}, err
	}
	var creds Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return Credentials{}, err
	}
	return creds, nil
}

// SetMasterPasswordHash stores the master password hash for a context.
func SetMasterPasswordHash(name, hash string) error {
	creds, err := LoadCredentials(name)
	if err != nil {
		return err
	}
	creds.MasterPasswordHash = hash
	return SaveCredentials(name, creds)
}

// Remove deletes contexts/<name> and clears the current context if it was
// this one.
func Remove(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if !Exists(name) {
		return notFound(name)
	}
	if err := os.RemoveAll(Dir(name)); err != nil {
		return err
	}
	if current, _ := Current(); current == name {
		return SetCurrent("")
	}
	return nil
}

type rootConfig struct {
	CurrentContext string `toml:"current_context"`
}

// Current returns config.toml current_context.
func Current() (string, error) {
	raw, err := os.ReadFile(filepath.Join(Root(), "config.toml"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var cfg rootConfig
	if err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&cfg); err != nil {
		return "", output.NewError(output.CodeConfig, "config.toml does not parse: "+err.Error(), output.ExitConfig, "")
	}
	return cfg.CurrentContext, nil
}

// SetCurrent writes config.toml current_context.
func SetCurrent(name string) error {
	if name != "" && !Exists(name) {
		return notFound(name)
	}
	if err := ensureRoot(); err != nil {
		return err
	}
	raw, err := toml.Marshal(rootConfig{CurrentContext: name})
	if err != nil {
		return err
	}
	return transport.WriteSecretFile(filepath.Join(Root(), "config.toml"), raw)
}

// Resolve picks the context: the --context flag, then ERU_CONTEXT, then the
// current context.
func Resolve(flag string) (*Context, error) {
	name := strings.TrimSpace(flag)
	if name == "" {
		name = strings.TrimSpace(os.Getenv(EnvContext))
	}
	if name == "" {
		current, err := Current()
		if err != nil {
			return nil, err
		}
		name = current
	}
	if name == "" {
		return nil, output.NewError(
			output.CodeContextNotFound,
			"no context is selected",
			output.ExitContext,
			contextHint("Run eru login <secrets.age>, or pick one with eru context use <name>."),
		)
	}
	return Load(name)
}

// FixModes tightens any file in contexts/<name> that is looser than 0600, and
// the directory to 0700. It returns a warning per fixed path.
func FixModes(name string) []output.Warning {
	var warnings []output.Warning
	dir := Dir(name)
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm() != 0o700 {
		if os.Chmod(dir, 0o700) == nil { //nolint:gosec // G302: §6.3 context directories are 0700.
			warnings = append(warnings, output.Warning{Code: "context_mode_fixed", Message: fmt.Sprintf("%s was %04o; set to 0700", dir, info.Mode().Perm())})
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return warnings
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 == 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if os.Chmod(path, 0o600) == nil {
			warnings = append(warnings, output.Warning{Code: "context_mode_fixed", Message: fmt.Sprintf("%s was %04o; set to 0600", path, info.Mode().Perm())})
		}
	}
	return warnings
}
