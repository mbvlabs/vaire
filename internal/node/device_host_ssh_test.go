package node

import (
	"context"
	"crypto/ed25519"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"golang.org/x/crypto/ssh"
)

func TestDeviceSSHIntentAndFileRecovery(t *testing.T) {
	oldRootOwned := rootOwned
	rootOwned = func(os.FileInfo) bool { return true }

	t.Cleanup(func() { rootOwned = oldRootOwned })
	dir := t.TempDir()
	path := filepath.Join(dir, "intent")

	intent := strings.Repeat("ab", 32)
	if err := writeNewDeviceFile(path, intent+"\n", 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := readDeviceIntent(path); err != nil || got != intent {
		t.Fatalf("read intent: %q, %v", got, err)
	}

	if err := os.WriteFile(path, []byte(strings.ToUpper(intent)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceIntent(path); err == nil {
		t.Fatal("accepted noncanonical intent")
	}

	if err := os.WriteFile(path, []byte(intent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config")
	if err := ensureDeviceFile(configPath, "expected\n"); err != nil {
		t.Fatal(err)
	}

	if err := ensureDeviceFile(configPath, "expected\n"); err != nil {
		t.Fatalf("matching retry failed: %v", err)
	}

	if err := ensureDeviceFile(configPath, "different\n"); err == nil {
		t.Fatal("accepted unrelated existing file")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceIntent(path); err == nil {
		t.Fatal("accepted readable intent")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(configPath, path); err != nil {
		t.Fatal(err)
	}

	if _, err := readDeviceIntent(path); err == nil {
		t.Fatal("accepted symlink intent")
	}
}

func TestDeviceSSHAccountIntentAndLock(t *testing.T) {
	account := &user.User{Username: "vdev-1", Uid: "123", Gid: "456"}
	intent := strings.Repeat("ab", 32)
	passwd := "vdev-1:x:123:456:vaire-device-" + intent + ":/nonexistent:/bin/sh\n"

	status := "vdev-1 L 01/01/2026 0 99999 7 -1\n"
	if err := checkDeviceAccountAttributes(account, intent, passwd, status); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ passwd, status string }{
		{strings.Replace(passwd, intent, strings.Repeat("cd", 32), 1), status},
		{passwd, strings.Replace(status, " L ", " P ", 1)},
		{strings.Replace(passwd, "/bin/sh", "/bin/bash", 1), status},
		{strings.Replace(passwd, "/nonexistent", "/home/vdev-1", 1), status},
		{strings.Replace(passwd, ":123:", ":124:", 1), status},
		{passwd, "other L\n"},
	} {
		if err := checkDeviceAccountAttributes(account, intent, tc.passwd, tc.status); err == nil {
			t.Errorf("accepted unexpected account: %q %q", tc.passwd, tc.status)
		}
	}
}

func TestDeviceSSHDValidationRejectsInvalidIdentity(t *testing.T) {
	for _, tc := range []struct{ login, address string }{
		{"root", "127.0.0.1"},
		{"vdev-1", "127.0.0.1,host=attacker"},
		{"vdev-1", "::1"},
	} {
		if err := validateDeviceSSHD(context.Background(), tc.login, tc.address); err == nil ||
			!strings.Contains(err.Error(), "invalid sshd validation identity") {
			t.Errorf("validation of %q, %q: %v", tc.login, tc.address, err)
		}
	}
}

func deviceHostTestKey(t *testing.T) string {
	t.Helper()

	key, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, 32)).Public())
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(key)), "\n")
}

func TestDeviceSSHRegistryBinding(t *testing.T) {
	d := config.AccessDevice{UnixLogin: "vdev-1", UnixUID: "123", State: config.DevicePending, Address: "10.0.0.1", SSHPublicKey: deviceHostTestKey(t)}

	r := config.AccessRegistry{Devices: []config.AccessDevice{d}}
	if err := checkDeviceSSHBinding(r, d.UnixLogin, d.UnixUID, d.Address, d.SSHPublicKey); err != nil {
		t.Fatal(err)
	}

	for _, change := range []func(*config.AccessDevice){
		func(d *config.AccessDevice) { d.UnixUID = "124" },
		func(d *config.AccessDevice) { d.State = config.DeviceRevoked },
		func(d *config.AccessDevice) { d.Address = "10.0.0.2" },
		func(d *config.AccessDevice) { d.SSHPublicKey += " comment" },
	} {
		r.Devices[0] = d
		change(&r.Devices[0])

		if err := checkDeviceSSHBinding(r, d.UnixLogin, d.UnixUID, d.Address, d.SSHPublicKey); err == nil {
			t.Error("accepted mismatched device binding")
		}
	}
}

func TestDeviceSSHNewFileDoesNotAdoptOrOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")
	if err := writeNewDeviceFile(path, "123\n", 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeNewDeviceFile(path, "456\n", 0o600); err == nil {
		t.Fatal("overwrote marker")
	}

	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "123\n" {
		t.Fatalf("marker: %q, %v", raw, err)
	}
}

func TestDeviceSSHSudoersParses(t *testing.T) {
	visudo, err := exec.LookPath("visudo")
	if err != nil {
		t.Skip("visudo not installed")
	}

	path := filepath.Join(t.TempDir(), "sudoers")
	if err := os.WriteFile(path, []byte(renderDeviceSudo("vdev-1")), 0o440); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(visudo, "-cf", path).CombinedOutput(); err != nil {
		t.Fatalf("visudo rejected sudoers: %s: %v", out, err)
	}
}

// Exercise the actual sshd parser without starting or reloading a server.
func TestDeviceSSHRealSSHDConfig(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("sshd not installed")
	}

	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}

	dir := t.TempDir()

	hostKey := filepath.Join(dir, "hostkey")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", hostKey).CombinedOutput(); err != nil {
		t.Fatalf("generate test host key: %s: %v", out, err)
	}

	path := filepath.Join(dir, "sshd_config")
	// The device drop-in sorts before 00-vaire.conf. The latter is what
	// normal sshd reapplication rewrites.
	content := "HostKey " + hostKey + "\nUsePAM yes\n" + renderDeviceSSHD("vdev-1") + renderDeviceSSHD("vdev-2") +
		"AllowUsers admin\nPubkeyAuthentication yes\nPasswordAuthentication no\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	effective := func(login string) string {
		t.Helper()

		out, err := exec.Command(sshd, "-T", "-f", path, "-C", "user="+login+",host=localhost,addr=10.0.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("sshd -T: %s: %v", out, err)
		}

		return string(out)
	}

	device := effective("vdev-1")
	if err := checkDeviceSSHD(device, "vdev-1"); err != nil {
		t.Fatalf("device effective config: %v\n%s", err, device)
	}

	other := effective("vdev-2")
	if err := checkDeviceSSHD(other, "vdev-2"); err != nil {
		t.Fatalf("second device effective config: %v\n%s", err, other)
	}

	admin := effective("admin")
	if strings.Contains(admin, "forcecommand /usr/local/bin/vaire node access-ssh\n") ||
		!strings.Contains(admin, "\nallowusers admin\n") {
		t.Fatalf("admin config affected: %s", admin)
	}

	if err := checkDeviceSSHD(strings.Replace(device, "trustedusercakeys none\n", "trustedusercakeys /etc/ssh/ca\n", 1), "vdev-1"); err == nil {
		t.Fatal("accepted alternate certificate authentication")
	}
}

func TestRenderDeviceHostSSH(t *testing.T) {
	key := deviceHostTestKey(t)

	got, err := renderDeviceHostSSH("admin", "vdev-1", "10.0.0.1/32", key)
	if err != nil {
		t.Fatal(err)
	}

	if got.AuthorizedKey != `restrict,from="10.0.0.1/32",command="/usr/local/bin/vaire node access-ssh" `+key+"\n" {
		t.Fatalf("unexpected key: %q", got.AuthorizedKey)
	}

	if got.Sudoers != "# managed by vaire\nvdev-1 ALL=(root) NOPASSWD: /usr/local/bin/vaire node rpc\n" {
		t.Fatalf("unexpected sudoers: %q", got.Sudoers)
	}
}

func TestRenderDeviceHostSSHRejectsUnsafeInputs(t *testing.T) {
	key := deviceHostTestKey(t)
	for _, login := range []string{"", "root", "vdev-", "vdev-A", "vdev-1\nroot ALL=(ALL) ALL", "vdev-" + strings.Repeat("a", 27)} {
		if _, err := renderDeviceHostSSH("admin", login, "10.0.0.1/32", key); err == nil {
			t.Errorf("accepted login %q", login)
		}
	}

	if _, err := renderDeviceHostSSH("vdev-1", "vdev-1", "10.0.0.1/32", key); err == nil {
		t.Error("accepted legacy admin collision")
	}

	if _, err := renderDeviceHostSSH("admin", "vdev-1", "0.0.0.0/0", key); err == nil {
		t.Error("accepted broad source")
	}

	if _, err := renderDeviceHostSSH("admin", "vdev-1", "10.0.0.1/32", "invalid"); err == nil {
		t.Error("accepted invalid key")
	}
}
