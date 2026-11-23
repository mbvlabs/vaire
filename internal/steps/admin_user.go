package steps

import (
	"context"
	"fmt"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

const adminUserID = "admin-user-v1"

// SudoersPath lets the admin user run the node RPC endpoint as root.
const SudoersPath = "/etc/sudoers.d/vaire"

// SecretSudoPassword is the request secret holding the break-glass SSH password.
const SecretSudoPassword = "sudo_password"

type adminUser struct{}

func (adminUser) ID() string { return adminUserID }

func (adminUser) Describe(cfg config.Cluster) string {
	return "create admin user " + cfg.Server.AdminUser + " with the operator SSH keys and the vaire sudoers rule"
}

// adminUserRender is everything step 2 writes. The break-glass password is applied
// when the request carries one and is never part of the inputs.
type adminUserRender struct {
	User           string `json:"user"`
	AuthorizedKeys string `json:"authorized_keys"`
	Sudoers        string `json:"sudoers"`
}

func renderAdminUser(cfg config.Cluster) (adminUserRender, error) {
	if err := config.CheckAdminUser(cfg.Server.AdminUser); err != nil {
		return adminUserRender{}, fmt.Errorf("server.admin_user: %w", err)
	}

	ops, err := config.LoadOperators(config.OperatorsPath)
	if err != nil {
		return adminUserRender{}, err
	}

	var keys strings.Builder
	keys.WriteString("# managed by vaire: operator keys from " + config.OperatorsPath + "\n")

	for _, op := range ops.Operators {
		keys.WriteString(strings.TrimSpace(op.SSHPublicKey) + "\n")
	}

	return adminUserRender{
		User:           cfg.Server.AdminUser,
		AuthorizedKeys: keys.String(),
		Sudoers:        adminSudoers(cfg.Server.AdminUser),
	}, nil
}

// ClearBootstrapSSH removes setup keys without changing the console recovery account.
func ClearBootstrapSSH(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) error {
	rt.SecretValues = nil

	_, err := (adminUser{}).apply(ctx, adminUserRender{
		User: cfg.Server.AdminUser, Sudoers: adminSudoers(cfg.Server.AdminUser),
		AuthorizedKeys: "# managed by vaire: operator keys from " + config.OperatorsPath + "\n",
	}, rt, report)

	return err
}

func adminSudoers(user string) string {
	return "# managed by vaire\n" + user + " ALL=(root) NOPASSWD: /usr/local/bin/vaire node rpc\n"
}

func (adminUser) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	r, err := renderAdminUser(cfg)
	if err != nil {
		return nil, err
	}

	return canonical(r)
}

func (adminUser) Check(_ context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	r, err := renderAdminUser(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}

	u, err := user.Lookup(r.User)
	if err != nil {
		return incomplete("user " + r.User + " does not exist"), nil
	}

	if err := checkAdminPrivileges(u); err != nil {
		return engine.CheckResult{}, err
	}

	if !fileIs(filepath.Join(u.HomeDir, ".ssh", "authorized_keys"), r.AuthorizedKeys, 0o600) {
		return incomplete("authorized_keys differs"), nil
	}

	if !fileIs(SudoersPath, r.Sudoers, 0o440) {
		return incomplete(SudoersPath + " differs"), nil
	}

	return complete(), nil
}

// Common administrative groups grant root or equivalent access outside the
// managed RPC rule. This cannot detect every host-specific privilege.
// Refuse existing privileged accounts rather than changing their access.
func checkAdminPrivileges(u *user.User) error {
	if u.Uid == "0" {
		return fmt.Errorf("admin user %s has UID 0; choose an unprivileged account", u.Username)
	}

	groups, err := u.GroupIds()
	if err != nil {
		return fmt.Errorf("cannot check admin user groups: %w", err)
	}

	for _, id := range groups {
		group, err := user.LookupGroupId(id)
		if err != nil {
			return fmt.Errorf("cannot check admin user group %s: %w", id, err)
		}

		switch group.Name {
		case "sudo", "wheel", "admin", "root", "docker", "lxd", "incus", "disk", "libvirt":
			return fmt.Errorf("admin user %s belongs to privileged group %s; choose an unprivileged account", u.Username, group.Name)
		}
	}

	return nil
}

func (s adminUser) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if !rt.TrustedHostAdmin {
		return engine.ApplyResult{}, output.NewError(output.CodePrecondition, "host account changes require a trusted root session",
			output.ExitPrecondition, "Use the provider console to apply the admin-user-v1 step.")
	}

	r, err := renderAdminUser(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}

	return s.apply(ctx, r, rt, report)
}

func (s adminUser) apply(ctx context.Context, r adminUserRender, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	env := map[string]string{
		"ADMIN_USER":      r.User,
		"AUTHORIZED_KEYS": r.AuthorizedKeys,
		"SUDOERS":         r.Sudoers,
		"SUDOERS_PATH":    SudoersPath,
	}
	if pw := rt.SecretValues[SecretSudoPassword]; pw != "" {
		env["SUDO_PASSWORD"] = pw
	}

	err := rt.Shell.Run(ctx, s.ID(), Script("admin-user"), env, report)

	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
