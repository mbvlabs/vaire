package backup

import "testing"

func TestArchiveStatusUpToDate(t *testing.T) {
	zero, two := int64(0), int64(2)
	for name, c := range map[string]struct {
		st   ArchiveStatus
		want bool
	}{
		"idle":            {ArchiveStatus{LastArchivedWAL: "000000010000000000000005", LastArchivedTime: "2026-10-14T10:00:00Z", PendingWAL: &zero}, true},
		"waiting":         {ArchiveStatus{LastArchivedWAL: "000000010000000000000005", LastArchivedTime: "2026-10-14T10:00:00Z", PendingWAL: &two}, false},
		"pending unknown": {ArchiveStatus{LastArchivedWAL: "000000010000000000000005", LastArchivedTime: "2026-10-14T10:00:00Z"}, false},
		"never archived":  {ArchiveStatus{PendingWAL: &zero}, false},
		"failing":         {ArchiveStatus{LastArchivedWAL: "x", LastArchivedTime: "2026-10-14T10:00:00Z", FailedCount: 1, LastFailedTime: "2026-10-14T10:05:00Z", PendingWAL: &zero}, false},
		"recovered":       {ArchiveStatus{LastArchivedWAL: "x", LastArchivedTime: "2026-10-14T10:05:00Z", FailedCount: 1, LastFailedTime: "2026-10-14T10:00:00Z", PendingWAL: &zero}, true},
	} {
		if got := c.st.UpToDate(); got != c.want {
			t.Errorf("%s: UpToDate = %v, want %v", name, got, c.want)
		}
	}
}
