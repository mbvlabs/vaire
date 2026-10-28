package diagnostics

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
)

func validHealth() activity.Health {
	return activity.Health{
		Node:            "db1",
		Dir:             activity.DefaultDir,
		Exists:          true,
		IsDir:           true,
		DirMode:         "0700",
		Readable:        true,
		Writable:        true,
		SegmentMode:     "0600",
		OwnerOnly:       true,
		MaxBytes:        32 << 20,
		UsedBytes:       1024,
		SegmentBytes:    1 << 20,
		Segments:        1,
		RetentionS:      14 * 24 * 60 * 60,
		FirstRetainedAt: "2026-10-26T09:00:00Z",
		LastRetainedAt:  "2026-10-26T10:00:00Z",
		LastWriteAt:     "2026-10-26T10:00:00Z",
	}
}

func TestValidateHealth(t *testing.T) {
	if err := ValidateHealth(validHealth()); err != nil {
		t.Fatalf("valid health: %v", err)
	}
	cases := map[string]func(*activity.Health){
		"node injection":    func(h *activity.Health) { h.Node = "db1; DROP TABLE users" },
		"unexpected dir":    func(h *activity.Health) { h.Dir = "/tmp/activity" },
		"bad dir mode":      func(h *activity.Health) { h.DirMode = "0755" },
		"bad segment mode":  func(h *activity.Health) { h.SegmentMode = "600" },
		"offset time":       func(h *activity.Health) { h.LastWriteAt = "2026-10-26T12:00:00+02:00" },
		"retained inverted": func(h *activity.Health) { h.FirstRetainedAt = "2026-10-26T11:00:00Z" },
		"negative bytes":    func(h *activity.Health) { h.UsedBytes = -1 },
		"negative segments": func(h *activity.Health) { h.Segments = -1 },
		"too many bad segments": func(h *activity.Health) {
			h.NonOwnerOnlySegments = 2
		},
		"negative malformed": func(h *activity.Health) { h.MalformedEvents = -1 },
		"loss flag missing": func(h *activity.Health) {
			h.MalformedEvents = 1
		},
		"over budget mismatch": func(h *activity.Health) { h.UsedBytes = h.MaxBytes + 1 },
		"owner only with bad segments": func(h *activity.Health) {
			h.NonOwnerOnlySegments = 1
		},
		"owner only without store": func(h *activity.Health) {
			h.Exists = false
		},
		"dir mode not owner only": func(h *activity.Health) {
			h.DirMode = "0750"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			h := validHealth()
			edit(&h)
			err := ValidateHealth(h)
			if codeOf(t, err) != output.CodeValidation {
				t.Fatalf("err = %v", err)
			}
			if name == "node injection" && strings.Contains(err.Error(), "DROP TABLE") {
				t.Fatalf("error echoed the value: %v", err)
			}
		})
	}
}
