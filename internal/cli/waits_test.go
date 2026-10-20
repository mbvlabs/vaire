package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestWaitStopTwice(t *testing.T) {
	p := &progress{opts: output.Options{Mode: output.ModeHuman}, stderr: &syncBuffer{}}
	stop := p.wait("connecting to db1")
	stop()
	stop()
	if p.beat != nil {
		t.Fatal("the heartbeat survived the stop")
	}
}

func TestWaitSilentWhenStructured(t *testing.T) {
	for _, opts := range []output.Options{{Mode: output.ModeJSON}, {Mode: output.ModeHuman, Stream: true}, {Mode: output.ModeHuman, Quiet: true}} {
		p := &progress{opts: opts, stderr: &syncBuffer{}}
		stop := p.wait("downloading usql")
		if p.beat != nil || p.status != nil {
			t.Errorf("%+v: a wait showed", opts)
		}
		stop()
	}
}

func TestDumpListMessage(t *testing.T) {
	if got := dumpListMessage(nil); got != "no dumps in the bucket" {
		t.Errorf("empty = %q", got)
	}
	lines := strings.Split(dumpListMessage([]backup.Dump{{Database: "app", ID: "20260701T030000Z", Size: 2048, Created: "2026-07-01T03:00:00Z"}}), "\n")
	if len(lines) != 2 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "DATABASE") || !strings.Contains(lines[1], "20260701T030000Z") {
		t.Errorf("lines = %q", lines)
	}
}

func TestDrillLinesRecoveredToOnce(t *testing.T) {
	drill := backup.Drill{Database: "app", Result: "pass", BackupLabel: "F1", RecoveredTo: "2026-10-14T10:00:00Z",
		Checks: []backup.DrillCheck{{Name: "restore", OK: true, Detail: "recovered to 2026-10-14T10:00:00Z"}, {Name: "tables", OK: true, Detail: "1 table"}}}
	if got := strings.Count(drillLines(drill), "recovered to"); got != 1 {
		t.Errorf("recovered to printed %d times:\n%s", got, drillLines(drill))
	}
	drill.Checks = nil
	if !strings.Contains(drillLines(drill), "recovered to") {
		t.Errorf("without a restore check the header must say where it recovered to:\n%s", drillLines(drill))
	}
}

func TestApplyNextOnlyWhenStepsApply(t *testing.T) {
	if next := applyNext(engine.PlanData{Total: 20}); next != nil {
		t.Errorf("0 steps suggested %v", next)
	}
	if got := pendingSteps(engine.PlanData{Total: 20}); got != "nothing to apply" {
		t.Errorf("pendingSteps = %q", got)
	}
	if next := applyNext(engine.PlanData{WouldApply: 2, Total: 20}); len(next) != 1 || next[0] != "vaire apply" {
		t.Errorf("2 steps suggested %v", next)
	}
}

func TestLiftNotice(t *testing.T) {
	if got := liftNotice("app2"); !strings.HasPrefix(got, "Lifting the protection of app2 needs a code") {
		t.Errorf("liftNotice = %q", got)
	}
}
