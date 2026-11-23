package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

const (
	deviceSSHStateDir  = "/var/lib/vaire/device-ssh"
	deviceSSHKeyDir    = "/etc/ssh/vaire-device-keys"
	deviceSSHConfigDir = "/etc/ssh/sshd_config.d"
	deviceSudoDir      = "/etc/sudoers.d"
)

// Device logins are also validated by the access registry. Validate again at
// this boundary because these values are interpolated into host configuration.
var deviceHostLoginPattern = regexp.MustCompile(`^vdev-[a-z0-9][a-z0-9_-]{0,25}$`)

type deviceHostSSHConfig struct {
	AuthorizedKey string
	Sudoers       string
}

func renderDeviceHostSSH(admin, login, source, publicKey string) (deviceHostSSHConfig, error) {
	if !deviceHostLoginPattern.MatchString(login) || login == admin {
		return deviceHostSSHConfig{}, fmt.Errorf("invalid or conflicting managed device login")
	}

	key, err := steps.RenderAccessSSHAuthorizedKey(source, publicKey)
	if err != nil {
		return deviceHostSSHConfig{}, err
	}

	return deviceHostSSHConfig{
		AuthorizedKey: key,
		Sudoers:       renderDeviceSudo(login),
	}, nil
}

// prepareDeviceSSH resumes only an account bound to a root-owned intent created
// before useradd. The caller persists the returned UID before installing a key.
func prepareDeviceSSH(ctx context.Context, login string) (string, error) {
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("device SSH provisioning requires root")
	}

	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return "", err
	}

	if !deviceHostLoginPattern.MatchString(login) || login == cfg.Server.AdminUser {
		return "", fmt.Errorf("invalid or conflicting managed device login")
	}

	if err := ensureDeviceSSHDirs(); err != nil {
		return "", err
	}

	intent, err := prepareDeviceIntent(login)
	if err != nil {
		return "", err
	}

	account, err := prepareDeviceAccount(ctx, login, intent)
	if err != nil {
		return "", err
	}

	if err := ensureDeviceFile(deviceMarkerPath(login), account.Uid+"\n"); err != nil {
		return "", err
	}

	if err := ensureDeviceSudo(ctx, login); err != nil {
		return "", err
	}

	created, err := ensureDeviceSSHD(login)
	if err != nil {
		return "", err
	}

	if err := validateDeviceSSHD(ctx, login, "127.0.0.1"); err != nil {
		if created {
			_ = os.Remove(deviceSSHDPath(login))
		}

		return "", err
	}

	if err := deviceCommand(ctx, "systemctl", "reload", "ssh.service"); err != nil {
		if created {
			_ = os.Remove(deviceSSHDPath(login))
		}

		return "", err
	}

	return account.Uid, nil
}

func prepareDeviceIntent(login string) (string, error) {
	path := deviceIntentPath(login)

	intent, err := readDeviceIntent(path)
	if err == nil {
		return intent, nil
	}

	if !os.IsNotExist(err) {
		return "", err
	}

	if _, err := user.Lookup(login); err == nil {
		return "", fmt.Errorf("device login already exists without provisioning intent")
	} else {
		if _, ok := errors.AsType[user.UnknownUserError](err); !ok {
			return "", fmt.Errorf("lookup device login: %w", err)
		}
	}

	for _, existing := range []string{deviceMarkerPath(login), deviceKeyPath(login), deviceSSHDPath(login), deviceSudoPath(login)} {
		if _, err := os.Lstat(existing); err == nil {
			return "", fmt.Errorf("device provisioning path already exists: %s", existing)
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}

	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}

	intent = hex.EncodeToString(random[:])
	if err := writeNewDeviceFile(path, intent+"\n", 0o600); err != nil {
		return "", err
	}

	return intent, nil
}

func prepareDeviceAccount(ctx context.Context, login, intent string) (*user.User, error) {
	account, err := user.Lookup(login)
	if err != nil { //nolint:nestif // Resume only a not-yet-created account, never a reserved missing one.
		if _, ok := errors.AsType[user.UnknownUserError](err); !ok {
			return nil, fmt.Errorf("lookup device login: %w", err)
		}
		// Once a UID is reserved, never replace a deleted account.
		if _, err := os.Lstat(deviceMarkerPath(login)); err == nil {
			return nil, fmt.Errorf("reserved device account is missing")
		} else if !os.IsNotExist(err) {
			return nil, err
		}

		if err := deviceCommand(ctx, "useradd", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/bin/sh", "--user-group", "--password", "!", "--comment", "vaire-device-"+intent, login); err != nil {
			return nil, err
		}

		account, err = user.Lookup(login)
		if err != nil {
			return nil, err
		}
	}

	if err := checkDeviceAccount(account, login, account.Uid); err != nil {
		return nil, err
	}

	if err := verifyDeviceAccount(ctx, account, intent); err != nil {
		return nil, err
	}

	return account, nil
}

func verifyDeviceAccount(ctx context.Context, account *user.User, intent string) error {
	if !deviceHostLoginPattern.MatchString(account.Username) {
		return fmt.Errorf("invalid device login")
	}

	passwd, err := exec.CommandContext(ctx, "/usr/bin/getent", "passwd", account.Username).Output() //nolint:gosec // Validated login is one argument, without a shell.
	if err != nil {
		return fmt.Errorf("cannot inspect device account: %w", err)
	}

	status, err := exec.CommandContext(ctx, "/usr/bin/passwd", "-S", account.Username).Output() //nolint:gosec // Validated login is one argument, without a shell.
	if err != nil {
		return fmt.Errorf("cannot inspect device account lock: %w", err)
	}

	return checkDeviceAccountAttributes(account, intent, string(passwd), string(status))
}

func checkDeviceAccountAttributes(account *user.User, intent, passwd, status string) error {
	fields := strings.Split(strings.TrimSuffix(passwd, "\n"), ":")

	lock := strings.Fields(status)
	if len(fields) != 7 || fields[0] != account.Username || fields[2] != account.Uid ||
		fields[3] != account.Gid || fields[4] != "vaire-device-"+intent ||
		fields[5] != "/nonexistent" || fields[6] != "/bin/sh" ||
		len(lock) < 2 || lock[0] != account.Username || lock[1] != "L" {
		return fmt.Errorf("device account does not match locked provisioning intent")
	}

	return nil
}

func installDeviceSSHKey(ctx context.Context, login, address, publicKey string) error {
	if os.Geteuid() != 0 || !deviceHostLoginPattern.MatchString(login) {
		return fmt.Errorf("invalid device login or insufficient privileges")
	}

	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("invalid device SSH address")
	}

	rendered, err := renderDeviceHostSSH("", login, addr.String()+"/32", publicKey)
	if err != nil {
		return err
	}

	if err := ensureDeviceSSHDirs(); err != nil {
		return err
	}

	uid, err := readDeviceMarker(login)
	if err != nil {
		return err
	}

	registry, err := loadDeviceRegistry(deviceRegistryPath)
	if err != nil {
		return err
	}

	if err := checkDeviceSSHBinding(registry, login, uid, addr.String(), publicKey); err != nil {
		return err
	}

	account, err := user.Lookup(login)
	if err != nil {
		return err
	}

	if err := checkDeviceAccount(account, login, uid); err != nil {
		return err
	}

	intent, err := readDeviceIntent(deviceIntentPath(login))
	if err != nil {
		return err
	}

	if err := verifyDeviceAccount(ctx, account, intent); err != nil {
		return err
	}

	if err := checkDeviceFile(deviceSSHDPath(login), renderDeviceSSHD(login), 0o644); err != nil {
		return err
	}

	if err := checkDeviceFile(deviceSudoPath(login), rendered.Sudoers, 0o440); err != nil {
		return err
	}

	if err := validateDeviceSSHD(ctx, login, addr.String()); err != nil {
		return err
	}

	return writeDeviceSSHKey(ctx, login, rendered.AuthorizedKey)
}

func writeDeviceSSHKey(ctx context.Context, login, key string) error {
	// Only root can replace entries in this directory. Atomic rename ensures
	// sshd never observes a partially written key.
	f, err := os.CreateTemp(deviceSSHKeyDir, ".key-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()

	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}

	if _, err := f.WriteString(key); err != nil {
		_ = f.Close()
		return err
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.Rename(f.Name(), deviceKeyPath(login)); err != nil {
		return err
	}

	return syncDeviceDir(deviceSSHKeyDir)
}

// Revocation removes only the key. The account and its UID remain reserved.
func removeDeviceSSHKey(_ context.Context, login string) error {
	if os.Geteuid() != 0 || !deviceHostLoginPattern.MatchString(login) {
		return fmt.Errorf("invalid device login or insufficient privileges")
	}

	if err := checkDeviceDir(deviceSSHKeyDir, 0o755); err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	err := os.Remove(deviceKeyPath(login))
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	return syncDeviceDir(deviceSSHKeyDir)
}

func renderDeviceSudo(login string) string {
	return "# managed by vaire\n" + login + " ALL=(root) NOPASSWD: /usr/local/bin/vaire node rpc\n"
}

func checkDeviceSSHBinding(registry config.AccessRegistry, login, uid, address, key string) error {
	for _, device := range registry.Devices {
		if device.UnixLogin == login && device.UnixUID == uid &&
			(device.State == config.DevicePending || device.State == config.DeviceActive) &&
			device.Address == address && device.SSHPublicKey == key {
			return nil
		}
	}

	return fmt.Errorf("device SSH identity is not bound to the registry")
}

func renderDeviceSSHD(login string) string {
	return "# managed by vaire\nMatch User " + login + "\n    AllowUsers " + login + "\n" +
		"    AuthorizedKeysFile " + deviceKeyPath(login) + "\n" +
		"    AuthorizedKeysCommand none\n    TrustedUserCAKeys none\n" +
		"    ForceCommand /usr/local/bin/vaire node access-ssh\n" +
		"    AuthenticationMethods publickey\n    PubkeyAuthentication yes\n" +
		"    PasswordAuthentication no\n    KbdInteractiveAuthentication no\n" +
		"    PermitTTY no\n    PermitUserRC no\n    DisableForwarding yes\nMatch all\n"
}

func deviceMarkerPath(login string) string { return filepath.Join(deviceSSHStateDir, login) }
func deviceIntentPath(login string) string { return filepath.Join(deviceSSHStateDir, login+".intent") }
func deviceKeyPath(login string) string    { return filepath.Join(deviceSSHKeyDir, login) }
func deviceSSHDPath(login string) string {
	return filepath.Join(deviceSSHConfigDir, "00-vaire-device-"+login+".conf")
}
func deviceSudoPath(login string) string { return filepath.Join(deviceSudoDir, "vaire-device-"+login) }

func ensureDeviceSSHDirs() error {
	for _, path := range []string{"/var/lib/vaire", "/etc/ssh", "/etc"} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		if !info.IsDir() || !rootOwned(info) || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("unsafe device SSH parent directory %s", path)
		}
	}

	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{deviceSSHStateDir, 0o700}, {deviceSSHKeyDir, 0o755},
		{deviceSSHConfigDir, 0o755}, {deviceSudoDir, 0o755},
	} {
		if err := os.Mkdir(dir.path, dir.mode); err != nil && !os.IsExist(err) {
			return err
		}

		if dir.path == deviceSudoDir {
			info, err := os.Lstat(dir.path)
			if err != nil || !info.IsDir() || !rootOwned(info) || info.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("unsafe device SSH directory %s", dir.path)
			}

			continue
		}

		if err := checkDeviceDir(dir.path, dir.mode); err != nil {
			return err
		}
	}

	return nil
}

func checkDeviceDir(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if !info.IsDir() || info.Mode() != os.ModeDir|mode || !rootOwned(info) {
		return fmt.Errorf("unsafe device SSH directory %s", path)
	}

	return nil
}

func writeNewDeviceFile(path, content string, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".vaire-device-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()

	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}

	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	if err := os.Link(f.Name(), path); err != nil {
		return err
	}

	return syncDeviceDir(filepath.Dir(path))
}

func ensureDeviceFile(path, content string) error {
	err := checkDeviceFile(path, content, 0o600)
	if os.IsNotExist(err) {
		return writeNewDeviceFile(path, content, 0o600)
	}

	return err
}

func ensureDeviceSudo(ctx context.Context, login string) error {
	path := deviceSudoPath(login)

	err := checkDeviceFile(path, renderDeviceSudo(login), 0o440)
	if os.IsNotExist(err) {
		return writeDeviceSudo(ctx, login)
	}

	if err != nil {
		return err
	}

	return deviceCommand(ctx, "visudo", "-cf", path)
}

func ensureDeviceSSHD(login string) (bool, error) {
	path := deviceSSHDPath(login)
	content := renderDeviceSSHD(login)

	err := checkDeviceFile(path, content, 0o644)
	if os.IsNotExist(err) {
		return true, writeNewDeviceFile(path, content, 0o644)
	}

	return false, err
}

func syncDeviceDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()

	return dir.Sync()
}

func writeDeviceSudo(ctx context.Context, login string) error {
	f, err := os.CreateTemp(deviceSudoDir, ".vaire-device-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()

	if err := f.Chmod(0o440); err != nil {
		_ = f.Close()
		return err
	}

	if _, err := f.WriteString(renderDeviceSudo(login)); err != nil {
		_ = f.Close()
		return err
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	if err := deviceCommand(ctx, "visudo", "-cf", f.Name()); err != nil {
		return err
	}

	if err := os.Link(f.Name(), deviceSudoPath(login)); err != nil {
		return err
	}

	return syncDeviceDir(deviceSudoDir)
}

func checkDeviceFile(path, content string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() || info.Mode().Perm() != mode || info.Mode() != mode || !rootOwned(info) {
		return fmt.Errorf("unsafe device SSH file %s", path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if string(raw) != content {
		return fmt.Errorf("unexpected device SSH file %s", path)
	}

	return nil
}

func readDeviceMarker(login string) (string, error) {
	path := deviceMarkerPath(login)

	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}

	if !info.Mode().IsRegular() || info.Mode() != 0o600 || !rootOwned(info) || info.Size() > 16 {
		return "", fmt.Errorf("unsafe device UID marker")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	uid := strings.TrimSuffix(string(raw), "\n")

	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != uid || string(raw) != uid+"\n" {
		return "", fmt.Errorf("invalid device UID marker")
	}

	return uid, nil
}

func readDeviceIntent(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}

	if !info.Mode().IsRegular() || info.Mode() != 0o600 || !rootOwned(info) || info.Size() != 65 {
		return "", fmt.Errorf("unsafe device provisioning intent")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	value := strings.TrimSuffix(string(raw), "\n")

	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != value || string(raw) != value+"\n" {
		return "", fmt.Errorf("invalid device provisioning intent")
	}

	return value, nil
}

func checkDeviceAccount(account *user.User, login, uid string) error {
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != uid || account.Username != login || account.Uid != uid {
		return fmt.Errorf("device account UID mismatch")
	}

	byUID, err := user.LookupId(uid)
	if err != nil || byUID.Username != login {
		return fmt.Errorf("device UID is not unique")
	}

	groups, err := account.GroupIds()
	if err != nil || len(groups) != 1 || groups[0] != account.Gid || account.Gid == "0" {
		return fmt.Errorf("device account has unexpected groups")
	}

	return nil
}

func deviceCommand(ctx context.Context, name string, args ...string) error {
	paths := map[string]string{
		"useradd": "/usr/sbin/useradd", "visudo": "/usr/sbin/visudo",
		"sshd": "/usr/sbin/sshd", "systemctl": "/usr/bin/systemctl",
	}

	path, ok := paths[name]
	if !ok {
		return fmt.Errorf("unsupported device provisioning command %q", name)
	}

	cmd := exec.CommandContext(ctx, path, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}

	return nil
}

func validateDeviceSSHD(ctx context.Context, login, address string) error {
	addr, err := netip.ParseAddr(address)
	if !deviceHostLoginPattern.MatchString(login) || err != nil || !addr.Is4() || addr.String() != address {
		return fmt.Errorf("invalid sshd validation identity")
	}

	if err := deviceCommand(ctx, "sshd", "-t"); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "/usr/sbin/sshd", "-T", "-C", "user="+login+",host="+address+",addr="+address) //nolint:gosec // Login and address are validated above; no shell is used.

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sshd effective configuration: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return checkDeviceSSHD(string(out), login)
}

func checkDeviceSSHD(effective, login string) error {
	lines := "\n" + effective
	for _, expected := range []string{
		"authorizedkeysfile " + deviceKeyPath(login), "authorizedkeyscommand none",
		"trustedusercakeys none", "forcecommand /usr/local/bin/vaire node access-ssh",
		"authenticationmethods publickey", "pubkeyauthentication yes", "passwordauthentication no",
		"kbdinteractiveauthentication no", "permittty no", "permituserrc no",
		"permituserenvironment no", "disableforwarding yes",
		"usepam yes", "usedns no", "allowusers " + login,
	} {
		if !strings.Contains(lines, "\n"+expected+"\n") {
			return fmt.Errorf("sshd effective configuration does not contain %q", expected)
		}
	}

	return nil
}
