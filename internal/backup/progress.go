package backup

import (
	"regexp"
	"strconv"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
)

// detailFile matches pgBackRest's detail log line for a copied file, for
// example "P01 DETAIL: backup file /var/lib/postgresql/18/main/base/1/1259
// (128KB, 42.17%) checksum ...". The percentage is of the whole backup or
// restore.
var detailFile = regexp.MustCompile(`(?:backup|restore) file .* \(([^,()]+), ([0-9]+(?:\.[0-9]+)?)%\)`)

// progress turns pgBackRest detail lines into progress events, one per whole
// percent so a large backup doesn't flood the stream.
type progress struct {
	phase  string
	report engine.Reporter
	last   int
}

func newProgress(phase string, report engine.Reporter) *progress {
	return &progress{phase: phase, report: report, last: -1}
}

// line inspects one output line.
func (p *progress) line(line string) {
	m := detailFile.FindStringSubmatch(line)
	if m == nil || p.report == nil {
		return
	}
	f, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return
	}
	pct := int(f)
	if pct <= p.last {
		return
	}
	p.last = pct
	p.report(output.Event{Type: "progress", Phase: p.phase, Pct: &pct, Detail: m[2] + "%"})
}
