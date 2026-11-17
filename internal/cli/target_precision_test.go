package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
)

// Every target-bearing command flag reaches the node as the same UTC
// RFC 3339Nano instant, fractional seconds included.
func TestTargetFlagKeepsWirePrecision(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		flag  string
		value string
		want  string
	}{
		{"restore fraction", "restore", "time", "2026-11-17T18:44:53.983364Z", "2026-11-17T18:44:53.983364Z"},
		{"restore drill fraction", "restore drill", "time", "2026-11-17T18:44:53.983364Z", "2026-11-17T18:44:53.983364Z"},
		{"fork offset", "db fork", "at", "2026-11-17T19:44:53.983364+01:00", "2026-11-17T18:44:53.983364Z"},
		{"recover whole second", "db recover", "time", "2026-11-17T18:44:53Z", "2026-11-17T18:44:53Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := commandAt(t, c.path, "--"+c.flag, c.value)
			gotTime, got, err := targetFlag(cmd, c.flag)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("wire target %q, want %q", got, c.want)
			}
			want, err := time.Parse(time.RFC3339Nano, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if !gotTime.Equal(want) {
				t.Errorf("parsed target %s, want %s", gotTime, want)
			}
		})
	}
}

func TestTargetFlagRefusesInvalidTime(t *testing.T) {
	cmd := commandAt(t, "restore", "--time", "2026-11-17 18:44:53")
	_, _, err := targetFlag(cmd, "time")
	var ce *output.CLIError
	if err == nil || !errors.As(err, &ce) || ce.Code != output.CodeUsage {
		t.Fatalf("invalid target: %v, want usage_error", err)
	}
}

// restoreParams is the wire map restore.start receives, so a fractional
// target must not be cut at the CLI.
func TestRestoreParamsKeepFractionalTarget(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"fraction", "2026-11-17T18:44:53.983364Z", "2026-11-17T18:44:53.983364Z"},
		{"offset", "2026-11-17T19:44:53.983364+01:00", "2026-11-17T18:44:53.983364Z"},
		{"whole second", "2026-11-17T18:44:53Z", "2026-11-17T18:44:53Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := commandAt(t, "restore", "--time", c.value)
			params, target, err := restoreParams(cmd, "app")
			if err != nil {
				t.Fatal(err)
			}
			if params["kind"] != backup.TargetTime || params["time"] != c.want {
				t.Errorf("params %v, want kind %q and time %q", params, backup.TargetTime, c.want)
			}
			want, err := time.Parse(time.RFC3339Nano, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if !target.Equal(want) {
				t.Errorf("target %s, want %s", target, want)
			}
		})
	}
}

func TestRestoreParamsRefuseInvalidTime(t *testing.T) {
	cmd := commandAt(t, "restore", "--time", "2026-11-17 18:44:53")
	if _, _, err := restoreParams(cmd, "app"); err == nil {
		t.Fatal("invalid --time was accepted")
	}
}

// forkParams is the wire map db.fork receives, so --at must not be cut at the
// CLI either.
func TestForkParamsKeepFractionalAt(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"fraction", "2026-11-17T18:44:53.983364Z", "2026-11-17T18:44:53.983364Z"},
		{"offset", "2026-11-17T19:44:53.983364+01:00", "2026-11-17T18:44:53.983364Z"},
		{"whole second", "2026-11-17T18:44:53Z", "2026-11-17T18:44:53Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := commandAt(t, "db fork", "--at", c.value)
			params, at, err := forkParams(cmd, "app", "app_fork")
			if err != nil {
				t.Fatal(err)
			}
			if params["at"] != c.want {
				t.Errorf("params[at] %q, want %q", params["at"], c.want)
			}
			want, err := time.Parse(time.RFC3339Nano, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if !at.Equal(want) {
				t.Errorf("at %s, want %s", at, want)
			}
		})
	}
}

func TestForkParamsRefuseInvalidAt(t *testing.T) {
	cmd := commandAt(t, "db fork", "--at", "2026-11-17 18:44:53")
	if _, _, err := forkParams(cmd, "app", "app_fork"); err == nil {
		t.Fatal("invalid --at was accepted")
	}
}

// recoverTarget is db.recover's wire target and keeps a fraction the same
// way.
func TestRecoverTargetKeepsFraction(t *testing.T) {
	cmd := commandAt(t, "db recover", "--time", "2026-11-17T18:44:53.983364Z")
	at, err := recoverTarget(cmd, []string{"app"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if at != "2026-11-17T18:44:53.983364Z" {
		t.Errorf("recover target %q", at)
	}
}
