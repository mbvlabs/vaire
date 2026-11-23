package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

const setupDeviceLabel = "setup-device"

// handleBootstrapOwner is bound to the bootstrap/recovery authority, never
// an already enrolled device. The first successful request pins the key pair.
func handleBootstrapOwner(ctx context.Context, env *Env) (Result, error) {
	if env.DeviceIdentity != nil {
		return Result{}, deviceAccessDenied()
	}

	if !env.TrustedHostAdmin {
		if err := authorizeUserManagement(env); err != nil {
			return Result{}, err
		}
	}

	var p rpc.AccessBootstrapOwnerParams
	if env.Req.DryRun || json.Unmarshal(env.Req.Params, &p) != nil ||
		config.ValidateBareSSHEd25519PublicKey(p.SSHPublicKey) != nil || !config.ValidAccessWGKey(p.WGPublicKey) {
		return Result{}, accessInvalid("initial owner requires new device public keys")
	}

	n, err := accessNode(env.Cluster)
	if err != nil {
		return Result{}, err
	}

	if err := authorizeBootstrapOperation(env.Cluster.Name(), n.Name, env.Req.OperationID); err != nil {
		return Result{}, err
	}

	legacy, err := config.LoadOperators(config.OperatorsPath)
	if err != nil {
		return Result{}, err
	}

	if matchesLegacyKey(legacy, p.SSHPublicKey, p.WGPublicKey) ||
		!slices.ContainsFunc(legacy.Operators, func(op config.Operator) bool { return op.Name == p.User }) {
		return Result{}, deviceAccessDenied()
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	index, err := bootstrapOwnerDevice(env.Cluster, &r, legacy, p)
	if err != nil {
		return Result{}, err
	}

	d := &r.Devices[index]
	if err := saveAccessRegistry(r); err != nil {
		return Result{}, err
	}

	if err := provisionAccessDevice(ctx, env.Cluster, r, d); err != nil {
		return Result{}, err
	}

	setup, err := permanentSetup(env.Cluster, n, p.User, *d)
	setup.Role = config.AccessRoleOwner

	return Result{Data: setup}, err
}

func authorizeBootstrapOperation(server, node, operation string) error {
	if operation == "" {
		return deviceAccessDenied()
	}

	if err := bootstrapDir(false); err != nil {
		return deviceAccessDenied()
	}

	owner, err := bootstrapReadOwnership()
	if err != nil || owner.Operation != operation || owner.Server != server || owner.Node != node {
		return deviceAccessDenied()
	}

	instance, err := bootstrapInstance()
	if err != nil || owner.Instance != instance {
		return deviceAccessDenied()
	}

	return nil
}

func bootstrapOwnerDevice(cfg config.Cluster, r *config.AccessRegistry, legacy config.Operators, p rpc.AccessBootstrapOwnerParams) (int, error) {
	for _, op := range r.Operators {
		if op.BootstrapOwner {
			return resumeBootstrapOwner(*r, op, p)
		}
	}

	opID, err := newAccessID("op-")
	if err != nil {
		return -1, err
	}

	devID, err := newAccessID("dev-")
	if err != nil {
		return -1, err
	}

	subnet, reserved, err := reservedAccessAddresses(cfg, *r, legacy)
	if err != nil {
		return -1, err
	}

	address, err := config.NextAccessAddress(subnet, reserved)
	if err != nil {
		return -1, err
	}

	if err := r.AddOperator(config.AccessOperator{ID: opID, Name: p.User, Role: config.AccessRoleOwner, BootstrapOwner: true}); err != nil {
		return -1, accessInvalid(err.Error())
	}

	digest := sha256.Sum256([]byte("bootstrap-owner\x00" + p.SSHPublicKey + "\x00" + p.WGPublicKey))
	d := config.AccessDevice{
		ID: devID, OperatorID: opID, Label: setupDeviceLabel, State: config.DevicePending,
		SSHPublicKey: p.SSHPublicKey, WGPublicKey: p.WGPublicKey, Address: address.String(),
		UnixLogin: "vdev-" + strings.TrimPrefix(devID, "dev-"), RequestDigest: hex.EncodeToString(digest[:]),
	}

	if d.UnixLogin == cfg.Server.AdminUser {
		return -1, deviceAccessDenied()
	}

	if err := r.AddDevice(d); err != nil {
		return -1, accessInvalid(err.Error())
	}

	return len(r.Devices) - 1, nil
}

func resumeBootstrapOwner(r config.AccessRegistry, op config.AccessOperator, p rpc.AccessBootstrapOwnerParams) (int, error) {
	if op.Revoked || op.Name != p.User {
		return -1, deviceAccessDenied()
	}

	for i, d := range r.Devices {
		if d.OperatorID == op.ID && d.Label == setupDeviceLabel && d.TemporaryAddress == "" &&
			d.State != config.DeviceRevoked && d.SSHPublicKey == p.SSHPublicKey && d.WGPublicKey == p.WGPublicKey {
			return i, nil
		}
	}

	return -1, deviceAccessDenied()
}
