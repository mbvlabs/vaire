package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestAdminSudoersOnlyAllowsRPC(t *testing.T) {
	want := "# managed by vaire\noperator ALL=(ALL) PASSWD: ALL\noperator ALL=(root) NOPASSWD: /usr/local/bin/vaire node rpc\n"
	if got := adminSudoers("operator"); got != want {
		t.Fatalf("sudoers = %q, want %q", got, want)
	}
}

func TestAdminUserRejectsRootBeforeLoadingOperators(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Server.AdminUser = "root"

	_, err := renderAdminUser(cfg)
	if err == nil || !strings.Contains(err.Error(), "other than root") {
		t.Fatalf("root admin error = %v", err)
	}
}

// All commands in the embedded script are stubbed; this never changes host accounts.
func runAdminUserScript(t *testing.T, uid, groups string, exists bool) (string, string, error) {
	t.Helper()
	dir := t.TempDir()

	stub := `#!/bin/bash
name="${0##*/}"
printf '%s %s\n' "$name" "$*" >> "$LOG"
case "$name" in
  id)
    case "$1" in
      -u) printf '%s\n' "$FAKE_UID";;
      -nG) printf '%s\n' "$FAKE_GROUPS";;
      *) [ "$EXISTS" = yes ] || [ -e "$STATE" ];;
    esac;;
  useradd) : > "$STATE";;
  chpasswd) while IFS= read -r line; do :; done;;
  cut) printf '%s\n' "$FAKE_HOME";;
  mktemp)
    case "$1" in
      */.ssh/*) printf '%s\n' "$FAKE_HOME/keys";;
      *) printf '%s\n' "$FAKE_HOME/sudoers";;
    esac;;
esac
`
	for _, name := range []string{"id", "useradd", "usermod", "chpasswd", "getent", "cut", "install", "mktemp", "chown", "chmod", "mv", "visudo", "rm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	existing := "no"
	if exists {
		existing = "yes"
	}

	cmd := exec.Command("/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(Script("admin-user"))
	cmd.Env = []string{
		"PATH=" + dir, "LOG=" + filepath.Join(dir, "calls"), "STATE=" + filepath.Join(dir, "state"),
		"FAKE_HOME=" + dir, "FAKE_UID=" + uid, "FAKE_GROUPS=" + groups, "EXISTS=" + existing,
		"ADMIN_USER=operator", "AUTHORIZED_KEYS=key", "SUDOERS=rule", "SUDOERS_PATH=/unused",
		"SUDO_PASSWORD=test-password",
	}
	out, err := cmd.CombinedOutput()

	log, readErr := os.ReadFile(filepath.Join(dir, "calls"))
	if readErr != nil {
		t.Fatal(readErr)
	}

	return string(out), string(log), err
}

func TestAdminUserScriptCreatesUnprivilegedPasswordAccount(t *testing.T) {
	for _, exists := range []bool{false, true} {
		out, log, err := runAdminUserScript(t, "1001", "operator", exists)
		if err != nil {
			t.Fatalf("exists=%t: %v: %s", exists, err, out)
		}

		if !strings.Contains(log, "chpasswd ") || !strings.Contains(out, "break-glass SSH password") {
			t.Fatalf("break-glass password not applied: %s\n%s", log, out)
		}

		if strings.Contains(log, "usermod --append") || strings.Contains(log, "usermod --groups") {
			t.Fatalf("added privileged group: %s", log)
		}
	}
}

func TestAdminUserScriptRefusesPrivilegedAccounts(t *testing.T) {
	for _, tc := range []struct{ uid, groups string }{
		{"0", "operator"}, {"1001", "operator sudo"}, {"1001", "operator wheel"},
		{"1001", "operator admin"}, {"1001", "operator root"},
		{"1001", "operator docker"}, {"1001", "operator lxd"}, {"1001", "operator incus"},
		{"1001", "operator disk"}, {"1001", "operator libvirt"},
	} {
		out, log, err := runAdminUserScript(t, tc.uid, tc.groups, true)
		if err == nil || !strings.Contains(out, "must not") {
			t.Fatalf("uid=%s groups=%s: %v %s", tc.uid, tc.groups, err, out)
		}

		if strings.Contains(log, "usermod ") || strings.Contains(log, "chpasswd ") {
			t.Fatalf("privileged account modified: %s", log)
		}
	}
}

func TestAdminUserScriptChecksNewAccountGroups(t *testing.T) {
	out, log, err := runAdminUserScript(t, "1001", "operator sudo", false)
	if err == nil || !strings.Contains(out, "privileged group sudo") || strings.Contains(log, "chpasswd ") {
		t.Fatalf("new account with privileged defaults: %v\n%s\n%s", err, out, log)
	}
}
