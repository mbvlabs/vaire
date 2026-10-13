package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

const wireguardID = "wireguard-v1"

// WireGuard files on the node.
const (
	WGPrivateKeyPath = "/etc/wireguard/eru.key"
	WGPublicKeyPath  = "/etc/wireguard/eru.pub"
	WGConfigPath     = "/etc/wireguard/wg0.conf"
)

type wireguard struct{}

func (wireguard) ID() string { return wireguardID }

func (wireguard) Describe(cfg config.Cluster) string {
	return fmt.Sprintf("bring up wg0 on UDP %d with the operator peers", cfg.Transport.ListenPort)
}

type wgPeer struct {
	Name       string `json:"name"`
	PublicKey  string `json:"public_key"`
	AllowedIPs string `json:"allowed_ips"`
}

type wgRender struct {
	Address    string   `json:"address"`
	ListenPort int      `json:"listen_port"`
	Peers      []wgPeer `json:"peers"`
}

// thisNode returns this host's [[nodes]] entry: the node.toml name when
// present, else the primary.
func thisNode(cfg config.Cluster) (config.NodeSpec, error) {
	if n, err := config.LoadNode(config.NodePath); err == nil {
		if spec, ok := cfg.Node(n.NodeName); ok {
			return spec, nil
		}
	}
	if spec, ok := cfg.Master(); ok {
		return spec, nil
	}
	return config.NodeSpec{}, ErrNoConfig
}

// NodeHostname is this node's one hostname, <node>.<domain> (RFC 8.4): the
// certificate's only name, nginx's server_name and what clients dial. It is
// empty without a domain.
func NodeHostname(cfg config.Cluster) string {
	node, err := thisNode(cfg)
	if err != nil {
		return ""
	}
	return cfg.Hostname(node.Name)
}

func renderWG(cfg config.Cluster) (wgRender, error) {
	node, err := thisNode(cfg)
	if err != nil {
		return wgRender{}, err
	}
	ops, err := config.LoadOperators(config.OperatorsPath)
	if err != nil {
		return wgRender{}, err
	}
	if len(ops.Operators) == 0 {
		return wgRender{}, errors.New("operators.toml lists no operator peer")
	}
	r := wgRender{
		Address:    node.WGAddress + "/" + strconv.Itoa(cfg.WGPrefixLen()),
		ListenPort: cfg.Transport.ListenPort,
	}
	for _, op := range ops.Operators {
		r.Peers = append(r.Peers, wgPeer{Name: op.Name, PublicKey: op.WGPublicKey, AllowedIPs: op.WGAddress + "/32"})
	}
	return r, nil
}

func (r wgRender) config(privateKey string) string {
	var b strings.Builder
	b.WriteString("# managed by eru: peers come from " + config.OperatorsPath + "\n")
	b.WriteString("[Interface]\n")
	b.WriteString("PrivateKey = " + privateKey + "\n")
	b.WriteString("Address = " + r.Address + "\n")
	b.WriteString("ListenPort = " + strconv.Itoa(r.ListenPort) + "\n")
	for _, p := range r.Peers {
		b.WriteString("\n# " + p.Name + "\n[Peer]\n")
		b.WriteString("PublicKey = " + p.PublicKey + "\n")
		b.WriteString("AllowedIPs = " + p.AllowedIPs + "\n")
	}
	return b.String()
}

func (wireguard) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	r, err := renderWG(cfg)
	if err != nil {
		return nil, err
	}
	return canonical(r)
}

func readWGPrivateKey() (string, error) {
	raw, err := os.ReadFile(WGPrivateKeyPath)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", errors.New(WGPrivateKeyPath + " is empty")
	}
	return key, nil
}

func (wireguard) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	r, err := renderWG(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	key, err := readWGPrivateKey()
	if err != nil {
		return incomplete("no node WireGuard key"), nil
	}
	if !fileIs(WGConfigPath, r.config(key), 0o600) {
		return incomplete(WGConfigPath + " differs"), nil
	}
	peers, err := Probe(ctx, "wg", "show", "wg0", "peers")
	if err != nil {
		return incomplete("wg0 is down"), nil
	}
	for _, p := range r.Peers {
		if !strings.Contains(peers, p.PublicKey) {
			return incomplete("wg0 lacks peer " + p.Name), nil
		}
	}
	return complete(), nil
}

func (s wireguard) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	r, err := renderWG(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := rt.Shell.Run(ctx, s.ID(), Script("wireguard-key"), map[string]string{
		"WG_PRIVATE_KEY_PATH": WGPrivateKeyPath,
		"WG_PUBLIC_KEY_PATH":  WGPublicKeyPath,
	}, report); err != nil {
		return engine.ApplyResult{}, err
	}
	key, err := readWGPrivateKey()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	rt.Secrets.Add(key)
	if err := writeFile(WGConfigPath, r.config(key), 0o600); err != nil {
		return engine.ApplyResult{}, err
	}
	err = rt.Shell.Run(ctx, s.ID(), Script("wireguard-up"), map[string]string{
		"WG_LISTEN_PORT": strconv.Itoa(r.ListenPort),
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

// WGPublicKey reads the node's WireGuard public key.
func WGPublicKey() (string, error) {
	raw, err := os.ReadFile(WGPublicKeyPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
