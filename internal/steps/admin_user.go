package steps

import (
	"context"
	"errors"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

const adminUserID = "admin-user-v1"

// SudoersPath lets the admin user run eru node commands as root (§7.4).
const SudoersPath = "/etc/sudoers.d/eru"

// SecretSudoPassword is the request secret holding the admin sudo password.
const SecretSudoPassword = "sudo_password"

type adminUser struct{}

func (adminUser) ID() string { return adminUserID }

func (adminUser) Describe(cfg config.Cluster) string {
	return "create admin user " + cfg.Server.AdminUser + " with the operator SSH keys and the eru sudoers rule"
}

// adminUserRender is everything step 2 writes. The sudo password is applied
// when the request carries one and is never part of the inputs.
type adminUserRender struct {
	User           string `json:"user"`
	AuthorizedKeys string `json:"authorized_keys"`
	Sudoers        string `json:"sudoers"`
}

func renderAdminUser(cfg config.Cluster) (adminUserRender, error) {
	ops, err := config.LoadOperators(config.OperatorsPath)
	if err != nil {
		return adminUserRender{}, err
	}
	if len(ops.Operators) == 0 {
		return adminUserRender{}, errors.New("operators.toml lists no operator; refusing to install an empty authorized_keys")
	}
	var keys strings.Builder
	keys.WriteString("# managed by eru: operator keys from " + config.OperatorsPath + "\n")
	for _, op := range ops.Operators {
		keys.WriteString(strings.TrimSpace(op.SSHPublicKey) + "\n")
	}
	return adminUserRender{
		User:           cfg.Server.AdminUser,
		AuthorizedKeys: keys.String(),
		Sudoers:        "# managed by eru\n" + cfg.Server.AdminUser + " ALL=(root) NOPASSWD: /usr/local/bin/eru node *\n",
	}, nil
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
	if !fileIs(filepath.Join(u.HomeDir, ".ssh", "authorized_keys"), r.AuthorizedKeys, 0o600) {
		return incomplete("authorized_keys differs"), nil
	}
	if !fileIs(SudoersPath, r.Sudoers, 0o440) {
		return incomplete(SudoersPath + " differs"), nil
	}
	return complete(), nil
}

func (s adminUser) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	r, err := renderAdminUser(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	env := map[string]string{
		"ADMIN_USER":      r.User,
		"AUTHORIZED_KEYS": r.AuthorizedKeys,
		"SUDOERS":         r.Sudoers,
		"SUDOERS_PATH":    SudoersPath,
	}
	if pw := rt.SecretValues[SecretSudoPassword]; pw != "" {
		env["SUDO_PASSWORD"] = pw
	}
	err = rt.Shell.Run(ctx, s.ID(), Script("admin-user"), env, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
