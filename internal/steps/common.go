package steps

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eru/internal/engine"
)

//go:embed scripts/*.sh
var scriptFS embed.FS

// Script returns the embedded script called name, without the .sh suffix.
func Script(name string) string {
	raw, err := scriptFS.ReadFile("scripts/" + name + ".sh")
	if err != nil {
		panic("missing embedded script " + name)
	}
	return string(raw)
}

// probeEnv is the fixed environment for read-only probes, matching scripts.
var probeEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"LANG=C.UTF-8",
	"LC_ALL=C.UTF-8",
	"HOME=/root",
}

const probeLimit = 30 * time.Second

// Probe runs a read-only command for Check and returns its trimmed stdout.
// Probes never mutate the host, so they also run in dry runs.
func Probe(ctx context.Context, name string, args ...string) (string, error) {
	return ProbeWithEnv(ctx, nil, name, args...)
}

// ProbeWithEnv is Probe with extra environment variables appended to the fixed
// probe environment.
func ProbeWithEnv(ctx context.Context, extraEnv []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed probe commands from the step catalogue.
	cmd.Env = append(append([]string(nil), probeEnv...), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s: %s", name, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// canonical marshals v as the step's Inputs bytes.
func canonical(v any) ([]byte, error) {
	return json.Marshal(v)
}

// fileIs reports whether path holds exactly content with mode perm.
func fileIs(path, content string, perm os.FileMode) bool {
	return fileIsOwned(path, content, perm, "", "")
}

// fileIsOwned is fileIs that also checks the owner and group, so a file a
// service cannot read counts as drift. An empty owner or group is not checked.
func fileIsOwned(path, content string, perm os.FileMode, owner, group string) bool {
	if !ownedAs(path, perm, owner, group) {
		return false
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: managed file paths from the step catalogue.
	return err == nil && string(raw) == content
}

// ownedAs reports whether path exists with mode perm and the named owner
// and group, whatever its content. An empty owner or group is not checked.
func ownedAs(path string, perm os.FileMode, owner, group string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != perm {
		return false
	}
	if owner != "" || group != "" {
		uid, gid, err := engine.ResolveIDs(owner, group)
		if err != nil {
			return false
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (uid != -1 && int(st.Uid) != uid) || (gid != -1 && int(st.Gid) != gid) {
			return false
		}
	}
	return true
}

// writeFile writes a managed file atomically, creating its directory.
func writeFile(path, content string, perm os.FileMode) error {
	return writeFileOwned(path, content, perm, "", "")
}

// writeFileOwned writes a managed file atomically with an owner and group,
// creating its directory. The owner is set on the temp file before the rename,
// so a service that reads the file never sees it with the wrong owner. Use it
// for every file a service user has to read.
func writeFileOwned(path, content string, perm os.FileMode, owner, group string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: system config directories are 0755.
		return err
	}
	return engine.WriteFileAtomicOwned(path, []byte(content), perm, owner, group)
}

// serviceActive reports whether a systemd unit is active.
func serviceActive(ctx context.Context, unit string) bool {
	out, _ := Probe(ctx, "systemctl", "is-active", unit)
	return out == "active"
}

// incomplete is a Check result naming what differs.
func incomplete(detail string) engine.CheckResult {
	return engine.CheckResult{Complete: false, Detail: detail}
}

func complete() engine.CheckResult {
	return engine.CheckResult{Complete: true}
}

// ErrNoConfig is returned by Inputs when cluster.toml has no primary node.
var ErrNoConfig = errors.New("cluster.toml has no primary node")
