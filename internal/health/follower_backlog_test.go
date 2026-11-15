package health

import "testing"

func TestFollowerSeparateBacklogs(t *testing.T) {
	src := map[string]string{"source_lsn": "0/100"}
	self := map[string]string{"in_recovery": "true", "receive_lsn": "0/F0", "replay_lsn": "0/80"}
	f := ParseFollower("app", "slot", src, self)
	if f.ReceiveBacklogBytes == nil || *f.ReceiveBacklogBytes != 16 || f.ReplayBacklogBytes == nil || *f.ReplayBacklogBytes != 128 {
		t.Fatalf("%+v", f)
	}
	self["receive_lsn"] = ""
	self["replay_lsn"] = "bad"
	f = ParseFollower("app", "slot", src, self)
	if f.ReceiveBacklogBytes != nil || f.ReplayBacklogBytes != nil {
		t.Fatalf("%+v", f)
	}
	if f = ParseFollower("app", "slot", nil, self); f.ReceiveBacklogBytes != nil || f.ReplayBacklogBytes != nil {
		t.Fatal(f)
	}
	self["receive_lsn"], self["replay_lsn"] = "0/100", "0/100"
	f = ParseFollower("app", "slot", src, self)
	if f.ReceiveBacklogBytes == nil || *f.ReceiveBacklogBytes != 0 || f.ReplayBacklogBytes == nil || *f.ReplayBacklogBytes != 0 {
		t.Fatal(f)
	}
}
