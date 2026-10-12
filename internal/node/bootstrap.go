package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/steps"
	"golang.org/x/crypto/ssh"
)

// Bootstrap phases. PhaseHost runs steps 1-6; PhaseDatabase runs steps 7-13
// (D1).
const (
	PhaseHost     = "host"
	PhaseDatabase = "database"
)

// minRAMMiB is the preflight RAM floor. A "1 GB" VPS reports a little under
// 1024 MiB of MemTotal, so the floor sits just below it.
const minRAMMiB = 900

type bootstrapParams struct {
	Phase       string          `json:"phase"`
	ClusterTOML string          `json:"cluster_toml"`
	NodeName    string          `json:"node_name"`
	Operator    config.Operator `json:"operator"`
	// Local reports a `cluster init --local` run, so the failure hint can name
	// the matching resume command.
	Local bool `json:"local,omitempty"`
}

// BootstrapResult is the cluster.bootstrap result: the node identity the
// operator pins and puts into the secrets document.
type BootstrapResult struct {
	Phase       string           `json:"phase"`
	Node        string           `json:"node"`
	OS          hostinfo.OS      `json:"os"`
	WGPublicKey string           `json:"wg_public_key"`
	SSHHostKeys []string         `json:"ssh_host_keys"`
	PublicIPv6  string           `json:"public_ipv6,omitempty"`
	Steps       engine.ApplyData `json:"steps"`
	// TLS is set by the database phase (D10).
	TLS *TLSState `json:"tls,omitempty"`
	// RestartPerformed reports whether the database phase restarted Postgres.
	RestartPerformed bool `json:"restart_performed,omitempty"`
}

// TLSState is the certificate state after the database phase (D10).
type TLSState struct {
	State    string `json:"state"` // acme | pending | self-signed
	NotAfter string `json:"not_after,omitempty"`
}

// bootstrapToken is {cluster}, read from the params because cluster.toml does
// not exist yet.
func bootstrapToken(_ config.Cluster, raw json.RawMessage) string {
	var p bootstrapParams
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	cfg, err := config.ParseCluster([]byte(p.ClusterTOML))
	if err != nil {
		return ""
	}
	return cfg.Name()
}

func handleClusterBootstrap(ctx context.Context, env *Env) (Result, error) {
	var p bootstrapParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	switch p.Phase {
	case PhaseHost:
		return bootstrapHost(ctx, env, p)
	case PhaseDatabase:
		return bootstrapDatabase(ctx, env, p)
	default:
		return Result{}, output.NewError(
			output.CodeUsage,
			"cluster.bootstrap phase "+p.Phase+" is not available",
			output.ExitUsage,
			"Use phase host or phase database.",
		).WithDetails(map[string]any{"phase": p.Phase})
	}
}

func bootstrapHost(ctx context.Context, env *Env, p bootstrapParams) (Result, error) {
	cfg, err := config.ParseCluster([]byte(p.ClusterTOML))
	if err != nil {
		return Result{}, output.NewError(output.CodeConfig, "cluster_toml does not parse: "+err.Error(), output.ExitConfig, "")
	}
	if err := cfg.ValidateFor(config.ScopeHost); err != nil {
		return Result{}, config.ValidationError(err)
	}
	spec, ok := cfg.Node(p.NodeName)
	if !ok {
		return Result{}, output.NewError(output.CodeValidation, "node "+p.NodeName+" is not in cluster_toml", output.ExitConfig, "")
	}
	if err := validateOperator(p.Operator); err != nil {
		return Result{}, err
	}
	osInfo, err := bootstrapPreflight(cfg)
	if err != nil {
		return Result{}, err
	}
	if err := writeBootstrapFiles(cfg, spec, p.Operator); err != nil {
		return Result{}, err
	}
	env.Cluster = cfg

	data, _, err := engine.Apply(ctx, steps.Catalog(), cfg, runtimeFor(env, false), engine.Options{
		Only:        steps.HostStepIDs(),
		Report:      env.Report,
		FailureHint: initFailureHint(cfg.ClusterName, p.Local),
	})
	if err != nil {
		return Result{}, err
	}
	pub, err := steps.WGPublicKey()
	if err != nil {
		return Result{}, err
	}
	hostKeys, err := sshHostKeys()
	if err != nil {
		return Result{}, err
	}
	host, _ := hostinfo.Detect(cfg.Server.DataDirectory)
	return Result{Data: BootstrapResult{
		Phase:       p.Phase,
		Node:        spec.Name,
		OS:          osInfo,
		WGPublicKey: pub,
		SSHHostKeys: hostKeys,
		PublicIPv6:  host.PublicIPv6,
		Steps:       data,
	}}, nil
}

func validateOperator(op config.Operator) error {
	var bad []string
	if strings.TrimSpace(op.Name) == "" {
		bad = append(bad, "name")
	}
	if _, err := netip.ParseAddr(op.WGAddress); err != nil {
		bad = append(bad, "wg_address")
	}
	if raw, err := base64.StdEncoding.DecodeString(op.WGPublicKey); err != nil || len(raw) != 32 {
		bad = append(bad, "wg_public_key")
	}
	if _, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(op.SSHPublicKey)); err != nil || strings.TrimSpace(string(rest)) != "" {
		bad = append(bad, "ssh_public_key")
	}
	if len(bad) == 0 {
		return nil
	}
	return output.NewError(
		output.CodeValidation,
		"operator is invalid: "+strings.Join(bad, ", "),
		output.ExitConfig,
		"",
	).WithDetails(map[string]any{"invalid": bad})
}

// bootstrapPreflight is the node-side preflight (MASTER §6.1): a supported OS,
// enough RAM, and either no eru node yet or a node of the same cluster.
func bootstrapPreflight(cfg config.Cluster) (hostinfo.OS, error) {
	osInfo, err := hostinfo.ReadOS()
	if err != nil || !osInfo.Supported() {
		return osInfo, output.NewError(
			output.CodePrecondition,
			"this server runs "+osName(osInfo)+", which eru does not support",
			output.ExitPrecondition,
			"Use "+hostinfo.SupportedList+".",
		).WithDetails(map[string]any{"reason": "os_unsupported", "os": osInfo})
	}
	if mib, err := hostinfo.MemTotalMiB(); err == nil && mib < minRAMMiB {
		return osInfo, output.NewError(
			output.CodePrecondition,
			fmt.Sprintf("this server has %d MiB of RAM; eru needs at least 1 GB", mib),
			output.ExitPrecondition,
			"Resize the VPS and retry.",
		).WithDetails(map[string]any{"reason": "ram_too_small", "ram_mib": mib})
	}
	if err := sshdOnPort22(); err != nil {
		return osInfo, err
	}
	existing, err := config.LoadNode(config.NodePath)
	switch {
	case err == nil && existing.Cluster != cfg.Name():
		return osInfo, output.NewError(
			output.CodePrecondition,
			"this server already belongs to cluster "+existing.Cluster,
			output.ExitPrecondition,
			"Bootstrap a fresh server, or use the cluster's own secrets document.",
		).WithDetails(map[string]any{"reason": "node_belongs_to_other_cluster", "cluster": existing.Cluster})
	case err == nil:
		return osInfo, nil
	case !config.IsNotExist(err):
		return osInfo, err
	}
	for _, dir := range []string{"/etc/postgresql", "/var/lib/postgresql"} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return osInfo, output.NewError(
				output.CodePrecondition,
				"PostgreSQL is already installed on this server",
				output.ExitPrecondition,
				"Bootstrap a fresh server. eru will not adopt an existing PostgreSQL.",
			).WithDetails(map[string]any{"reason": "postgres_present", "path": dir})
		}
	}
	return osInfo, nil
}

// sshdOnPort22 checks that sshd serves port 22: the lockdown moves SSH to
// <wg_address>:22, and a revert reopens public 22/tcp.
func sshdOnPort22() error {
	out, err := steps.Probe(context.Background(), "sshd", "-T")
	if err != nil {
		// sshd -T needs /run/sshd on some releases; the hardening step creates
		// it, so an unreadable config is not a reason to refuse here.
		return nil
	}
	var ports []string
	for _, line := range strings.Split(out, "\n") {
		if port, ok := strings.CutPrefix(line, "port "); ok {
			ports = append(ports, port)
			if port == "22" {
				return nil
			}
		}
	}
	return output.NewError(
		output.CodePrecondition,
		"sshd does not listen on port 22 (ports: "+strings.Join(ports, ", ")+")",
		output.ExitPrecondition,
		"eru serves SSH on port 22 over WireGuard. Add Port 22 to the server's sshd config, then retry.",
	).WithDetails(map[string]any{"reason": "sshd_port_not_22", "ports": ports})
}

func osName(o hostinfo.OS) string {
	if o.Pretty != "" {
		return o.Pretty
	}
	if o.ID == "" {
		return "an unknown OS"
	}
	return o.ID + " " + o.VersionID
}

// writeBootstrapFiles writes cluster.toml, node.toml and the operator into
// operators.toml. A re-run keeps installed_at and the other operators.
func writeBootstrapFiles(cfg config.Cluster, spec config.NodeSpec, op config.Operator) error {
	if err := os.MkdirAll(filepath.Dir(config.ClusterPath), 0o755); err != nil { //nolint:gosec // G301: §6.2 /etc/eru is world-listable; files inside are 0640.
		return err
	}
	clusterRaw, err := cfg.Encode()
	if err != nil {
		return err
	}
	node := config.Node{Cluster: cfg.Name(), NodeName: spec.Name, Role: spec.Role}
	if existing, err := config.LoadNode(config.NodePath); err == nil {
		node.InstalledAt = existing.InstalledAt
	} else {
		node.InstalledAt = time.Now().UTC().Truncate(time.Second)
	}
	nodeRaw, err := config.EncodeNode(node)
	if err != nil {
		return err
	}
	ops, err := config.LoadOperators(config.OperatorsPath)
	if err != nil {
		return err
	}
	ops.Upsert(op)
	opsRaw, err := ops.Encode()
	if err != nil {
		return err
	}
	for path, raw := range map[string][]byte{
		config.ClusterPath:   clusterRaw,
		config.NodePath:      nodeRaw,
		config.OperatorsPath: opsRaw,
	} {
		if err := engine.WriteFileAtomic(path, raw, 0o640); err != nil {
			return err
		}
	}
	return nil
}

// sshHostKeys returns the server's public host keys as "type base64" lines.
func sshHostKeys() ([]string, error) {
	paths, err := filepath.Glob("/etc/ssh/ssh_host_*_key.pub")
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var keys []string
	for _, path := range paths {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed host key glob.
		if err != nil {
			return nil, err
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey(raw)
		if err != nil {
			continue
		}
		keys = append(keys, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no SSH host keys under /etc/ssh")
	}
	return keys, nil
}

func handleConfirmSecrets(context.Context, *Env) (Result, error) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return Result{}, err
	}
	st.SecretsConfirmedAt = time.Now().UTC().Format(time.RFC3339)
	if err := st.Save(engine.StatePath); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]string{"secrets_confirmed_at": st.SecretsConfirmedAt}}, nil
}

// initFailureHint is the step_failed hint during cluster init, where there is
// no context yet for eru apply. local names the resume command for the mode
// that started the init, so a remote init is not told to add --local.
func initFailureHint(cluster string, local bool) string {
	command := "eru cluster init --resume --name " + cluster
	if local {
		command += " --local"
	}
	return "Fix the cause, then run " + command + ". It resumes at this step."
}
