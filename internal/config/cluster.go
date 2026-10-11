package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/pelletier/go-toml/v2"
)

// ClusterPath is the desired state on the central command.
const ClusterPath = "/etc/eru/cluster.toml"

// ClusterSchema is the cluster.toml format version.
const ClusterSchema = 1

// Addressing defaults (§7.1). Nodes use 10.42.0.0/24, operators 10.42.255.0/24.
const (
	DefaultSubnet          = "10.42.0.0/16"
	DefaultListenPort      = 51820
	DefaultCentralWG       = "10.42.0.1"
	DefaultFirstOperatorWG = "10.42.255.1"
	DefaultAdminUser       = "eru"
	DefaultSwapMB          = 1024
	DefaultTimezone        = "UTC"
	DefaultDataDirectory   = "/var/lib/postgresql/18/main"
	RolePrimary            = "primary"
)

// Cluster is cluster.toml (§6.1). Phase 1 reads the tables the host steps
// need. Unknown keys are allowed until Phase 2 adds the full schema and
// switches to a strict decode.
type Cluster struct {
	Schema      int        `toml:"schema"`
	ClusterName string     `toml:"name"`
	Domain      string     `toml:"domain,omitempty"`
	ACMEEmail   string     `toml:"acme_email,omitempty"`
	Timezone    string     `toml:"timezone"`
	Transport   Transport  `toml:"transport"`
	Nodes       []NodeSpec `toml:"nodes"`
	Server      Server     `toml:"server"`
	Network     Network    `toml:"network"`
}

// Transport is [transport].
type Transport struct {
	Kind       string `toml:"kind"`
	Subnet     string `toml:"subnet"`
	ListenPort int    `toml:"listen_port"`
}

// NodeSpec is one [[nodes]] entry.
type NodeSpec struct {
	Name          string `toml:"name"`
	Role          string `toml:"role"`
	PublicAddress string `toml:"public_address"`
	WGAddress     string `toml:"wg_address"`
}

// Server is [server].
type Server struct {
	AdminUser     string `toml:"admin_user"`
	SwapMB        int    `toml:"swap_mb"`
	DataDirectory string `toml:"data_directory"`
}

// Network is [network].
type Network struct {
	ClientAllowlist []string `toml:"client_allowlist"`
	ExposePgbouncer bool     `toml:"expose_pgbouncer"`
	ExposePostgres  bool     `toml:"expose_postgres"`
}

// Name returns the cluster name.
func (c Cluster) Name() string { return c.ClusterName }

// DefaultCluster returns a cluster.toml with every Phase 1 default set.
func DefaultCluster() Cluster {
	return Cluster{
		Schema:    ClusterSchema,
		Timezone:  DefaultTimezone,
		Transport: Transport{Kind: "wireguard", Subnet: DefaultSubnet, ListenPort: DefaultListenPort},
		Server:    Server{AdminUser: DefaultAdminUser, SwapMB: DefaultSwapMB, DataDirectory: DefaultDataDirectory},
		Network:   Network{ClientAllowlist: []string{}, ExposePgbouncer: true},
	}
}

// Primary returns the node with role primary.
func (c Cluster) Primary() (NodeSpec, bool) {
	for _, n := range c.Nodes {
		if n.Role == RolePrimary {
			return n, true
		}
	}
	return NodeSpec{}, false
}

// Node returns the node called name.
func (c Cluster) Node(name string) (NodeSpec, bool) {
	for _, n := range c.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return NodeSpec{}, false
}

// WGPrefixLen is the prefix length of [transport] subnet, used for the wg0
// interface address.
func (c Cluster) WGPrefixLen() int {
	p, err := netip.ParsePrefix(c.Transport.Subnet)
	if err != nil {
		return 16
	}
	return p.Bits()
}

// LoadCluster reads path. A missing file returns cluster_not_initialized. A
// file that does not parse or validate returns config_error.
func LoadCluster(path string) (Cluster, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed cluster config path.
	if err != nil {
		if IsNotExist(err) {
			return Cluster{}, output.NewError(
				output.CodeClusterNotInitialized,
				"this node has no cluster.toml",
				output.ExitContext,
				"Initialise the cluster first with eru cluster init.",
			).WithDetails(map[string]any{"path": path})
		}
		return Cluster{}, configError(path, err)
	}
	cfg, err := ParseCluster(raw)
	if err != nil {
		return Cluster{}, configError(path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Cluster{}, configError(path, err)
	}
	return cfg, nil
}

// ParseCluster decodes raw over the defaults.
func ParseCluster(raw []byte) (Cluster, error) {
	cfg := DefaultCluster()
	if err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&cfg); err != nil {
		return Cluster{}, err
	}
	if cfg.Network.ClientAllowlist == nil {
		cfg.Network.ClientAllowlist = []string{}
	}
	return cfg, nil
}

// Encode returns cluster.toml bytes.
func (c Cluster) Encode() ([]byte, error) {
	raw, err := toml.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte("# managed by eru: change it with eru config set or eru config edit\n"), raw...), nil
}

var (
	// NamePattern is the cluster name format; it is also the pgBackRest stanza.
	NamePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	nodeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)
	userPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	timezonePattern = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)
	fqdnPattern     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// Validate checks the Phase 1 fields (§6.1). Phase 2 extends it to the full
// schema.
func (c Cluster) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if c.Schema != ClusterSchema {
		add("schema must be %d", ClusterSchema)
	}
	if !NamePattern.MatchString(c.ClusterName) {
		add("name %q must match %s", c.ClusterName, NamePattern)
	}
	if c.Domain != "" && !fqdnPattern.MatchString(c.Domain) {
		add("domain %q is not a fully qualified domain name", c.Domain)
	}
	if c.ACMEEmail != "" && !strings.Contains(c.ACMEEmail, "@") {
		add("acme_email %q is not an email address", c.ACMEEmail)
	}
	if !timezonePattern.MatchString(c.Timezone) {
		add("timezone %q is not a timezone name", c.Timezone)
	}
	if c.Transport.Kind != "wireguard" {
		add("transport.kind must be wireguard until Phase 9")
	}
	subnet, err := netip.ParsePrefix(c.Transport.Subnet)
	if err != nil || !subnet.Addr().Is4() {
		add("transport.subnet %q is not an IPv4 prefix", c.Transport.Subnet)
	}
	if c.Transport.ListenPort < 1 || c.Transport.ListenPort > 65535 {
		add("transport.listen_port must be 1-65535")
	}
	if !userPattern.MatchString(c.Server.AdminUser) || c.Server.AdminUser == "root" {
		add("server.admin_user %q must be a lower-case user name other than root", c.Server.AdminUser)
	}
	if c.Server.SwapMB < 0 || c.Server.SwapMB > 65536 {
		add("server.swap_mb must be 0-65536")
	}
	if !strings.HasPrefix(c.Server.DataDirectory, "/") {
		add("server.data_directory must be absolute")
	}
	for _, cidr := range c.Network.ClientAllowlist {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			add("network.client_allowlist entry %q is not a CIDR", cidr)
		}
	}
	primaries := 0
	names := map[string]bool{}
	for i, n := range c.Nodes {
		if !nodeNamePattern.MatchString(n.Name) {
			add("nodes[%d].name %q must match %s", i, n.Name, nodeNamePattern)
		}
		if names[n.Name] {
			add("nodes[%d].name %q is a duplicate", i, n.Name)
		}
		names[n.Name] = true
		if n.Role == RolePrimary {
			primaries++
		} else {
			add("nodes[%d].role %q: only primary exists until Phase 9", i, n.Role)
		}
		if _, err := netip.ParseAddr(n.PublicAddress); err != nil {
			add("nodes[%d].public_address %q is not an IP address", i, n.PublicAddress)
		}
		wg, err := netip.ParseAddr(n.WGAddress)
		if err != nil || (subnet.IsValid() && !subnet.Contains(wg)) {
			add("nodes[%d].wg_address %q is not inside %s", i, n.WGAddress, c.Transport.Subnet)
		}
	}
	if primaries != 1 {
		add("exactly one node with role primary is required (multi-node arrives in Phase 9)")
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// ValidationError wraps a Validate failure as validation_failed.
func ValidationError(err error) *output.CLIError {
	return output.NewError(
		output.CodeValidation,
		"cluster settings are invalid: "+err.Error(),
		output.ExitConfig,
		"Fix the values named in the message.",
	)
}
