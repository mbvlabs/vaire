package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/secretsdoc"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// Init phases recorded in pending/<name>/progress.json (§8.2). --resume
// continues after the last completed one.
const (
	phaseSecrets     = "secrets_generated"
	phaseFirstHost   = "first_contact"
	phaseBinary      = "binary_installed"
	phaseHost        = "host_bootstrapped"
	phaseTransport   = "transport_switched"
	phaseLockdown    = "locked_down"
	phaseDNS         = "dns_confirmed"
	phaseDatabase    = "database_bootstrapped"
	phaseTLS         = "tls_checked"
	phaseDocument    = "secrets_written"
	phaseSecretsDone = "secrets_confirmed"
)

// Lockdown timing (§8.3, §8.5).
const (
	lockdownTimeout      = 120 * time.Second
	localLockdownTimeout = 30 * time.Minute
	confirmWithin        = 60 * time.Second
	revertGrace          = 15 * time.Second
	firstOperatorName    = "op-1"
)

// laterPhaseFlags are init flags whose settings arrive with a later phase.
// The database backup flags work from Phase 3 on; VPS backups arrive in
// Phase 7.
var laterPhaseFlags = []string{"vps-backup-schedule"}

// tlsWaitTimeout bounds the wait for a real certificate (D10).
const tlsWaitTimeout = 120 * time.Second

// initRun is one server init: the answers, the pending state, and the
// generated secrets.
type initRun struct {
	cmd      *cobra.Command
	p        *progress
	local    bool
	name     string
	cfg      config.Cluster
	boot     contexts.Bootstrap
	prog     *contexts.Progress
	sec      contexts.PendingSecrets
	warnings []output.Warning
	// dbSecrets is the RPC secret map for the database phase (D7).
	dbSecrets map[string]string
	// tlsState is the certificate state after the database phase (D10).
	tlsState *node.TLSState
	// dbRestart reports whether the database phase restarted Postgres.
	dbRestart bool
	// dnsShown reports whether this run already showed the DNS records
	// (with dnsIPv6 as the AAAA address) after first contact, so the DNS
	// step doesn't show or ask again.
	dnsShown bool
	dnsIPv6  string
	// backup holds the bucket answers of this run; nil when skipped or on
	// --resume, where they come from the pending state.
	backup *backupSettings
	// backupsDone reports whether this run configured backups.
	backupsDone bool
}

type initPlanData struct {
	Server        string   `json:"server"`
	Node          string   `json:"node"`
	Host          string   `json:"host,omitempty"`
	PublicAddress string   `json:"public_address"`
	WGAddress     string   `json:"wg_address"`
	AdminUser     string   `json:"admin_user"`
	Local         bool     `json:"local"`
	Resume        bool     `json:"resume"`
	Done          []string `json:"done,omitempty"`
	Steps         []string `json:"steps"`
	DryRun        bool     `json:"dry_run"`
}

type initResult struct {
	Server           string   `json:"server"`
	Node             string   `json:"node"`
	PublicAddress    string   `json:"public_address"`
	WGAddress        string   `json:"wg_address"`
	AdminUser        string   `json:"admin_user"`
	Context          string   `json:"context,omitempty"`
	SecretsDocument  string   `json:"secrets_document,omitempty"`
	Passphrase       string   `json:"passphrase,omitempty"`
	PublicSSHClosed  bool     `json:"public_ssh_closed"`
	LockdownPending  bool     `json:"lockdown_pending"`
	LockdownDeadline string   `json:"lockdown_confirm_by,omitempty"`
	LoginCommand     string   `json:"login_command,omitempty"`
	TLS              string   `json:"tls,omitempty"`
	TLSNotAfter      string   `json:"tls_not_after,omitempty"`
	RestartPerformed bool     `json:"restart_performed,omitempty"`
	Next             []string `json:"next,omitempty"`
}

func runServerInit(cmd *cobra.Command, args []string) error {
	if flagChanged(cmd, "restore") || flagChanged(cmd, "restore-time") {
		return output.NewError(
			output.CodeError,
			"eru server init --restore is not implemented yet",
			output.ExitGeneric,
			"Disaster recovery from a secrets document arrives in Phase 7.",
		)
	}
	if flagTrue(cmd, "discard") {
		return runInitDiscard(cmd)
	}
	if flagTrue(cmd, "local") {
		return runInitLocal(cmd)
	}
	return runInitRemote(cmd)
}

// prepareInit collects the answers, or loads them on --resume, checks every
// non-interactive requirement before anything changes, and asks for the go.
func prepareInit(cmd *cobra.Command, local bool) (*initRun, error) {
	r := &initRun{cmd: cmd, p: newProgress(cmd), local: local}
	resume := flagTrue(cmd, "resume")
	if resume {
		if err := r.loadPending(); err != nil {
			return nil, err
		}
	} else if err := r.collect(); err != nil {
		return nil, err
	}
	if err := r.cfg.Validate(); err != nil {
		return nil, config.ValidationError(err)
	}
	if !resume {
		if contexts.Exists(r.name) && !local {
			return nil, output.NewError(
				output.CodePrecondition,
				"a context named "+r.name+" already exists on this machine",
				output.ExitPrecondition,
				"Pick another --name, or remove the context with eru context remove "+r.name+".",
			).WithDetails(map[string]any{"reason": "context_exists", "context": r.name})
		}
		if contexts.PendingExists(r.name) {
			return nil, output.NewError(
				output.CodePrecondition,
				"an unfinished init for "+r.name+" exists",
				output.ExitPrecondition,
				"Continue it with eru server init --resume --name "+r.name+", or drop it with eru server init --discard --name "+r.name+".",
			).WithDetails(map[string]any{"reason": "init_pending", "server": r.name, "pending_path": contexts.PendingDir(r.name)})
		}
	}
	for _, name := range laterPhaseFlags {
		if flagChanged(cmd, name) {
			r.warnings = append(r.warnings, output.Warning{
				Code:    "flag_not_used_yet",
				Message: "--" + name + " is not used yet: VPS backups arrive in Phase 7",
			})
		}
	}
	if dryRun(cmd) {
		return r, nil
	}
	if err := requireStructuredHandoff(cmd, "secrets-out", "server init", "confirm-secrets-stored"); err != nil {
		return nil, err
	}
	if err := r.confirm(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *initRun) confirm() error {
	cmd := r.cmd
	if stringFlag(cmd, "confirm") != "" || MustNotPrompt(cmd) {
		return Confirm(cmd, r.name)
	}
	if flagTrue(cmd, "yes") {
		return nil
	}
	target := "this server"
	if !r.local {
		target = r.boot.User + "@" + r.boot.Host
	}
	ok, err := PromptYes(cmd, fmt.Sprintf("Bootstrap %s as server %s? This hardens the host and closes public SSH.", target, r.name))
	if err != nil {
		return err
	}
	if !ok {
		return output.NewError(output.CodeUnsafeAction, "server init was not confirmed", output.ExitUnsafe, "Re-run with --confirm "+r.name+".").
			WithDetails(map[string]any{"confirm_token": r.name})
	}
	return nil
}

// collect builds the answers from --answers, flags and prompts.
func (r *initRun) collect() error {
	cmd := r.cmd
	cfg := config.DefaultCluster()
	var answersRaw []byte
	if path := stringFlag(cmd, "answers"); path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this answers file.
		answersRaw = raw
		if err != nil {
			return output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
		}
		if cfg, err = config.ParseAnswers(raw); err != nil {
			return output.NewError(output.CodeConfig, path+" does not parse: "+err.Error(), output.ExitConfig, "")
		}
		if len(cfg.Clusters) > 0 {
			return output.NewError(output.CodeValidation, path+" declares [[clusters]]: server init creates no databases", output.ExitConfig,
				"Remove [[clusters]] from the answers file and add each database after init with eru db create <name>.")
		}
		_, r.boot, err = decodeBootstrap(raw)
		if err != nil {
			return output.NewError(output.CodeConfig, path+" [bootstrap] does not parse: "+err.Error(), output.ExitConfig, "")
		}
	}
	if v := stringFlag(cmd, "name"); v != "" {
		cfg.ClusterName = v
	}
	if v := stringFlag(cmd, "domain"); v != "" {
		cfg.Domain = v
	}
	if v := stringFlag(cmd, "admin-user"); v != "" {
		cfg.Server.AdminUser = v
	}
	if v := stringFlag(cmd, "timezone"); v != "" {
		cfg.Timezone = v
	}
	if v := stringFlag(cmd, "data-root"); v != "" {
		cfg.Server.DataRoot = v
	}
	if v := stringFlag(cmd, "acme-email"); v != "" {
		cfg.ACMEEmail = v
	}
	if flagChanged(cmd, "allow") {
		allow, _ := cmd.Flags().GetStringSlice("allow")
		cfg.Network.ClientAllowlist = allow
	}
	if v := stringFlag(cmd, "profile"); v != "" {
		cfg.Postgres.Profile = v
	}
	if v := stringFlag(cmd, "default-size"); v != "" {
		cfg.Postgres.DefaultSize = v
	}
	if flagChanged(cmd, "client-connections") {
		if n, err := cmd.Flags().GetInt("client-connections"); err == nil {
			cfg.Postgres.ClientConnections = n
		}
	}
	if v := stringFlag(cmd, "tls-mode"); v != "" {
		cfg.TLS.Mode = v
	}
	// An answers file's [bootstrap] user and port count as explicit.
	userGiven, portGiven := r.boot.User != "", r.boot.Port != 0
	if v := stringFlag(cmd, "host"); v != "" {
		user, host, port, err := parseHostFlag(v)
		if err != nil {
			return err
		}
		r.boot.User, r.boot.Host, r.boot.Port = user, host, port
		userGiven, portGiven = hostFlagExplicit(v)
	}
	if v := stringFlag(cmd, "identity"); v != "" {
		r.boot.Identity = v
	}
	if v := stringFlag(cmd, "host-key-fingerprint"); v != "" {
		r.boot.HostKeyFingerprint = v
	}
	if err := checkIdentity(r.boot.Identity); err != nil {
		return err
	}

	if !r.local && r.boot.Host == "" {
		if err := RequireFlag(cmd, false, "--host", "server init"); err != nil {
			return err
		}
		v, err := promptLine(cmd, "Server to bootstrap (user@host, or a Host from ~/.ssh/config)", "")
		if err != nil {
			return err
		}
		user, host, port, err := parseHostFlag(v)
		if err != nil {
			return err
		}
		r.boot.User, r.boot.Host, r.boot.Port = user, host, port
		userGiven, portGiven = hostFlagExplicit(v)
	}
	if !r.local {
		if src := applySSHConfig(&r.boot, userGiven, portGiven, r.boot.Identity != ""); src != "" {
			r.p.note("Using " + src + " for first contact.")
		}
	}
	if r.boot.User == "" {
		r.boot.User = "root"
	}
	if r.boot.Port == 0 {
		r.boot.Port = 22
	}
	if cfg.ClusterName == "" {
		if err := RequireFlag(cmd, false, "--name", "server init"); err != nil {
			return err
		}
		v, err := promptLine(cmd, "Server name", "prod")
		if err != nil {
			return err
		}
		cfg.ClusterName = v
	}
	if !config.NamePattern.MatchString(cfg.ClusterName) {
		return output.NewError(output.CodeValidation, "server name "+cfg.ClusterName+" must match "+config.NamePattern.String(), output.ExitConfig, "")
	}
	r.name = cfg.ClusterName

	public := ""
	if len(cfg.Nodes) > 0 {
		public = cfg.Nodes[0].PublicAddress
	}
	if public == "" {
		addr, err := r.publicAddress()
		if err != nil {
			return err
		}
		public = addr
	}
	if len(cfg.Nodes) == 0 {
		cfg.Nodes = []config.NodeSpec{{Role: config.RoleMaster, WGAddress: config.DefaultCentralWG}}
	}
	cfg.Nodes[0].PublicAddress = public
	if v := stringFlag(cmd, "node-name"); v != "" {
		cfg.Nodes[0].Name = v
	}
	if cfg.Nodes[0].Name == "" {
		cfg.Nodes[0].Name = config.DefaultNodeName
	}
	if cfg.Nodes[0].WGAddress == "" {
		cfg.Nodes[0].WGAddress = config.DefaultCentralWG
	}
	if cfg.Nodes[0].Role == "" {
		cfg.Nodes[0].Role = config.RoleMaster
	}
	supplied := suppliedAnswers(cmd, cfg, stringFlag(cmd, "answers") != "")
	// The huh interview asks the Phase 2 question set in a TTY (D13).
	if !MustNotPrompt(cmd) {
		if err := runInterview(cmd, &cfg, supplied); err != nil {
			return err
		}
	}
	if err := r.collectBackups(&cfg, answersRaw); err != nil {
		return err
	}
	r.cfg = cfg
	r.prog = &contexts.Progress{Local: r.local, Phases: map[string]string{}}
	return nil
}

func decodeBootstrap(raw []byte) ([]byte, contexts.Bootstrap, error) {
	var wrap struct {
		Bootstrap contexts.Bootstrap `toml:"bootstrap"`
	}
	if err := toml.Unmarshal(raw, &wrap); err != nil {
		return nil, contexts.Bootstrap{}, err
	}
	return raw, wrap.Bootstrap, nil
}

// publicAddress resolves the node's public IP: the --host for a remote init,
// the default-route source address for --local.
func (r *initRun) publicAddress() (string, error) {
	host := r.boot.Host
	if r.local && host == "" {
		addr, err := defaultRouteAddress()
		if err != nil {
			return "", output.NewError(output.CodeInputRequired, "could not detect this server's public address", output.ExitInput,
				"Pass it with --host <public-ip>.").WithDetails(map[string]any{"flag": "--host"})
		}
		if addr.IsPrivate() {
			r.warnings = append(r.warnings, output.Warning{
				Code:    "public_address_private",
				Message: "the detected address " + addr.String() + " is private; pass --host <public-ip> if operators connect through NAT",
			})
		}
		return addr.String(), nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String(), nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(commandContext(r.cmd), "ip", host)
	if err != nil || len(ips) == 0 {
		return "", output.NewError(output.CodeRemoteUnreachable, "cannot resolve "+host, output.ExitTransport, "Pass the server's IP address in --host.")
	}
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			return ip.Unmap().String(), nil
		}
	}
	return ips[0].String(), nil
}

func defaultRouteAddress() (netip.Addr, error) {
	conn, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		return netip.Addr{}, err
	}
	defer func() { _ = conn.Close() }()
	udp, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, errors.New("no local UDP address")
	}
	addr, ok := netip.AddrFromSlice(udp.IP)
	if !ok {
		return netip.Addr{}, errors.New("no local UDP address")
	}
	return addr.Unmap(), nil
}

// parseHostFlag parses user@host[:port]; IPv6 needs brackets with a port.
func parseHostFlag(v string) (user, host string, port int, err error) {
	user = "root"
	rest := strings.TrimSpace(v)
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		user, rest = rest[:at], rest[at+1:]
	}
	port = 22
	if h, p, splitErr := net.SplitHostPort(rest); splitErr == nil {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 1 || n > 65535 {
			return "", "", 0, output.NewError(output.CodeUsage, "--host port "+p+" is invalid", output.ExitUsage, "")
		}
		rest, port = h, n
	}
	rest = strings.Trim(rest, "[]")
	if user == "" || rest == "" {
		return "", "", 0, output.NewError(output.CodeUsage, "--host must be user@host[:port]", output.ExitUsage, "For example --host root@203.0.113.10.")
	}
	return user, rest, port, nil
}

// loadPending restores an unfinished init for --resume.
func (r *initRun) loadPending() error {
	name := stringFlag(r.cmd, "name")
	if name == "" {
		pending := contexts.PendingNames()
		switch len(pending) {
		case 0:
			return output.NewError(output.CodePrecondition, "there is no unfinished init to resume", output.ExitPrecondition,
				"Start one with eru server init.").WithDetails(map[string]any{"reason": "nothing_to_resume"})
		case 1:
			name = pending[0]
		default:
			return output.NewError(output.CodeAmbiguous, "several unfinished inits exist", output.ExitInput,
				"Pass --name with one of details.pending.").WithDetails(map[string]any{"pending": pending})
		}
	}
	if !contexts.PendingExists(name) {
		return output.NewError(output.CodePrecondition, "there is no unfinished init for "+name, output.ExitPrecondition,
			"Start one with eru server init.").WithDetails(map[string]any{"reason": "nothing_to_resume"})
	}
	raw, boot, err := contexts.LoadAnswers(name)
	if err != nil {
		return err
	}
	cfg, err := config.ParseAnswers(raw)
	if err != nil {
		return output.NewError(output.CodeConfig, "the pending answers do not parse: "+err.Error(), output.ExitConfig, "")
	}
	prog, err := contexts.LoadProgress(name)
	if err != nil {
		return err
	}
	if prog.Local != r.local {
		return output.NewError(output.CodeUsage, "the unfinished init of "+name+" was started with a different --local setting", output.ExitUsage, "")
	}
	// First-contact credentials may be passed again on --resume.
	if v := stringFlag(r.cmd, "identity"); v != "" {
		boot.Identity = v
	}
	if v := stringFlag(r.cmd, "host-key-fingerprint"); v != "" {
		boot.HostKeyFingerprint = v
	}
	if !prog.Done(phaseHost) {
		if err := checkIdentity(boot.Identity); err != nil {
			return err
		}
	}
	r.name, r.cfg, r.boot, r.prog = name, cfg, boot, prog
	if prog.Done(phaseSecrets) {
		sec, err := contexts.LoadSecrets(name)
		if err != nil {
			return err
		}
		r.sec = sec
		registerPending(sec)
	}
	return nil
}

func registerPending(sec contexts.PendingSecrets) {
	secrets.Add(sec.WGPrivateKey)
	secrets.Add(sec.SSHPrivateKey)
	secrets.Add(sec.SudoPassword)
	if sec.Database != nil {
		for _, value := range sec.Database.SecretValues() {
			secrets.Add(value)
		}
	}
	if sec.Backup != nil {
		for _, value := range sec.Backup.SecretValues() {
			secrets.Add(value)
		}
	}
}

func (r *initRun) master() config.NodeSpec {
	n, _ := r.cfg.Master()
	return n
}

func (r *initRun) planData() initPlanData {
	n := r.master()
	done := make([]string, 0, len(r.prog.Phases))
	for _, phase := range []string{phaseSecrets, phaseFirstHost, phaseBinary, phaseHost, phaseTransport, phaseLockdown, phaseDNS, phaseDatabase, phaseTLS, phaseBackups, phaseDocument, phaseSecretsDone} {
		if r.prog.Done(phase) {
			done = append(done, phase)
		}
	}
	data := initPlanData{
		Server:        r.name,
		Node:          n.Name,
		PublicAddress: n.PublicAddress,
		WGAddress:     n.WGAddress,
		AdminUser:     r.cfg.Server.AdminUser,
		Local:         r.local,
		Resume:        flagTrue(r.cmd, "resume"),
		Done:          done,
		Steps:         steps.HostStepIDs(),
		DryRun:        true,
	}
	if !r.local {
		data.Host = r.boot.User + "@" + net.JoinHostPort(r.boot.Host, strconv.Itoa(r.boot.Port))
	}
	return data
}

func (r *initRun) dryRunResult() error {
	data := r.planData()
	message := fmt.Sprintf("Would bootstrap %s as node %s, the master of server %s: steps %s, then move SSH to WireGuard %s and show the secrets handoff.",
		map[bool]string{true: "this machine", false: data.Host}[r.local], data.Node, data.Server, strings.Join(data.Steps, ", "), data.WGAddress)
	return output.Success(r.cmd, output.Result{Data: data, Message: message, Warnings: r.warnings})
}

// savePending writes answers.toml and progress.json.
func (r *initRun) savePending() error {
	raw, err := r.cfg.Encode()
	if err != nil {
		return err
	}
	if err := contexts.SaveAnswers(r.name, raw, r.boot); err != nil {
		return err
	}
	return contexts.SaveProgress(r.name, r.prog)
}

func (r *initRun) mark(phase string) error {
	r.prog.Mark(phase)
	return contexts.SaveProgress(r.name, r.prog)
}

// generateSecrets creates the operator identity and sudo password (§8.2 step 3).
func (r *initRun) generateSecrets() error {
	if r.prog.Done(phaseSecrets) {
		return nil
	}
	r.p.phase("init.secrets", "started", "generate the operator WireGuard and SSH keys")
	wgPriv, _, err := transport.GenerateWGKey()
	if err != nil {
		return err
	}
	sshPriv, _, err := transport.GenerateSSHKey("eru-" + r.name + "-" + firstOperatorName)
	if err != nil {
		return err
	}
	sudo, err := secrets.Base62(24)
	if err != nil {
		return err
	}
	r.sec = contexts.PendingSecrets{
		OperatorName:  firstOperatorName,
		WGPrivateKey:  wgPriv,
		SSHPrivateKey: string(sshPriv),
		SudoPassword:  sudo,
	}
	registerPending(r.sec)
	if err := contexts.SaveSecrets(r.name, r.sec); err != nil {
		return err
	}
	r.p.phase("init.secrets", "completed", "")
	return r.mark(phaseSecrets)
}

// ensureDatabaseSecrets fills the operator's eru_admin and service role
// credentials and records the RPC secret map (D7). Init creates no
// application databases.
func (r *initRun) ensureDatabaseSecrets() error {
	creds := contexts.DatabaseCreds{}
	if r.sec.Database != nil {
		creds = *r.sec.Database
	}
	rpc, generated, err := ensureClusterSecrets(&creds)
	if err != nil {
		return err
	}
	r.sec.Database = &creds
	r.dbSecrets = rpc
	r.warnings = append(r.warnings, staleWarning(generated)...)
	registerPending(r.sec)
	return contexts.SaveSecrets(r.name, r.sec)
}

func (r *initRun) operator() (config.Operator, error) {
	pub, err := transport.WGPublicKey(r.sec.WGPrivateKey)
	if err != nil {
		return config.Operator{}, err
	}
	sshPub, err := transport.AuthorizedKey([]byte(r.sec.SSHPrivateKey), "eru-"+r.name+"-"+r.sec.OperatorName)
	if err != nil {
		return config.Operator{}, err
	}
	return config.Operator{Name: r.sec.OperatorName, WGAddress: config.DefaultFirstOperatorWG, WGPublicKey: pub, SSHPublicKey: sshPub}, nil
}

// bootstrapHost runs cluster.bootstrap phase=host (§8.2 step 6) and records
// the node identity.
func (r *initRun) bootstrapHost(ctx context.Context, caller remote.Caller) (node.BootstrapResult, error) {
	op, err := r.operator()
	if err != nil {
		return node.BootstrapResult{}, err
	}
	raw, err := r.nodeConfig().Encode()
	if err != nil {
		return node.BootstrapResult{}, err
	}
	r.p.phase("init.bootstrap", "started", "run host steps 1-6 on the node")
	reply, err := caller.Call(ctx, "cluster.bootstrap", map[string]any{
		"phase":        node.PhaseHost,
		"cluster_toml": string(raw),
		"node_name":    r.master().Name,
		"operator":     op,
		"local":        r.local,
	}, remote.CallOptions{
		Secrets: map[string]string{steps.SecretSudoPassword: r.sec.SudoPassword},
		Confirm: r.name,
		OnEvent: r.p.event,
	})
	if err != nil {
		return node.BootstrapResult{}, err
	}
	var res node.BootstrapResult
	if err := reply.Decode(&res); err != nil {
		return res, err
	}
	r.warnings = append(r.warnings, reply.Warnings...)
	r.prog.WGPublicKey = res.WGPublicKey
	r.prog.SSHHostKeys = res.SSHHostKeys
	r.prog.OS = res.OS.Pretty
	r.p.phase("init.bootstrap", "completed", "")
	return res, nil
}

func runInitRemote(cmd *cobra.Command) error {
	r, err := prepareInit(cmd, false)
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		return r.dryRunResult()
	}
	ctx := commandContext(cmd)
	timeout := timeoutFlag(cmd)
	n := r.master()
	publicSSH := net.JoinHostPort(n.PublicAddress, strconv.Itoa(r.boot.Port))
	if err := r.savePending(); err != nil {
		return err
	}

	// 1. Local preflight: the target answers on SSH (§20.1).
	if !r.prog.Done(phaseHost) {
		if err := localenv.CheckReachable(ctx, publicSSH, timeout); err != nil {
			return err
		}
	}
	// 3. Secrets.
	if err := r.generateSecrets(); err != nil {
		return err
	}
	if err := r.ensureDatabaseSecrets(); err != nil {
		return err
	}
	if err := r.ensureBackupKit(commandContext(cmd)); err != nil {
		return err
	}
	known, err := transport.LoadKnownHosts(contexts.PendingKnownHosts(r.name))
	if err != nil {
		return err
	}
	opAuth, err := transport.KeyAuth([]byte(r.sec.SSHPrivateKey))
	if err != nil {
		return err
	}

	// 5-6. First contact, binary push, host bootstrap over public SSH.
	if !r.prog.Done(phaseHost) {
		if err := r.firstContact(ctx, known, publicSSH, timeout); err != nil {
			return err
		}
	}

	// 7. Switch transport to WireGuard as the admin user.
	r.p.phase("init.transport", "started", "connect over WireGuard to "+n.WGAddress)
	tun, err := upTunnel(r.sec.WGPrivateKey, config.DefaultFirstOperatorWG, r.prog.WGPublicKey, n.PublicAddress, r.cfg.Transport.ListenPort, n.WGAddress, r.p)
	if err != nil {
		return err
	}
	defer tun.Close()
	adminTarget := transport.SSHTarget{User: r.cfg.Server.AdminUser, Addr: net.JoinHostPort(n.WGAddress, "22")}
	sshOpts := transport.SSHOptions{Auth: []ssh.AuthMethod{opAuth}, HostKeys: known, Timeout: 15 * time.Second}
	wgClient, err := dialRetryReport(ctx, tun, adminTarget, sshOpts, confirmWithin, r.attemptReporter("init.transport", "SSH over WireGuard to "+adminTarget.Addr))
	if err != nil {
		return tunnelHint(err, n.PublicAddress, r.cfg.Transport.ListenPort)
	}
	defer wgClient.Close()
	wgNode := &remote.Node{SSH: wgClient, Operator: r.sec.OperatorName}
	if err := wgNode.Handshake(ctx, remote.HandshakeOptions{AutoUpgrade: flagTrue(cmd, "auto-upgrade-nodes"), Progress: r.p.note}); err != nil {
		return err
	}
	r.p.phase("init.transport", "completed", "")
	if err := r.mark(phaseTransport); err != nil {
		return err
	}

	// 8. Commit-confirm lockdown.
	lockedAt := ""
	if !r.prog.Done(phaseLockdown) {
		// A revert reopens public 22/tcp, whatever port first contact used.
		at, err := r.lockdown(ctx, wgNode, tun, adminTarget, sshOpts, net.JoinHostPort(n.PublicAddress, "22"))
		if err != nil {
			return err
		}
		lockedAt = at
		if err := r.mark(phaseLockdown); err != nil {
			return err
		}
	}
	if lockedAt == "" {
		lockedAt = r.prog.Phases[phaseLockdown]
	}

	// 9. DNS (acme mode only).
	if r.cfg.TLS.Mode == "acme" && !r.prog.Done(phaseDNS) {
		if err := r.dnsStep(ctx); err != nil {
			return err
		}
		if err := r.mark(phaseDNS); err != nil {
			return err
		}
	}

	// 10. Database phase: steps 7-13.
	if !r.prog.Done(phaseDatabase) {
		r.p.phase("init.database", "started", "run database steps 7-13 on the node")
		res, err := r.callDatabasePhase(ctx, wgNode)
		if err != nil {
			return err
		}
		r.tlsState, r.dbRestart = res.TLS, res.RestartPerformed
		r.p.phase("init.database", "completed", "")
		if err := r.mark(phaseDatabase); err != nil {
			return err
		}
	}
	// 11. Wait for the certificate.
	if !r.prog.Done(phaseTLS) {
		if err := r.waitForTLS(ctx, wgNode); err != nil {
			return err
		}
		if err := r.mark(phaseTLS); err != nil {
			return err
		}
	}
	// Backups, between the database and the handoff.
	if err := r.configureInitBackups(ctx, wgNode); err != nil {
		return err
	}

	// 12. Secrets handoff, then the node records the confirmation.
	doc, res, err := r.writeDocument()
	if err != nil {
		return err
	}
	if _, err := wgNode.Call(ctx, "cluster.confirm-secrets", nil, remote.CallOptions{OnEvent: r.p.event}); err != nil {
		return err
	}
	if err := r.mark(phaseSecretsDone); err != nil {
		return err
	}

	// 13. Local context, then drop the pending state.
	if contexts.Exists(r.name) {
		return output.NewError(output.CodePrecondition, "a context named "+r.name+" appeared during init", output.ExitPrecondition,
			"Remove it, then run eru login and paste the transfer bundle.")
	}
	if _, err := secretsdoc.Install(doc, r.name, lockedAt); err != nil {
		return err
	}
	if err := contexts.SetCurrent(r.name); err != nil {
		return err
	}
	if err := contexts.RemovePending(r.name); err != nil {
		return err
	}
	res.Context = r.name
	res.PublicSSHClosed = true
	r.fillResult(&res)
	message := strings.Join([]string{
		fmt.Sprintf("Server %s is ready. Node %s (%s) runs PostgreSQL 18 behind PgBouncer at %s:6432.", r.name, n.Name, n.PublicAddress, r.cfg.MasterHostname()),
		"TLS: " + r.tlsSummary(),
		"No application databases yet: create one with eru db create <name>, or point your agent to the CLI (eru guide agents).",
		"Context " + r.name + " is the current context on this machine. On another machine run eru login and paste the transfer bundle.",
	}, "\n")
	r.warnings = append(r.warnings, node.NoOffsiteWarning(r.cfg)...)
	return r.finish(res, message, res.Next, fmt.Sprintf("Server %s is ready", r.name), nil)
}

// finish renders the init result. On a terminal it draws the summary card;
// every other mode gets the plain message and the envelope.
func (r *initRun) finish(res initResult, message string, next []string, title string, notes []string) error {
	cmd := r.cmd
	if r.p.live == nil {
		return output.Success(cmd, output.Result{Data: res, Message: message, Warnings: r.warnings, Next: next})
	}
	n := r.master()
	rows := [][2]string{
		{"Node", fmt.Sprintf("%s  %s  (WireGuard %s)", n.Name, n.PublicAddress, n.WGAddress)},
		{"PgBouncer", r.cfg.MasterHostname() + ":6432"},
		{"Direct TLS", r.cfg.MasterHostname() + ":443  (sslnegotiation=direct)"},
		{"TLS", r.tlsSummary()},
		{"Secrets", "in your password manager (transfer bundle: eru login on another machine)"},
		{"Databases", "none yet: create one with eru db create <name>, or point your agent to the CLI (eru guide agents)"},
		{"Backups", r.backupSummary()},
	}
	var footer []string
	for _, w := range r.warnings {
		footer = append(footer, ui.Warn(w.Message))
	}
	footer = append(footer, notes...)
	if len(next) > 0 {
		footer = append(footer, ui.Dim("Next:"))
		for _, step := range next {
			footer = append(footer, "  "+ui.Code(step))
		}
	}
	_, err := fmt.Fprintln(ui.Writer(cmd.OutOrStdout()), "\n"+ui.Card(title, rows, footer))
	return err
}

// fillResult adds the TLS state, restart flag and next steps to the result.
func (r *initRun) fillResult(res *initResult) {
	if r.tlsState != nil {
		res.TLS = r.tlsState.State
		res.TLSNotAfter = r.tlsState.NotAfter
	}
	res.RestartPerformed = r.dbRestart
	res.Next = []string{"eru db create <name>", "eru guide agents"}
	if !r.cfg.BackupConfigured() {
		res.Next = append(res.Next, "eru backup setup")
	}
}

func (r *initRun) tlsSummary() string {
	if r.tlsState == nil {
		return r.cfg.TLS.Mode
	}
	return r.tlsState.State
}

// firstContact pins the host key, pushes the binary and runs the host phase
// over public SSH.
func (r *initRun) firstContact(ctx context.Context, known *transport.KnownHosts, publicSSH string, timeout time.Duration) error {
	cmd := r.cmd
	r.p.phase("init.first-contact", "started", r.boot.User+"@"+publicSSH)
	if len(known.Keys(publicSSH)) == 0 {
		key, err := transport.ProbeHostKey(ctx, transport.Direct{Timeout: timeout}, publicSSH, timeout)
		if err != nil {
			return err
		}
		if err := r.trustHostKey(key); err != nil {
			return err
		}
		known.Pin(publicSSH, key)
		if err := known.Save(); err != nil {
			return err
		}
	}
	auth, closeAuth, err := firstContactAuth(cmd, r.boot)
	if err != nil {
		return err
	}
	defer closeAuth()
	client, err := transport.DialSSH(ctx, transport.Direct{Timeout: timeout}, transport.SSHTarget{User: r.boot.User, Addr: publicSSH},
		transport.SSHOptions{Auth: auth, HostKeys: known, Timeout: timeout})
	if err != nil {
		return err
	}
	defer client.Close()
	n := &remote.Node{SSH: client, Operator: r.sec.OperatorName}

	osInfo, err := n.OS(ctx)
	if err != nil {
		return err
	}
	if !osInfo.Supported() {
		return output.NewError(output.CodePrecondition, "the server runs "+osInfo.Pretty+", which eru does not support", output.ExitPrecondition,
			"Use "+hostinfo.SupportedList+".").WithDetails(map[string]any{"reason": "os_unsupported", "os": osInfo})
	}
	arch, err := n.Arch(ctx)
	if err != nil {
		return err
	}
	r.prog.OS, r.prog.Arch = osInfo.Pretty, arch
	r.prog.PublicIPv6 = n.PublicIPv6(ctx)
	if err := r.mark(phaseFirstHost); err != nil {
		return err
	}
	r.p.phase("init.first-contact", "completed", osInfo.Pretty+", "+arch)
	if err := r.confirmDNSEarly(); err != nil {
		return err
	}

	r.p.phase("init.binary", "started", "install eru "+buildinfo.Version+" on the node")
	got, installed, err := n.Version(ctx)
	if err != nil {
		return err
	}
	if t := r.p.task("init.binary"); t != nil {
		n.OnUpload = func(sent, total int64) {
			t.Progress(float64(sent)/float64(max(total, 1)), fmt.Sprintf("%.1f / %.1f MB", float64(sent)/1e6, float64(total)/1e6))
		}
	}
	if !installed || !remote.SameBuild(got, remote.LocalBuild()) {
		art, err := remote.NodeArtifact(ctx, arch)
		if err != nil {
			return err
		}
		if installed {
			if _, err := n.InstallUpgrade(ctx, art, remote.CallOptions{OnEvent: r.p.event}); err != nil {
				return err
			}
		} else if err := n.InstallBootstrap(ctx, art); err != nil {
			return err
		}
		if err := n.Handshake(ctx, remote.HandshakeOptions{}); err != nil {
			return err
		}
	}
	r.p.phase("init.binary", "completed", "")
	if err := r.mark(phaseBinary); err != nil {
		return err
	}

	res, err := r.bootstrapHost(ctx, n)
	if err != nil {
		return err
	}
	r.prog.PublicIPv6 = res.PublicIPv6
	hostKeys, err := transport.ParseHostKeys(res.SSHHostKeys)
	if err != nil {
		return err
	}
	for _, pinned := range known.Keys(publicSSH) {
		if !transport.ContainsKey(hostKeys, pinned) {
			return output.NewError(output.CodeHostKeyMismatch, "the node reported host keys that do not include the key pinned on first contact",
				output.ExitTransport, "Do not continue until a human checks the server.").
				WithDetails(map[string]any{"pinned": ssh.FingerprintSHA256(pinned)})
		}
	}
	known.Pin(publicSSH, hostKeys...)
	known.Pin(net.JoinHostPort(r.master().PublicAddress, "22"), hostKeys...)
	known.Pin(net.JoinHostPort(r.master().WGAddress, "22"), hostKeys...)
	if err := known.Save(); err != nil {
		return err
	}
	return r.mark(phaseHost)
}

// trustHostKey is TOFU on first contact (§7.3): a matching
// --host-key-fingerprint, --accept-new-host-key, or a confirmation in a
// terminal.
func (r *initRun) trustHostKey(key ssh.PublicKey) error {
	fp := ssh.FingerprintSHA256(key)
	want := strings.TrimSpace(r.boot.HostKeyFingerprint)
	switch {
	case want != "":
		if want != fp && "SHA256:"+want != fp {
			return output.NewError(output.CodeHostKeyMismatch, "the server's host key "+fp+" does not match --host-key-fingerprint", output.ExitTransport,
				"Check the fingerprint on the provider console with ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub.").
				WithDetails(map[string]any{"got": fp, "want": want})
		}
		return nil
	case flagTrue(r.cmd, "accept-new-host-key"):
		r.warnings = append(r.warnings, output.Warning{Code: "host_key_accepted_unverified", Message: "trusted host key " + fp + " without checking it"})
		return nil
	case !MustNotPrompt(r.cmd):
		ok, err := PromptYes(r.cmd, "The server's SSH host key is "+key.Type()+" "+fp+". Trust it?")
		if err != nil {
			return err
		}
		if !ok {
			return output.NewError(output.CodeHostKeyMismatch, "the server's host key was not trusted", output.ExitTransport,
				"Check it on the provider console, then pass --host-key-fingerprint "+fp+".").WithDetails(map[string]any{"got": fp})
		}
		return nil
	default:
		return output.NewError(output.CodeInputRequired, "first contact needs the server's host key to be confirmed", output.ExitInput,
			"Ask a human to check "+fp+", then pass --host-key-fingerprint "+fp+" (or --accept-new-host-key).").
			WithDetails(map[string]any{"flag": "--host-key-fingerprint", "fingerprint": fp})
	}
}

// firstContactAuth picks the first-contact credential (§7.3): the
// --password-stdin password, --identity, then ssh-agent keys and the default
// ~/.ssh keys. The agent and default keys go into one publickey method,
// because x/crypto/ssh tries only the first method of each kind. An encrypted
// default key that the agent doesn't hold is unlocked with a passphrase prompt
// in a terminal, and named in the error otherwise.
func firstContactAuth(cmd *cobra.Command, boot contexts.Bootstrap) ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	if flagTrue(cmd, "password-stdin") {
		pw, err := readFirstLine(cmd.InOrStdin())
		if err != nil {
			return nil, noop, err
		}
		secrets.Add(pw)
		return transport.PasswordAuth(pw), noop, nil
	}
	if boot.Identity != "" {
		path := expandHome(boot.Identity)
		ask := func() ([]byte, error) {
			if MustNotPrompt(cmd) {
				return nil, errors.New("the identity is encrypted; load it into ssh-agent")
			}
			pw, err := readSecretTTY(cmd, "Passphrase for "+path)
			return []byte(pw), err
		}
		auth, err := transport.KeyFileAuth(path, ask)
		if err != nil {
			return nil, noop, output.WrapError(output.CodeUsage, err, output.ExitUsage, "Pass a readable private key with --identity.")
		}
		return []ssh.AuthMethod{auth}, noop, nil
	}
	var signers []ssh.Signer
	closeAgent := noop
	agentSigners, closer, ok := transport.AgentSigners()
	if ok {
		signers = append(signers, agentSigners...)
		closeAgent = closer
	}
	var skipped []string
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			path := filepath.Join(home, ".ssh", name)
			signer, encrypted, err := transport.KeyFileSigner(path, nil)
			switch {
			case err == nil:
				signers = append(signers, signer)
			case encrypted && transport.AgentHolds(agentSigners, path+".pub"):
				// The agent already offers this key.
			case encrypted && !MustNotPrompt(cmd):
				signer, _, err = transport.KeyFileSigner(path, func() ([]byte, error) {
					pw, err := readSecretTTY(cmd, "Passphrase for "+path+" (empty to skip)")
					if err == nil && pw == "" {
						err = errors.New("skipped")
					}
					return []byte(pw), err
				})
				if err != nil {
					skipped = append(skipped, path)
					continue
				}
				signers = append(signers, signer)
			case encrypted:
				skipped = append(skipped, path)
			}
		}
	}
	if len(signers) == 0 {
		hint := "Pass --identity <key>, load a key into ssh-agent, or pipe the password with --password-stdin."
		details := map[string]any{"flag": "--identity"}
		if len(skipped) > 0 {
			hint = "These keys have a passphrase and were skipped: " + strings.Join(skipped, ", ") + ". Load one with ssh-add, or pass it with --identity in a terminal."
			details["skipped_keys"] = skipped
		}
		return nil, closeAgent, output.NewError(output.CodeInputRequired, "first contact needs an SSH credential", output.ExitInput, hint).
			WithDetails(details)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, closeAgent, nil
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// attemptReporter shows "attempt n" and the time left on a running phase.
func (r *initRun) attemptReporter(phase, what string) func(int, time.Time) {
	return func(attempt int, deadline time.Time) {
		if t := r.p.task(phase); t != nil {
			t.Deadline(deadline)
			t.Detail(fmt.Sprintf("%s, attempt %d", what, attempt))
		}
	}
}

// lockdown runs the commit-confirm switchover (§8.3): arm and apply on the
// node, then prove a new SSH connection over WireGuard works and confirm. A
// failure waits for the node's revert and reports what it did.
func (r *initRun) lockdown(ctx context.Context, wgNode *remote.Node, tun *transport.Tunnel, target transport.SSHTarget, opts transport.SSHOptions, publicSSH string) (string, error) {
	r.p.phase("init.lockdown", "started", "close public SSH; the node reverts unless confirmed within 2 minutes")
	reply, err := wgNode.Call(ctx, "ssh.lockdown", map[string]any{"timeout_s": int(lockdownTimeout.Seconds())}, remote.CallOptions{OnEvent: r.p.event})
	if err != nil {
		return "", err
	}
	armedAt := time.Now()
	var res node.LockdownResult
	if err := reply.Decode(&res); err != nil {
		return "", err
	}
	if res.AlreadyLocked {
		r.p.phase("init.lockdown", "completed", "already locked down")
		return time.Now().UTC().Format(time.RFC3339), nil
	}
	confirmClient, err := dialRetryReport(ctx, tun, target, opts, confirmWithin, r.attemptReporter("init.lockdown", "new SSH connection over WireGuard"))
	if err == nil {
		defer confirmClient.Close()
		confirmNode := &remote.Node{SSH: confirmClient, Operator: r.sec.OperatorName}
		var reply remote.Reply
		reply, err = confirmNode.Call(ctx, "ssh.confirm", nil, remote.CallOptions{OnEvent: r.p.event})
		if err == nil {
			var confirmed node.ConfirmResult
			if err := reply.Decode(&confirmed); err != nil {
				return "", err
			}
			r.p.phase("init.lockdown", "completed", "public SSH is closed; SSH answers on WireGuard")
			return confirmed.LockedDownAt, nil
		}
	}
	return "", r.lockdownFailed(ctx, armedAt, publicSSH, opts, err)
}

func (r *initRun) lockdownFailed(ctx context.Context, armedAt time.Time, publicSSH string, opts transport.SSHOptions, cause error) error {
	r.p.phase("init.lockdown", "failed", "waiting for the node to revert")
	wait := time.Until(armedAt.Add(lockdownTimeout + revertGrace))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
	}
	details := map[string]any{"cause": secrets.Redact(cause.Error())}
	client, err := transport.DialSSH(ctx, transport.Direct{Timeout: opts.Timeout}, transport.SSHTarget{User: r.cfg.Server.AdminUser, Addr: publicSSH}, opts)
	if err != nil {
		details["public_ssh"] = secrets.Redact(err.Error())
		return output.NewError(output.CodeLockdownFailed,
			"SSH over WireGuard did not work, and the node is not reachable over public SSH either", output.ExitLockdown,
			"Use the provider console: run eru node ssh-revert as root, check ufw and wg0, then eru server init --resume.").WithDetails(details)
	}
	defer client.Close()
	n := &remote.Node{SSH: client, Operator: r.sec.OperatorName}
	if reply, err := n.Call(ctx, "ssh.status", nil, remote.CallOptions{}); err == nil {
		var status node.SSHStatus
		if reply.Decode(&status) == nil {
			details["ssh_status"] = status
		}
	} else {
		details["ssh_status_error"] = secrets.Redact(err.Error())
	}
	return output.NewError(output.CodeLockdownFailed,
		"SSH over WireGuard did not work, so the node reverted to public SSH", output.ExitLockdown,
		"Check that UDP "+strconv.Itoa(r.cfg.Transport.ListenPort)+" reaches the server (provider firewall, local network), then run eru server init --resume.").
		WithDetails(details)
}

// document builds the secrets document from the pending state (§10.1).
func (r *initRun) document() (secretsdoc.Document, error) {
	n := r.master()
	if r.prog.WGPublicKey == "" || len(r.prog.SSHHostKeys) == 0 {
		return secretsdoc.Document{}, errors.New("the node identity is missing from the pending init; re-run with --resume")
	}
	return secretsdoc.Document{
		Schema:     secretsdoc.Schema,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		EruVersion: buildinfo.Version,
		Server:     secretsdoc.Server{Name: r.name, Domain: r.cfg.Domain, TLSMode: r.cfg.TLS.Mode},
		Nodes: []secretsdoc.Node{{
			Name:          n.Name,
			Role:          n.Role,
			Hostname:      r.cfg.Hostname(n.Name),
			PublicAddress: n.PublicAddress,
			ListenPort:    r.cfg.Transport.ListenPort,
			WGAddress:     n.WGAddress,
			WGPublicKey:   r.prog.WGPublicKey,
			SSHHostKeys:   r.prog.SSHHostKeys,
		}},
		Operator: secretsdoc.Operator{
			Name:          r.sec.OperatorName,
			WGAddress:     config.DefaultFirstOperatorWG,
			WGPrivateKey:  r.sec.WGPrivateKey,
			SSHUser:       r.cfg.Server.AdminUser,
			SSHPrivateKey: r.sec.SSHPrivateKey,
			SudoPassword:  r.sec.SudoPassword,
		},
		Database:   r.sec.Database,
		Backup:     r.sec.Backup,
		BreakGlass: secretsdoc.BreakGlass(r.name, r.cfg.Server.AdminUser, n.PublicAddress),
	}, nil
}

// writeDocument shows the secrets handoff (§10.1) and gates on "stored"
// (§8.2 step 12). Nothing is written unless --secrets-out names a file for the
// transfer bundle. A resumed init that has not been confirmed shows a new
// handoff with a new passphrase.
func (r *initRun) writeDocument() (secretsdoc.Document, initResult, error) {
	cmd := r.cmd
	n := r.master()
	res := initResult{Server: r.name, Node: n.Name, PublicAddress: n.PublicAddress, WGAddress: n.WGAddress, AdminUser: r.cfg.Server.AdminUser}
	doc, err := r.document()
	if err != nil {
		return doc, res, err
	}
	out := stringFlag(cmd, "secrets-out")
	if out == "" {
		out = r.prog.SecretsOut
	}
	if out != "" {
		if abs, err := filepath.Abs(out); err == nil {
			out = abs
		}
		if _, err := os.Stat(out); err == nil && r.prog.SecretsOut != out {
			return doc, res, output.NewError(output.CodePrecondition, out+" already exists", output.ExitPrecondition,
				"Move it away, or pass --secrets-out with another path, then re-run with --resume.").WithDetails(map[string]any{"reason": "secrets_out_exists", "path": out})
		}
	}
	r.p.phase("init.secrets-document", "started", out)
	h, err := newHandoff(doc, out)
	if err != nil {
		return doc, res, err
	}
	r.prog.SecretsOut = h.path
	if err := r.mark(phaseDocument); err != nil {
		return doc, res, err
	}
	res.SecretsDocument = h.path
	r.p.phase("init.secrets-document", "completed", h.path)
	if MustNotPrompt(cmd) {
		// Explicitly requested with --reveal-passphrase: it travels in the envelope.
		res.Passphrase = h.passphrase
		return doc, res, nil
	}
	// On the server itself the record belongs on the operator's machine, so
	// the save action is not offered.
	if err := h.show(cmd, flagTrue(cmd, "confirm-secrets-stored"), !r.local); err != nil {
		return doc, res, err
	}
	return doc, res, nil
}

func runInitLocal(cmd *cobra.Command) error {
	if handled, err := localenv.Escalate(cmd, "initialise this server"); handled || err != nil {
		return err
	}
	osInfo, err := hostinfo.ReadOS()
	if err != nil || !osInfo.Supported() {
		return output.NewError(output.CodePrecondition, "this server runs "+osInfo.Pretty+", which eru does not support", output.ExitPrecondition,
			"Use "+hostinfo.SupportedList+".").WithDetails(map[string]any{"reason": "os_unsupported", "os": osInfo})
	}
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil && resolved != node.BinaryPath {
			return output.NewError(output.CodePrecondition, "server init --local must run as "+node.BinaryPath, output.ExitPrecondition,
				"Install eru with the install script as root (curl -fsSL <base>/install.sh | sudo bash), then run "+node.BinaryPath+" server init --local.").
				WithDetails(map[string]any{"reason": "binary_not_installed", "running": resolved})
		}
	}
	if _, err := os.Stat(config.NodePath); err == nil && !flagTrue(cmd, "resume") {
		return output.NewError(output.CodePrecondition, "this server is already an eru node ("+config.NodePath+" exists)", output.ExitPrecondition,
			"Operate it from your machine with eru login, or continue an unfinished init with --resume.").
			WithDetails(map[string]any{"reason": "already_a_node"})
	}
	r, err := prepareInit(cmd, true)
	if err != nil {
		return err
	}
	if dryRun(cmd) {
		return r.dryRunResult()
	}
	ctx := commandContext(cmd)
	if err := r.savePending(); err != nil {
		return err
	}
	if err := r.generateSecrets(); err != nil {
		return err
	}
	if err := r.ensureDatabaseSecrets(); err != nil {
		return err
	}
	if err := r.ensureBackupKit(commandContext(cmd)); err != nil {
		return err
	}
	caller := remote.Local{Operator: firstOperatorName}
	if !r.prog.Done(phaseHost) {
		r.prog.PublicIPv6 = hostinfo.PublicIPv6()
		if err := r.confirmDNSEarly(); err != nil {
			return err
		}
		hostRes, err := r.bootstrapHost(ctx, caller)
		if err != nil {
			return err
		}
		r.prog.PublicIPv6 = hostRes.PublicIPv6
		if err := r.mark(phaseHost); err != nil {
			return err
		}
	}
	// --local cannot prove the tunnel from here, so public SSH stays open and
	// the first operator command over WireGuard confirms (§8.5).
	deadline := ""
	if !r.prog.Done(phaseLockdown) {
		r.p.phase("init.lockdown", "started", "arm the lockdown; public SSH stays open until an operator confirms over WireGuard")
		if _, err := caller.Call(ctx, "ssh.lockdown", map[string]any{
			"timeout_s":   int(localLockdownTimeout.Seconds()),
			"keep_public": true,
		}, remote.CallOptions{OnEvent: r.p.event}); err != nil {
			return err
		}
		deadline = time.Now().Add(localLockdownTimeout).UTC().Format(time.RFC3339)
		r.p.phase("init.lockdown", "completed", "pending until "+deadline)
		if err := r.mark(phaseLockdown); err != nil {
			return err
		}
	}
	// DNS (acme mode only).
	if r.cfg.TLS.Mode == "acme" && !r.prog.Done(phaseDNS) {
		if err := r.dnsStep(ctx); err != nil {
			return err
		}
		if err := r.mark(phaseDNS); err != nil {
			return err
		}
	}
	// Database phase: steps 7-13 on this server.
	if !r.prog.Done(phaseDatabase) {
		r.p.phase("init.database", "started", "run database steps 7-13 on this server")
		res, err := r.callDatabasePhase(ctx, caller)
		if err != nil {
			return err
		}
		r.tlsState, r.dbRestart = res.TLS, res.RestartPerformed
		r.p.phase("init.database", "completed", "")
		if err := r.mark(phaseDatabase); err != nil {
			return err
		}
	}
	if !r.prog.Done(phaseTLS) {
		if err := r.waitForTLS(ctx, caller); err != nil {
			return err
		}
		if err := r.mark(phaseTLS); err != nil {
			return err
		}
	}
	if err := r.configureInitBackups(ctx, caller); err != nil {
		return err
	}
	_, res, err := r.writeDocument()
	if err != nil {
		return err
	}
	if _, err := caller.Call(ctx, "cluster.confirm-secrets", nil, remote.CallOptions{OnEvent: r.p.event}); err != nil {
		return err
	}
	if err := contexts.RemovePending(r.name); err != nil {
		return err
	}
	res.LockdownPending = true
	res.LockdownDeadline = deadline
	res.LoginCommand = "eru login"
	r.fillResult(&res)
	message := strings.Join([]string{
		fmt.Sprintf("Server %s is bootstrapped on this machine (%s, WireGuard %s).", r.name, res.PublicAddress, res.WGAddress),
		"TLS: " + r.tlsSummary(),
		"No application databases yet: create one with eru db create <name>, or point your agent to the CLI (eru guide agents).",
		"On your machine run eru login and paste the transfer bundle from the handoff.",
		"Then run any remote command, such as eru plan, within 30 minutes. That confirms SSH over WireGuard and closes public SSH.",
		"If nobody confirms in time, the SSH change reverts and the first remote command later locks down instead.",
	}, "\n")
	return r.finish(res, message, append([]string{res.LoginCommand}, res.Next...), fmt.Sprintf("Server %s is bootstrapped on this machine", r.name), []string{
		"On your machine run eru login and paste the transfer bundle,",
		"then any remote command within 30 minutes. That closes public SSH; otherwise the SSH change reverts.",
	})
}
