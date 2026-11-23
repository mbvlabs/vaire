package node

import (
	"context"
	"slices"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/steps"
)

// The verified setup device retires its operation's temporary transport.
// It remains connected throughout the cut, including on a retry.
func handleCompleteSetupAccess(ctx context.Context, env *Env) (Result, error) {
	if env.Req.DryRun || env.DeviceIdentity == nil {
		return Result{}, deviceAccessDenied()
	}

	if err := authorizeUserManagement(env); err != nil {
		return Result{}, err
	}

	n, err := accessNode(env.Cluster)
	if err != nil {
		return Result{}, err
	}

	if err := authorizeBootstrapOperation(env.Cluster.Name(), n.Name, env.Req.OperationID); err != nil {
		return Result{}, err
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	id := env.DeviceIdentity

	index := accessOperator(r, id.Operator)
	if id.Device != setupDeviceLabel || index < 0 || r.Operators[index].ID != id.OperatorID || !r.Operators[index].BootstrapOwner {
		return Result{}, deviceAccessDenied()
	}

	if !slices.ContainsFunc(r.Devices, func(d config.AccessDevice) bool {
		return d.Recovery && d.OperatorID == id.OperatorID && d.State != config.DeviceRevoked
	}) {
		return Result{}, accessInvalid("store the owner recovery kit before retiring setup access")
	}

	if err := retireBootstrapOperator(ctx, env, n, id.Operator); err != nil {
		return Result{}, err
	}

	if err := steps.SaveAccessWG(env.Cluster, r); err != nil {
		return Result{}, err
	}

	return Result{}, steps.RefreshAccessPoolerHBA(ctx, env.Cluster, runtimeFor(env, false))
}

func retireBootstrapOperator(ctx context.Context, env *Env, n config.NodeSpec, name string) error {
	ops, err := config.LoadOperators(config.OperatorsPath)
	if err != nil || len(ops.Operators) == 0 {
		return err
	}

	if len(ops.Operators) != 1 || ops.Operators[0].Name != name {
		return deviceAccessDenied()
	}

	if err := steps.ClearBootstrapSSH(ctx, env.Cluster, runtimeFor(env, false), env.Report); err != nil {
		return err
	}

	op := ops.Operators[0]
	if _, err := temporaryAccessCommand(ctx, "wg", "set", "wg0", "peer", op.WGPublicKey, "remove"); err != nil {
		return err
	}

	if err := closeAccessSSH(ctx, n.WGAddress, op.WGAddress); err != nil {
		return err
	}

	raw, err := (config.Operators{}).Encode()
	if err != nil {
		return err
	}

	if err := engine.WriteFileAtomic(config.OperatorsPath, raw, config.FileMode); err != nil {
		return err
	}

	masterbackup.MarkDirty("bootstrap-access-retired")

	return nil
}
