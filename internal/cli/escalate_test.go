package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/release"
	"github.com/mbvlabs/vaire/internal/upgrade"
)

// fakeSudo puts a sudo on PATH that succeeds without running the child, so
// Escalate reports a handled child with no real sudo and no root.
func fakeSudo(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("running as root: Escalate does not escalate")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// unwritableDir is a directory this process cannot write to, so a binary in
// it can only be replaced by a sudo child.
func unwritableDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	return dir
}

// An upgrade child re-executed under sudo renders its own result. The parent
// must stop after it, with the child's error, instead of rendering a second
// envelope that lacks the child's previous path.
func TestApplyUpgradeStopsAfterEscalation(t *testing.T) {
	fakeSudo(t)
	cmd := commandAt(t, "upgrade")
	cmd.SetContext(context.Background())

	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	manifest := release.Manifest{Version: "1.1.0", Channel: "stable"}
	plan := upgrade.Plan{
		From:    upgrade.Version{Version: "1.0.0", Channel: "stable"},
		To:      upgrade.Version{Version: "1.1.0", Channel: "stable"},
		Target:  filepath.Join(unwritableDir(t), "vaire"),
		Changed: true,
	}
	data := upgradeData{From: plan.From, To: plan.To, Path: plan.Target, Changed: true}

	handled, err := escalateUpgrade(cmd, manifest, plan.Target)
	if !handled || err != nil {
		t.Fatalf("escalateUpgrade = %v, %v; want a handled child and no error", handled, err)
	}

	if err := applyUpgrade(cmd, upgradeOptions{yes: true}, manifest, plan, data, nil); err != nil {
		t.Fatalf("the parent continued after the child: %v", err)
	}

	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("the parent rendered after the child: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
