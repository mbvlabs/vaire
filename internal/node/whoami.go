package node

import (
	"context"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func handleWhoami(_ context.Context, env *Env) (Result, error) {
	var identity rpc.WhoamiReply
	if env.DeviceIdentity != nil {
		identity = *env.DeviceIdentity
	} else {
		account, err := legacySSHAccount(os.Getenv("SUDO_UID"), env.Cluster.Server.AdminUser)
		if err != nil {
			return Result{}, err
		}

		identity.SSHAccount = account
		identity.IdentityKind = rpc.IdentityLegacyShared
	}

	node, err := config.LoadNode(config.NodePath)
	if err != nil {
		return Result{}, err
	}

	identity.Server, identity.Node = node.Server, node.NodeName

	return Result{Data: identity}, nil
}

var lookupDeviceUID = user.LookupId
var deviceRegistryPath = config.AccessDevicesPath
var loadDeviceRegistry = readDeviceRegistry

// SUDO_UID is trusted only because the installed sudo rule resets the
// environment. Direct root sessions remain trusted host administration.
func authorizeDeviceCaller(uid, method string) (*rpc.WhoamiReply, error) {
	if uid == "" || uid == "0" {
		return nil, nil
	}

	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != uid {
		return nil, deviceAccessDenied()
	}

	account, err := lookupDeviceUID(uid)
	if err != nil || account == nil || account.Uid != uid {
		return nil, deviceAccessDenied()
	}

	if !strings.HasPrefix(account.Username, "vdev-") {
		return nil, nil
	}

	// RC1 allowed arbitrary admin names, including vdev- names. Such an
	// account remains shared; future provisioning must refuse this collision.
	cluster, err := loadGuardCluster()
	if err != nil {
		return nil, deviceAccessDenied()
	}

	if account.Username == cluster.Server.AdminUser {
		return nil, nil
	}

	registry, err := loadDeviceRegistry(deviceRegistryPath)
	if err != nil {
		return nil, deviceAccessDenied()
	}

	return registryDeviceIdentity(registry, account.Username, uid, method)
}

// These are deliberately explicit: new RPCs do not grant device authority.
// Managed owners may perform supported database operations without inheriting
// the recovery account's host authority.
func managedDeviceMethod(role, method string) bool {
	switch method {
	case "whoami", "status", "config.get", "db.list", "db.access.show", "ext.list",
		"tune.show", "tls.status", "backup.list", "restore.status",
		"activity.list", "activity.show", "activity.health", "protect.status":
		return role == config.AccessRoleOwner || role == config.AccessRoleMember
	case "user.add", "user.list", "user.revoke", "device.add", "device.list", "device.revoke", "access.recovery-issue", "access.complete-setup",
		"plan", "db.create", "db.recover", "db.write-access", "auth.status", "auth.setup",
		"backup.now", "master-backup.now", "master-backup.list", "master-backup.get", "upgrade.install":
		return role == config.AccessRoleOwner
	default:
		return false
	}
}

func registryDeviceIdentity(registry config.AccessRegistry, login, uid, method string) (*rpc.WhoamiReply, error) {
	for _, device := range registry.Devices {
		if device.UnixLogin != login {
			continue
		}

		if device.State != config.DeviceActive || device.Cleanup || device.UnixUID != uid {
			return nil, deviceAccessDenied()
		}

		for _, operator := range registry.Operators {
			if operator.ID == device.OperatorID && !operator.Revoked && managedDeviceMethod(operator.Role, method) {
				return &rpc.WhoamiReply{SSHAccount: login, IdentityKind: rpc.IdentityVerifiedDevice,
					OperatorID: operator.ID, Operator: operator.Name, DeviceID: device.ID, Device: device.Label, Role: operator.Role}, nil
			}
		}
	}

	return nil, deviceAccessDenied()
}

// /etc is assumed root-controlled. /etc/vaire may be 0755, but only root
// may replace its entries. Open the registry without following a symlink and
// check the opened inode, so a file replacement cannot change what is read.
func readDeviceRegistry(path string) (config.AccessRegistry, error) {
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || !rootOwned(dir) || dir.Mode().Perm()&0o022 != 0 || dir.Mode()&os.ModeType != os.ModeDir {
		return config.AccessRegistry{}, deviceAccessDenied()
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return config.AccessRegistry{}, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !rootOwned(info) || info.Mode().Perm()&0o077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return config.AccessRegistry{}, deviceAccessDenied()
	}

	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return config.AccessRegistry{}, deviceAccessDenied()
	}

	return config.DecodeAccessRegistry(raw)
}

var rootOwned = func(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func deviceAccessDenied() error {
	return output.NewError(output.CodePrecondition, "managed device access denied", output.ExitPrecondition,
		"Contact a trusted host administrator.").WithDetails(map[string]any{"reason": "device_access_denied"})
}

// Sudo sets SUDO_UID for the exact node RPC command. This identifies an OS
// account, not the person or device using the shared RC1 SSH login.
func legacySSHAccount(sudoUID, adminUser string) (string, error) {
	if sudoUID != "" && sudoUID != "0" {
		if account, err := user.LookupId(sudoUID); err == nil && account.Username == adminUser {
			return account.Username, nil
		}
	}

	return "", output.NewError(output.CodePrecondition, "no recognized Vaire operator login",
		output.ExitPrecondition, "Run vaire whoami from a connected workspace.")
}
