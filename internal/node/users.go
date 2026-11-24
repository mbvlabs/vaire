package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func authorizeUserManagement(env *Env) error {
	if env.DeviceIdentity != nil {
		id := env.DeviceIdentity
		if id.IdentityKind == rpc.IdentityVerifiedDevice && id.OperatorID != "" && id.DeviceID != "" && id.Role == config.AccessRoleOwner {
			return nil
		}

		return deviceAccessDenied()
	}

	_, err := legacySSHAccount(os.Getenv("SUDO_UID"), env.Cluster.Server.AdminUser)
	if err != nil {
		return deviceAccessDenied()
	}

	return nil
}

func userRegistry() (config.AccessRegistry, error) {
	r, err := readDeviceRegistry(deviceRegistryPath)
	if errors.Is(err, os.ErrNotExist) {
		return config.AccessRegistry{}, nil
	}

	return r, err
}

func handleUserAdd(_ context.Context, env *Env) (Result, error) {
	if err := authorizeUserManagement(env); err != nil {
		return Result{}, err
	}

	var p rpc.UserAddParams
	if err := json.Unmarshal(env.Req.Params, &p); err != nil {
		return Result{}, output.NewError(output.CodeValidation, "invalid user request", output.ExitPrecondition, "Provide a user name and role.")
	}

	if p.Role == "" {
		p.Role = config.AccessRoleMember
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	id, err := newAccessID("op-")
	if err != nil {
		return Result{}, err
	}

	op := config.AccessOperator{ID: id, Name: p.Name, Role: p.Role}
	if err := r.AddOperator(op); err != nil {
		return Result{}, output.NewError(output.CodeValidation, err.Error(), output.ExitPrecondition, "Use a unique lowercase user name and role admin or member.")
	}

	if !env.Req.DryRun {
		if err := saveAccessRegistry(r); err != nil {
			return Result{}, err
		}
	}

	return Result{Data: rpc.User{ID: op.ID, Name: op.Name, Role: op.Role, Status: "active"}}, nil
}

func handleUserList(_ context.Context, env *Env) (Result, error) {
	if err := authorizeUserManagement(env); err != nil {
		return Result{}, err
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	users := make([]rpc.User, 0, len(r.Operators))
	for _, op := range r.Operators {
		users = append(users, accessUser(op))
	}

	return Result{Data: rpc.UsersReply{Users: users}}, nil
}

func newAccessID(prefix string) (string, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}

	return prefix + hex.EncodeToString(id[:]), nil
}

func saveAccessRegistry(r config.AccessRegistry) error {
	raw, err := r.Encode()
	if err != nil {
		return err
	}

	return engine.WriteFileAtomic(deviceRegistryPath, raw, 0o600)
}

func accessUser(op config.AccessOperator) rpc.User {
	status := "active"
	if op.Revoked {
		status = "revoked"
	}

	return rpc.User{ID: op.ID, Name: op.Name, Role: op.Role, Status: status}
}
