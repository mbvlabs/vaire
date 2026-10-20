// Package promfile writes vaire's node_exporter textfiles (Phase 4). Each
// owner writes its own file in Dir, so no writer needs another's data or a
// shared lock: vaire.prom (backups), vaire-host.prom (cgroups, TLS, budget),
// vaire-doctor.prom and vaire-maint.prom. Phase 5 adds vaire-master-backup.prom.
package promfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/engine"
)

// Dir is node_exporter's textfile collector directory.
const Dir = "/var/lib/node_exporter/textfile"

// The textfiles, by owner.
const (
	Backup = "vaire.prom"
	Host   = "vaire-host.prom"
	Doctor = "vaire-doctor.prom"
	Maint  = "vaire-maint.prom"
)

// Label is one name="value" pair. Labels keep their order.
type Label struct{ Name, Value string }

// L builds labels from name/value pairs: L("db", "app", "type", "full").
func L(pairs ...string) []Label {
	out := make([]Label, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Label{pairs[i], pairs[i+1]})
	}
	return out
}

// Sample is one line of a family.
type Sample struct {
	Labels []Label
	Value  float64
}

// Family is one metric with its HELP, TYPE and samples.
type Family struct {
	Name    string
	Help    string
	Type    string // gauge or counter
	Samples []Sample
}

// Add appends a sample.
func (f *Family) Add(value float64, labels ...Label) {
	f.Samples = append(f.Samples, Sample{Labels: labels, Value: value})
}

// Gauge returns an empty gauge family.
func Gauge(name, help string) *Family { return &Family{Name: name, Help: help, Type: "gauge"} }

// Counter returns an empty counter family.
func Counter(name, help string) *Family { return &Family{Name: name, Help: help, Type: "counter"} }

// Render renders families in the text exposition format. A family with no
// samples is left out; samples keep their order, and HELP and TYPE appear
// once per family.
func Render(families []*Family) []byte {
	var b strings.Builder
	for _, f := range families {
		if f == nil || len(f.Samples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n", f.Name, escapeHelp(f.Help))
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.Name, f.Type)
		for _, s := range f.Samples {
			b.WriteString(f.Name)
			if len(s.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range s.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(l.Name)
					b.WriteString(`="`)
					b.WriteString(escapeLabel(l.Value))
					b.WriteByte('"')
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(formatValue(s.Value))
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// formatValue prints integers without an exponent and everything else with
// Go's shortest representation.
func formatValue(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}

func escapeHelp(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(v)
}

// Write renders families into Dir/name, atomically, mode 0644 (node_exporter
// reads it as its own user).
func Write(name string, families []*Family) error {
	return WriteIn(Dir, name, families)
}

// WriteIn is Write into dir.
func WriteIn(dir, name string, families []*Family) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: node_exporter reads this directory.
		return err
	}
	return engine.WriteFileAtomic(filepath.Join(dir, name), Render(families), 0o644)
}

// SortedKeys returns m's keys in order, for deterministic output.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
