package backup

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/masterbackup"
)

func TestDurableHashIgnoresBackupFacts(t *testing.T) {
	r, _ := decode(nil)
	base := r.durableHash()

	r.RecordBackup("app", Set{Label: "20261015-010000F", Type: TypeFull}, "2026-10-15T01:05:00Z")
	r.LastDump["app"] = Dump{Database: "app", ID: "x"}
	r.Drills = append(r.Drills, Drill{Database: "app"})
	if r.durableHash() != base {
		t.Fatal("a backup fact changed the durable hash: every backup would make a bundle")
	}

	r.DroppedStanzas = append(r.DroppedStanzas, DroppedStanza{Database: "old", Stanza: "old-abc123"})
	afterDrop := r.durableHash()
	if afterDrop == base {
		t.Fatal("a dropped stanza didn't change the durable hash")
	}
	r.Timelines["app"] = TimelineSwitch{Stanza: "app-abc123", Timeline: "2"}
	if r.durableHash() == afterDrop {
		t.Fatal("a timeline switch didn't change the durable hash")
	}
	before := r.durableHash()
	r.AwaitingRecovery["app"] = Awaiting{Stanza: "app-abc123"}
	if r.durableHash() == before || !r.IsAwaiting("app") {
		t.Fatal("awaiting recovery isn't durable")
	}
}

func TestOwnsPrefix(t *testing.T) {
	mine := &masterbackup.Epoch{Server: "prod", InstanceID: "aaa"}
	earlier := &masterbackup.Epoch{Server: "prod", InstanceID: "old"}
	theirs := &masterbackup.Epoch{Server: "staging", InstanceID: "bbb"}
	for name, c := range map[string]struct {
		epoch   *masterbackup.Epoch
		stanzas []string
		owner   PrefixOwner
		want    bool
	}{
		"empty prefix":                         {nil, nil, PrefixOwner{Server: "prod"}, true},
		"our epoch and instance, any stanzas":  {mine, []string{"app-abc123", "old-def456"}, PrefixOwner{Server: "prod", Instance: "aaa"}, true},
		"another server's epoch":               {theirs, nil, PrefixOwner{Server: "prod", Instance: "aaa"}, false},
		"an earlier install's epoch":           {earlier, nil, PrefixOwner{Server: "prod", Instance: "aaa"}, false},
		"a fresh init on an earlier epoch":     {earlier, nil, PrefixOwner{Server: "prod"}, false},
		"a resumed init after its first epoch": {mine, nil, PrefixOwner{Server: "prod", Resumed: true}, true},
		"a resumed init, another server":       {theirs, nil, PrefixOwner{Server: "prod", Resumed: true}, false},
		"no epoch, our stanzas":                {nil, []string{"app-abc123"}, PrefixOwner{Server: "prod", Stanzas: []string{"app-abc123", "other-aaa111"}}, true},
		"no epoch, a foreign stanza":           {nil, []string{"app-abc123", "x-999999"}, PrefixOwner{Server: "prod", Stanzas: []string{"app-abc123"}}, false},
		"no epoch, a resumed init's stanzas":   {nil, []string{"app-abc123"}, PrefixOwner{Server: "prod", Resumed: true}, true},
		"no epoch, stanza by servers":          {nil, []string{"prod"}, PrefixOwner{Server: "prod"}, false},
	} {
		if _, got := OwnsPrefix(c.owner, c.epoch, c.stanzas); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
