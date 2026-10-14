package backup

import "testing"

func TestVerifyOf(t *testing.T) {
	rs := Records{LastVerify: map[string]VerifyResult{
		"app":    {OK: true, Stanza: "app-82446f"},
		"legacy": {OK: true},
	}}
	if _, ok := rs.VerifyOf("app", "app-82446f"); !ok {
		t.Error("the verify of the current stanza is missing")
	}
	if _, ok := rs.VerifyOf("app", "app-c273db"); ok {
		t.Error("a new stanza shows the old stanza's verify")
	}
	if _, ok := rs.VerifyOf("legacy", "legacy-1"); ok {
		t.Error("a verify without a stanza counts")
	}
}

func TestDrillOf(t *testing.T) {
	rs := Records{Drills: []Drill{
		{ID: "1", Database: "app", Stanza: "app-82446f", Result: DrillPass},
		{ID: "2", Database: "app", Stanza: "app-c273db", Result: DrillFail},
		{ID: "3", Database: "legacy", Result: DrillPass},
	}}
	if d, ok := rs.DrillOf("app", "app-82446f"); !ok || d.ID != "1" {
		t.Errorf("the drill of the old stanza: %+v, %v", d, ok)
	}
	if d, ok := rs.DrillOf("app", "app-c273db"); !ok || d.ID != "2" {
		t.Errorf("the drill of the current stanza: %+v, %v", d, ok)
	}
	if _, ok := rs.DrillOf("app", "app-new"); ok {
		t.Error("a new stanza shows another stanza's drill")
	}
	if _, ok := rs.DrillOf("legacy", "legacy-1"); ok {
		t.Error("a drill without a stanza counts")
	}
}
