package promfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderEscapesAndOrders(t *testing.T) {
	g := Gauge("eru_x", "A help line\nwith a newline.")
	g.Add(1, L("db", `a"b\c`+"\n")...)
	g.Add(2.5, L("db", "z")...)
	empty := Counter("eru_empty", "never rendered")
	c := Counter("eru_y_total", "Counter.")
	c.Add(1234567)
	got := string(Render([]*Family{g, empty, nil, c}))
	want := `# HELP eru_x A help line\nwith a newline.
# TYPE eru_x gauge
eru_x{db="a\"b\\c\n"} 1
eru_x{db="z"} 2.5
# HELP eru_y_total Counter.
# TYPE eru_y_total counter
eru_y_total 1234567
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "eru_empty") {
		t.Error("an empty family was rendered")
	}
}

func TestFormatValue(t *testing.T) {
	for in, want := range map[float64]string{0: "0", 1.7e9: "1700000000", 0.25: "0.25", -3: "-3", 1e20: "1e+20"} {
		if got := formatValue(in); got != want {
			t.Errorf("formatValue(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestWriteIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "textfile")
	g := Gauge("eru_a", "a")
	g.Add(1)
	if err := WriteIn(dir, Host, []*Family{g}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, Host))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", info.Mode())
	}
}
