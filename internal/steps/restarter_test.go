package steps

import (
	"context"
	"strings"
	"testing"
)

// The restart goes through the cluster's systemd unit, never pg_ctlcluster,
// which picks systemctl or pg_ctl by its environment.
func TestRestartCommandUsesTheUnit(t *testing.T) {
	name, args := restartCommand("other")
	if got := name + " " + strings.Join(args, " "); got != "systemctl restart postgresql@18-other.service" {
		t.Errorf("restart command %q", got)
	}
}

// A failing command's error carries the end of its output, so a failed
// restart says why.
func TestRunCmdKeepsTheOutputTail(t *testing.T) {
	err := runCmd(context.Background(), "sh", "-c", "echo starting; echo 'Job for postgresql@18-other.service failed' >&2; exit 1")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit status 1") || !strings.Contains(msg, "Job for postgresql@18-other.service failed") || !strings.Contains(msg, "starting") {
		t.Errorf("error %q", msg)
	}
	if err := runCmd(context.Background(), "sh", "-c", "echo fine"); err != nil {
		t.Errorf("a passing command failed: %v", err)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{max: 8}
	_, _ = b.Write([]byte("0123456789"))
	_, _ = b.Write([]byte("ab"))
	if got := b.String(); got != "…456789ab" {
		t.Errorf("tail %q", got)
	}
	short := &tailBuffer{max: 8}
	_, _ = short.Write([]byte("abc"))
	if got := short.String(); got != "abc" {
		t.Errorf("short %q", got)
	}
}
