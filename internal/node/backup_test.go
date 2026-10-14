package node

import (
	"testing"

	"github.com/mbvlabs/eru/internal/backup"
)

func TestRestoreWindow(t *testing.T) {
	one := backup.Info{Sets: []backup.Set{{Label: "a", Stop: "2026-10-14T10:21:20Z"}}}
	two := backup.Info{Sets: []backup.Set{{Label: "a", Stop: "2026-10-14T08:00:00Z"}, {Label: "b", Stop: "2026-10-14T08:20:14Z"}}}
	cases := []struct {
		name     string
		info     backup.Info
		archived string
		from, to string
	}{
		{"archive time ends the window", two, "2026-10-14T08:35:15Z", "2026-10-14T08:00:01Z", "2026-10-14T08:35:15Z"},
		{"no archive time: the newest set's end", two, "", "2026-10-14T08:00:01Z", "2026-10-14T08:20:14Z"},
		{"archive older than the newest set", two, "2026-10-14T08:10:00Z", "2026-10-14T08:00:01Z", "2026-10-14T08:20:14Z"},
		{"one set, no archive time: never ends before it starts", one, "", "2026-10-14T10:21:21Z", "2026-10-14T10:21:21Z"},
		{"no sets", backup.Info{}, "", "", ""},
	}
	for _, c := range cases {
		w := restoreWindow(c.info, backup.ArchiveStatus{LastArchivedTime: c.archived})
		if w.From != c.from || w.To != c.to {
			t.Errorf("%s: window %s to %s, want %s to %s", c.name, w.From, w.To, c.from, c.to)
		}
	}
}
