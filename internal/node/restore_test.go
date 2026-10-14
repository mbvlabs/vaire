package node

import (
	"errors"
	"testing"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
)

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error %v is not a CLIError", err)
	}
	if cliErr.Code != output.CodePrecondition {
		t.Fatalf("code = %s, want %s", cliErr.Code, output.CodePrecondition)
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason
}

func records(leftovers []backup.Leftover, undoing map[string]backup.Undo) backup.Records {
	if undoing == nil {
		undoing = map[string]backup.Undo{}
	}
	return backup.Records{Leftovers: leftovers, Undoing: undoing}
}

var (
	promoted  = backup.Leftover{Database: "app2", Name: "app2__pre_restore_20261014T100000", RestoreID: "r1", Port: 6001}
	undone    = backup.Leftover{Database: "app2", Name: "app2__pre_restore_20261014T110000", RestoreID: undoRestoreID("r1"), Port: 6001}
	promoted2 = backup.Leftover{Database: "app2", Name: "app2__pre_restore_20261014T120000", RestoreID: "r2", Port: 6002}
)

func TestPlanUndoAfterPromote(t *testing.T) {
	u, resuming, err := planUndo(records([]backup.Leftover{promoted}, nil), "app2")
	if err != nil {
		t.Fatal(err)
	}
	if resuming || u.From.Name != promoted.Name {
		t.Fatalf("got resuming=%v from=%s, want a fresh undo from %s", resuming, u.From.Name, promoted.Name)
	}
}

func TestPlanUndoRefusesCompletedUndo(t *testing.T) {
	// A completed undo removes the promote's leftover and records the
	// restored data as an undo leftover; a second undo must not swap it in.
	_, _, err := planUndo(records([]backup.Leftover{undone}, nil), "app2")
	if err == nil {
		t.Fatal("a second undo was allowed")
	}
	if r := reasonOf(t, err); r != "already_undone" {
		t.Fatalf("reason = %s, want already_undone", r)
	}
	// An older promote's leftover beside it isn't swapped in either.
	older := backup.Leftover{Database: "app2", Name: "app2__pre_restore_20261014T090000", RestoreID: "r0"}
	if _, _, err := planUndo(records([]backup.Leftover{older, undone}, nil), "app2"); err == nil || reasonOf(t, err) != "already_undone" {
		t.Fatalf("err = %v, want already_undone", err)
	}
}

func TestPlanUndoResumesInterruptedUndo(t *testing.T) {
	// An interrupted undo keeps its Undoing record; the leftovers may be
	// in any state, even look like a completed undo's.
	rec := backup.Undo{From: promoted, Aside: "app2__pre_restore_20261014T110000", StartedAt: "2026-10-14T11:00:00Z"}
	for _, leftovers := range [][]backup.Leftover{{promoted}, nil, {undone}} {
		u, resuming, err := planUndo(records(leftovers, map[string]backup.Undo{"app2": rec}), "app2")
		if err != nil {
			t.Fatalf("leftovers %v: %v", leftovers, err)
		}
		if !resuming || u.From.Name != promoted.Name || u.Aside != rec.Aside {
			t.Fatalf("leftovers %v: got resuming=%v from=%s aside=%s, want the recorded undo", leftovers, resuming, u.From.Name, u.Aside)
		}
	}
}

func TestPlanUndoAfterUndoThenNewPromote(t *testing.T) {
	u, resuming, err := planUndo(records([]backup.Leftover{undone, promoted2}, nil), "app2")
	if err != nil {
		t.Fatal(err)
	}
	if resuming || u.From.Name != promoted2.Name {
		t.Fatalf("got from=%s, want %s", u.From.Name, promoted2.Name)
	}
}

func TestPlanUndoNoLeftover(t *testing.T) {
	other := backup.Leftover{Database: "app", Name: "app__pre_restore_20261014T100000", RestoreID: "r9"}
	if _, _, err := planUndo(records([]backup.Leftover{other}, nil), "app2"); err == nil || reasonOf(t, err) != "no_leftover" {
		t.Fatalf("err = %v, want no_leftover", err)
	}
}

func TestNoOpenRestore(t *testing.T) {
	cases := []struct {
		name string
		bs   backup.Records
		want string
	}{
		{"nothing", records(nil, nil), "no_open_restore"},
		{"promoted", records([]backup.Leftover{promoted}, nil), "restore_promoted"},
		{"undone", records([]backup.Leftover{undone}, nil), "restore_undone"},
		{"undo stopped part way", records([]backup.Leftover{promoted}, map[string]backup.Undo{"app2": {From: promoted, Aside: "x"}}), "undo_in_progress"},
	}
	for _, c := range cases {
		if got := reasonOf(t, noOpenRestore(c.bs, "app2")); got != c.want {
			t.Errorf("%s: reason = %s, want %s", c.name, got, c.want)
		}
	}
}
