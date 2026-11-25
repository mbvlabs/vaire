package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/transport"
)

const setupTTL = 30 * time.Minute

func accessInvalid(message string) error {
	return output.NewError(output.CodeValidation, message, output.ExitPrecondition, "")
}

func accessNode(cfg config.Cluster) (config.NodeSpec, error) {
	if len(cfg.Nodes) != 1 || cfg.Nodes[0].Role != config.RoleMaster {
		return config.NodeSpec{}, accessInvalid("managed access requires a single-node workspace")
	}

	return cfg.Nodes[0], nil
}

func accessOperator(r config.AccessRegistry, name string) int {
	return slices.IndexFunc(r.Operators, func(op config.AccessOperator) bool { return op.Name == name })
}

func accessDevice(r config.AccessRegistry, operatorID, label string) int {
	for i, d := range slices.Backward(r.Devices) {
		if d.OperatorID == operatorID && d.Label == label {
			return i
		}
	}

	return -1
}

func deviceReply(d config.AccessDevice, user string) rpc.Device {
	row := rpc.Device{ID: d.ID, User: user, Label: d.Label, Status: d.State, Address: d.Address}
	if d.State == config.DevicePending && !d.ExpiresAt.IsZero() {
		row.ExpiresAt = d.ExpiresAt.UTC().Format(time.RFC3339)
		if !time.Now().Before(d.ExpiresAt) {
			row.Status = "expired"
		}
	}

	return row
}

func accessAddresses(cfg config.Cluster, r config.AccessRegistry, legacy config.Operators) (string, string, error) {
	subnet, reserved, err := reservedAccessAddresses(cfg, r, legacy)
	if err != nil {
		return "", "", err
	}

	permanent, err := config.NextAccessAddress(subnet, reserved)
	if err != nil {
		return "", "", err
	}

	temporary, err := config.NextAccessAddress(subnet, append(reserved, permanent))
	if err != nil {
		return "", "", err
	}

	return permanent.String(), temporary.String(), nil
}

func reservedAccessAddresses(cfg config.Cluster, r config.AccessRegistry, legacy config.Operators) (netip.Prefix, []netip.Addr, error) {
	subnet, err := netip.ParsePrefix(cfg.Transport.Subnet)
	if err != nil {
		return netip.Prefix{}, nil, err
	}

	reserved := make([]netip.Addr, 0, len(cfg.Nodes)+len(legacy.Operators)+2*len(r.Devices))
	for _, spec := range cfg.Nodes {
		addr, err := netip.ParseAddr(spec.WGAddress)
		if err != nil {
			return netip.Prefix{}, nil, err
		}

		reserved = append(reserved, addr)
	}

	for _, op := range legacy.Operators {
		addr, err := netip.ParseAddr(op.WGAddress)
		if err != nil {
			return netip.Prefix{}, nil, err
		}

		reserved = append(reserved, addr)
	}

	for _, d := range r.Devices {
		reserved = append(reserved, netip.MustParseAddr(d.Address))
		if d.TemporaryAddress != "" {
			reserved = append(reserved, netip.MustParseAddr(d.TemporaryAddress))
		}
	}

	return subnet, reserved, nil
}

func handleDeviceAdd(ctx context.Context, env *Env) (Result, error) {
	if err := authorizeUserManagement(env); err != nil {
		return Result{}, err
	}

	var p rpc.DeviceAddParams
	if json.Unmarshal(env.Req.Params, &p) != nil || env.Req.DryRun {
		return Result{}, accessInvalid("device issuance requires a user and device label")
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	user := accessOperator(r, p.User)
	if user < 0 || r.Operators[user].Revoked {
		return Result{}, accessInvalid("active workspace user not found")
	}

	if err := prepareDeviceInvite(&r, r.Operators[user].ID, p.Label, p.Fresh); err != nil {
		return Result{}, err
	}

	n, err := accessNode(env.Cluster)
	if err != nil {
		return Result{}, err
	}

	d, setup, err := issueDeviceSetup(env.Cluster, n, r, r.Operators[user], p.Label)
	if err != nil {
		return Result{}, err
	}

	if err := r.AddDevice(d); err != nil {
		return Result{}, accessInvalid(err.Error())
	}

	code, err := setup.Encode()
	if err != nil {
		return Result{}, err
	}

	if err := prepareTemporaryAccess(ctx, n.WGAddress); err != nil {
		return Result{}, err
	}

	if err := saveAccessRegistry(r); err != nil {
		return Result{}, err
	}

	if err := addTemporaryAccessPeer(ctx, n.WGAddress, d.TemporaryAddress, d.TemporaryPublicKey); err != nil {
		r.Devices[len(r.Devices)-1].State = config.DeviceRevoked
		r.Devices[len(r.Devices)-1].Cleanup = true

		return Result{}, errors.Join(err, saveAccessRegistry(r))
	}

	row := deviceReply(d, p.User)
	row.SetupCode = code

	return Result{Data: row}, nil
}

// Supersede only an unused enrollment and only on explicit request.
// The old digest and peer are retained
// for rejection and timer cleanup, respectively. The caller saves this state
// together with the replacement under the exclusive device.add lock.
func prepareDeviceInvite(r *config.AccessRegistry, operatorID, label string, fresh bool) error {
	index := accessDevice(*r, operatorID, label)
	if index < 0 {
		if fresh {
			return accessInvalid("no existing setup code; use device add without --fresh")
		}

		return nil
	}

	d := &r.Devices[index]

	unused := !d.Recovery && d.SSHPublicKey == "" && d.WGPublicKey == "" && d.UnixUID == "" &&
		(d.State == config.DeviceRevoked || d.State == config.DevicePending && d.TemporaryAddress != "")
	if !unused {
		if !fresh && d.State == config.DeviceRevoked {
			return nil
		}

		return accessInvalid("this device label is already registered; --fresh cannot replace an enrolled, in-progress or recovery device")
	}

	if !fresh {
		return accessInvalid("this device label already has a setup code; use device add with --fresh")
	}

	if d.State == config.DevicePending {
		d.State, d.Cleanup = config.DeviceRevoked, true
	}

	return nil
}

func issueDeviceSetup(cfg config.Cluster, n config.NodeSpec, r config.AccessRegistry, user config.AccessOperator, label string) (config.AccessDevice, rpc.AccessSetup, error) {
	legacy, err := config.LoadOperators(config.OperatorsPath)
	if err != nil {
		return config.AccessDevice{}, rpc.AccessSetup{}, err
	}

	address, temporary, err := accessAddresses(cfg, r, legacy)
	if err != nil {
		return config.AccessDevice{}, rpc.AccessSetup{}, err
	}

	id, err := newAccessID("dev-")
	if err != nil {
		return config.AccessDevice{}, rpc.AccessSetup{}, err
	}

	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return config.AccessDevice{}, rpc.AccessSetup{}, err
	}

	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	digest := sha256.Sum256([]byte(token))

	tempPrivate, tempPublic, err := transport.GenerateWGKey()
	if err != nil {
		return config.AccessDevice{}, rpc.AccessSetup{}, err
	}

	d := config.AccessDevice{
		ID: id, OperatorID: user.ID, Label: label, State: config.DevicePending,
		Address: address, TemporaryAddress: temporary, TemporaryPublicKey: tempPublic,
		UnixLogin: "vdev-" + strings.TrimPrefix(id, "dev-"), RequestDigest: hex.EncodeToString(digest[:]),
		ExpiresAt: time.Now().UTC().Add(setupTTL).Truncate(time.Second),
	}

	setup, err := permanentSetup(cfg, n, user.Name, d)
	if err != nil {
		return config.AccessDevice{}, rpc.AccessSetup{}, err
	}

	setup.Token, setup.TemporaryAddress, setup.TemporaryPrivateKey, setup.ExpiresAt = token, temporary, tempPrivate, d.ExpiresAt
	setup.Role = user.Role

	return d, setup, nil
}

func permanentSetup(cfg config.Cluster, n config.NodeSpec, name string, d config.AccessDevice) (rpc.AccessSetup, error) {
	node, err := config.LoadNode(config.NodePath)
	if err != nil {
		return rpc.AccessSetup{}, err
	}

	nodePub, err := steps.WGPublicKey()
	if err != nil {
		return rpc.AccessSetup{}, err
	}

	rawHostKey, err := os.ReadFile("/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return rpc.AccessSetup{}, err
	}

	hostParts := strings.Fields(string(rawHostKey))
	if len(hostParts) < 2 {
		return rpc.AccessSetup{}, accessInvalid("node SSH host key is missing")
	}

	hostKey := strings.Join(hostParts[:2], " ")
	if err := config.ValidateBareSSHEd25519PublicKey(hostKey); err != nil {
		return rpc.AccessSetup{}, err
	}

	return rpc.AccessSetup{
		Version: 1, ID: d.ID, Server: node.Server, Node: node.NodeName, Domain: cfg.Domain, TLSMode: cfg.TLS.Mode,
		User: name, Label: d.Label, UnixLogin: d.UnixLogin, Address: d.Address, NodeAddress: n.WGAddress,
		NodePublicAddress: n.PublicAddress, NodeWGPublicKey: nodePub, HostPublicKey: hostKey,
		WGPort: cfg.Transport.ListenPort,
	}, nil
}

func handleDeviceList(_ context.Context, env *Env) (Result, error) {
	if err := authorizeUserManagement(env); err != nil {
		return Result{}, err
	}

	var p rpc.DeviceListParams
	if len(env.Req.Params) > 0 && json.Unmarshal(env.Req.Params, &p) != nil {
		return Result{}, accessInvalid("invalid device list filter")
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	operators := make(map[string]string, len(r.Operators))
	for _, op := range r.Operators {
		operators[op.ID] = op.Name
	}

	rows := make([]rpc.Device, 0, len(r.Devices))
	for _, d := range r.Devices {
		name := operators[d.OperatorID]
		if p.User == "" || p.User == name {
			rows = append(rows, deviceReply(d, name))
		}
	}

	return Result{Data: rpc.DevicesReply{Devices: rows}}, nil
}

func handleDeviceRevoke(ctx context.Context, env *Env) (Result, error) {
	if err := authorizeUserManagement(env); err != nil {
		return Result{}, err
	}

	var p rpc.DeviceRevokeParams
	if json.Unmarshal(env.Req.Params, &p) != nil {
		return Result{}, accessInvalid("invalid device revocation request")
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	user := accessOperator(r, p.User)
	if user < 0 {
		return Result{}, accessInvalid("workspace user not found")
	}

	index := accessDevice(r, r.Operators[user].ID, p.Label)
	if index < 0 {
		return Result{}, accessInvalid("device not found")
	}

	d := &r.Devices[index]
	if p.ID == "" {
		return Result{Data: deviceReply(*d, p.User)}, nil
	}

	if p.ID != d.ID || env.Req.Confirm != d.ID || env.Req.DryRun {
		return Result{}, accessInvalid("device revocation must be confirmed by ID")
	}

	if env.DeviceIdentity != nil && env.DeviceIdentity.DeviceID == d.ID {
		return Result{}, accessInvalid("revoke this owner device from a different owner login")
	}

	if d.State != config.DeviceRevoked {
		d.State, d.Cleanup = config.DeviceRevoked, true

		if err := saveAccessRegistry(r); err != nil {
			return Result{}, err
		}
	}

	if err := reconcileAccessDevice(ctx, env.Cluster, r, d); err != nil {
		return Result{}, err
	}

	return Result{Data: deviceReply(*d, p.User)}, nil
}

func handleUserRevoke(ctx context.Context, env *Env) (Result, error) {
	if err := authorizeUserManagement(env); err != nil {
		return Result{}, err
	}

	var p rpc.UserRevokeParams
	if json.Unmarshal(env.Req.Params, &p) != nil || p.User == "" || env.Req.Confirm != p.User || env.Req.DryRun {
		return Result{}, accessInvalid("user revocation must be confirmed by name")
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	user := accessOperator(r, p.User)
	if user < 0 {
		return Result{}, accessInvalid("workspace user not found")
	}

	op := &r.Operators[user]
	if env.DeviceIdentity != nil && env.DeviceIdentity.OperatorID == op.ID {
		return Result{}, accessInvalid("revoke this owner from a different owner login")
	}

	op.Revoked = true

	for i := range r.Devices {
		d := &r.Devices[i]
		if d.OperatorID == op.ID && d.State != config.DeviceRevoked {
			d.State, d.Cleanup = config.DeviceRevoked, true
		}
	}

	if err := saveAccessRegistry(r); err != nil {
		return Result{}, err
	}

	var cleanupErr error

	for i := range r.Devices {
		d := &r.Devices[i]
		if d.OperatorID == op.ID {
			cleanupErr = errors.Join(cleanupErr, reconcileAccessDevice(ctx, env.Cluster, r, d))
		}
	}

	return Result{Data: accessUser(*op)}, cleanupErr
}

// AccessRedeem is invoked only by the socket-activated connection handler.
// The peer address comes from systemd, not from the contents of the request.
func AccessRedeem(ctx context.Context, source string, in io.Reader) error {
	if os.Geteuid() != 0 || os.Getenv("SUDO_UID") != "" {
		return deviceAccessDenied()
	}

	var p rpc.AccessRedeemParams

	dec := json.NewDecoder(io.LimitReader(in, 4096))
	dec.DisallowUnknownFields()

	if dec.Decode(&p) != nil || config.ValidateBareSSHEd25519PublicKey(p.SSHPublicKey) != nil ||
		!config.ValidAccessWGKey(p.WGPublicKey) || len(p.Token) != 43 {
		return deviceAccessDenied()
	}

	release, err := engine.Acquire(ctx, engine.LockExclusive, "access.redeem", "", 10*time.Second)
	if err != nil {
		return err
	}
	defer release()

	cfg, err := loadGuardCluster()
	if err != nil {
		return err
	}

	r, err := userRegistry()
	if err != nil {
		return err
	}

	return redeemAccess(ctx, cfg, r, source, p)
}

func redeemAccess(ctx context.Context, cfg config.Cluster, r config.AccessRegistry, source string, p rpc.AccessRedeemParams) error {
	index := slices.IndexFunc(r.Devices, func(d config.AccessDevice) bool { return d.ID == p.ID })
	if index < 0 {
		return deviceAccessDenied()
	}

	d := &r.Devices[index]
	if err := validateAccessRedemption(*d, source, p, time.Now()); err != nil {
		return err
	}

	legacy, err := config.LoadOperators(config.OperatorsPath)
	if err != nil {
		return err
	}

	if matchesLegacyKey(legacy, p.SSHPublicKey, p.WGPublicKey) {
		return deviceAccessDenied()
	}

	if d.UnixLogin == cfg.Server.AdminUser || !slices.ContainsFunc(r.Operators, func(op config.AccessOperator) bool {
		return op.ID == d.OperatorID && !op.Revoked
	}) {
		return deviceAccessDenied()
	}
	// Persist the winning keys before any OS changes. A second client with
	// different keys cannot resume a partially provisioned activation.
	d.WGPublicKey, d.SSHPublicKey = p.WGPublicKey, p.SSHPublicKey

	if err := saveAccessRegistry(r); err != nil {
		return err
	}

	return provisionAccessDevice(ctx, cfg, r, d)
}

func matchesLegacyKey(legacy config.Operators, sshKey, wgKey string) bool {
	return slices.ContainsFunc(legacy.Operators, func(op config.Operator) bool {
		key := strings.Fields(op.SSHPublicKey)
		return op.WGPublicKey == wgKey || len(key) >= 2 && strings.Join(key[:2], " ") == sshKey
	})
}

func provisionAccessDevice(ctx context.Context, cfg config.Cluster, r config.AccessRegistry, d *config.AccessDevice) error {
	if d.State == config.DeviceActive {
		return reconcileAccessDevice(ctx, cfg, r, d)
	}

	if d.UnixUID == "" {
		uid, err := prepareDeviceSSH(ctx, d.UnixLogin)
		if err != nil {
			return err
		}

		d.UnixUID = uid

		if err := saveAccessRegistry(r); err != nil {
			return err
		}
	}

	if err := installDeviceSSHKey(ctx, d.UnixLogin, d.Address, d.SSHPublicKey); err != nil {
		return err
	}

	d.State, d.Cleanup = config.DeviceActive, true

	if err := saveAccessRegistry(r); err != nil {
		return err
	}

	return reconcileAccessDevice(ctx, cfg, r, d)
}

func validateAccessRedemption(d config.AccessDevice, source string, p rpc.AccessRedeemParams, now time.Time) error {
	digest := sha256.Sum256([]byte(p.Token))
	if d.State == config.DeviceRevoked || (d.State == config.DeviceActive && !d.Cleanup) ||
		d.TemporaryAddress == "" || source != d.TemporaryAddress ||
		(!d.Recovery && !now.Before(d.ExpiresAt)) || subtle.ConstantTimeCompare([]byte(d.RequestDigest), []byte(hex.EncodeToString(digest[:]))) != 1 {
		return deviceAccessDenied()
	}

	if d.WGPublicKey != "" && (d.WGPublicKey != p.WGPublicKey || d.SSHPublicKey != p.SSHPublicKey) {
		return deviceAccessDenied()
	}

	return nil
}

func reconcileAccessDevice(ctx context.Context, cfg config.Cluster, r config.AccessRegistry, d *config.AccessDevice) error {
	if !d.Cleanup {
		return nil
	}

	n, err := accessNode(cfg)
	if err != nil {
		return err
	}

	if d.UnixUID != "" && d.State == config.DeviceRevoked {
		if err := revokeLiveAccess(ctx, cfg, r, n, *d); err != nil {
			return err
		}
	}

	if d.UnixUID != "" && d.State == config.DeviceActive {
		if err := activateDeviceAccess(ctx, cfg, r, *d); err != nil {
			return err
		}
	}

	if d.TemporaryPublicKey != "" {
		if err := removeTemporaryAccessPeer(ctx, n.WGAddress, d.TemporaryAddress, d.TemporaryPublicKey); err != nil {
			return err
		}

		d.TemporaryPublicKey, d.TemporaryAddress, d.ExpiresAt = "", "", time.Time{}
	}

	if d.UnixUID != "" {
		if err := steps.RefreshAccessPoolerHBA(ctx, cfg, runtimeFor(&Env{Cluster: cfg}, false)); err != nil {
			return err
		}
	}

	d.Cleanup = false

	return saveAccessRegistry(r)
}

func activateDeviceAccess(ctx context.Context, cfg config.Cluster, r config.AccessRegistry, d config.AccessDevice) error {
	if slices.ContainsFunc(r.Operators, func(op config.AccessOperator) bool {
		return op.ID == d.OperatorID && !op.Revoked && op.Role == config.AccessRoleOwner
	}) {
		if err := installAdminShell(ctx, d); err != nil {
			return err
		}
	}

	if err := steps.SaveAccessWG(cfg, r); err != nil {
		return err
	}

	_, err := temporaryAccessCommand(ctx, "wg", "set", "wg0", "peer", d.WGPublicKey, "allowed-ips", d.Address+"/32")

	return err
}

// All independent cuts are attempted even if persisting or updating one
// fails. The durable cleanup flag leaves incomplete host work retryable.
func revokeLiveAccess(ctx context.Context, cfg config.Cluster, r config.AccessRegistry, n config.NodeSpec, d config.AccessDevice) error {
	_, wgErr := temporaryAccessCommand(ctx, "wg", "set", "wg0", "peer", d.WGPublicKey, "remove")
	keyErr := removeDeviceSSHKey(ctx, d.UnixLogin)

	var hostErr error

	for _, op := range r.Operators {
		if op.ID != d.OperatorID || op.Role != config.AccessRoleOwner {
			continue
		}

		if config.CheckAdminUser(op.Name) != nil {
			hostErr = deviceAccessDenied()
			break
		}

		var keys string
		if !op.Revoked {
			keys, hostErr = renderAdminShellKeys(r, op.ID)
		}

		if hostErr == nil {
			hostErr = writeDeviceSSHKey(ctx, op.Name, keys)
		}

		break
	}

	sshErr := closeAccessSSH(ctx, n.WGAddress, d.Address)
	bootErr := steps.SaveAccessWG(cfg, r)

	return errors.Join(wgErr, keyErr, hostErr, sshErr, bootErr)
}

// No process is selected by UID: only SSH sockets on the WG address whose
// remote endpoint is this device's immutable /32. Detached jobs are not
// affected; already-admitted operations retain their normal semantics.
func closeAccessSSH(ctx context.Context, nodeAddress, deviceAddress string) error {
	if _, err := temporaryIPv4(nodeAddress); err != nil {
		return err
	}

	if _, err := temporaryIPv4(deviceAddress); err != nil {
		return err
	}

	if nodeAddress == deviceAddress {
		return errors.New("device and node WireGuard addresses match")
	}

	return errors.Join(
		closeAccessSSHSockets(ctx, "-4", nodeAddress+":22", deviceAddress),
		closeAccessSSHSockets(ctx, "-6", "[::ffff:"+nodeAddress+"]:22", "[::ffff:"+deviceAddress+"]"),
	)
}

func closeAccessSSHSockets(ctx context.Context, family, source, destination string) error {
	filter := []string{"-H", family, "-t", "state", "established", "src", source, "dst", destination}
	if _, err := temporaryAccessCommand(ctx, "ss", append([]string{"-K"}, filter...)...); err != nil {
		return err
	}

	out, err := temporaryAccessCommand(ctx, "ss", filter...)
	if err != nil {
		return err
	}

	if strings.TrimSpace(out) != "" {
		return errors.New("revoked device still has established management sessions")
	}

	return nil
}

// ExpireAccess reconciles interrupted access updates and removes expired
// temporary peers, even if no user returns to the workspace.
func ExpireAccess(ctx context.Context) error {
	if os.Geteuid() != 0 || os.Getenv("SUDO_UID") != "" {
		return deviceAccessDenied()
	}

	release, err := engine.Acquire(ctx, engine.LockExclusive, "access.expire", "", 10*time.Second)
	if err != nil {
		return err
	}
	defer release()

	r, err := userRegistry()
	if err != nil || len(r.Devices) == 0 {
		return err
	}

	cfg, err := loadGuardCluster()
	if err != nil {
		return err
	}

	n, err := accessNode(cfg)
	if err != nil {
		return err
	}

	return expireAccessDevices(ctx, cfg, r, n)
}

func expireAccessDevices(ctx context.Context, cfg config.Cluster, r config.AccessRegistry, n config.NodeSpec) error {
	var failures error

	for i := range r.Devices {
		d := &r.Devices[i]
		if d.State == config.DevicePending && d.TemporaryAddress != "" {
			if accessLeaseUsable(*d, time.Now()) {
				failures = errors.Join(failures, addTemporaryAccessPeer(ctx, n.WGAddress, d.TemporaryAddress, d.TemporaryPublicKey))
				continue
			}

			d.State, d.Cleanup = config.DeviceRevoked, true

			if err := saveAccessRegistry(r); err != nil {
				return errors.Join(failures, err)
			}
		}

		failures = errors.Join(failures, reconcileAccessDevice(ctx, cfg, r, d))
	}

	return failures
}

func accessLeaseUsable(d config.AccessDevice, now time.Time) bool {
	return d.Recovery || now.Before(d.ExpiresAt)
}
