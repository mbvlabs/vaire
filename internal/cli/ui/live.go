package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// tick is how often the live area redraws.
const tick = 100 * time.Millisecond

// Live draws running tasks as spinners at the bottom of the terminal and
// prints finished tasks, headers and panels above them. It never reads from
// the terminal, so Ctrl+C and prompts keep working; prompts call Suspend.
type Live struct {
	mu       sync.Mutex
	out      io.Writer
	fd       int
	tasks    []*Task
	drawn    int
	stop     chan struct{}
	running  bool
	paused   int
	spinner  spinner.Spinner
	bar      progress.Model
	lastHead string
}

// Task is one running phase or step.
type Task struct {
	l        *Live
	key      string
	title    string
	detail   string
	lines    []string
	pct      float64
	hasPct   bool
	pctLabel string
	started  time.Time
	deadline time.Time
}

var (
	activeMu sync.Mutex
	active   *Live
)

// Enabled reports whether f is a terminal that can show the live UI.
func Enabled(f *os.File) bool {
	if f == nil || !term.IsTerminal(int(f.Fd())) { //nolint:gosec // G115: file descriptors fit in int.
		return false
	}
	return os.Getenv("TERM") != "dumb"
}

// Writer wraps w so colors follow the terminal's color profile and
// NO_COLOR.
func Writer(w io.Writer) io.Writer {
	return colorprofile.NewWriter(w, os.Environ())
}

// Start returns the process-wide live area on f, creating it on first use.
func Start(f *os.File) *Live {
	activeMu.Lock()
	defer activeMu.Unlock()
	if active == nil {
		active = &Live{
			out:     Writer(f),
			fd:      int(f.Fd()), //nolint:gosec // G115: file descriptors fit in int.
			spinner: spinner.MiniDot,
			bar: progress.New(
				progress.WithWidth(28),
				progress.WithColors(colorAccent, colorInfo),
				progress.WithoutPercentage(),
			),
		}
	}
	return active
}

// Suspend clears the live area and stops redrawing until the returned func
// is called. It is safe to call when no live area exists.
func Suspend() func() {
	activeMu.Lock()
	l := active
	activeMu.Unlock()
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	l.paused++
	l.clear()
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.paused--
		l.draw()
		l.mu.Unlock()
	}
}

// Finish ends the live area at the end of a command. With failed, tasks
// still running are shown as failed; otherwise they are dropped.
func Finish(failed bool) {
	activeMu.Lock()
	l := active
	activeMu.Unlock()
	if l == nil {
		return
	}
	if failed {
		l.mu.Lock()
		running := append([]*Task(nil), l.tasks...)
		l.mu.Unlock()
		for i := len(running) - 1; i >= 0; i-- {
			running[i].End(EndFail, "")
		}
	}
	Close()
}

// Close clears the live area and stops it. Running tasks are dropped.
func Close() {
	activeMu.Lock()
	l := active
	active = nil
	activeMu.Unlock()
	if l == nil {
		return
	}
	l.mu.Lock()
	l.tasks = nil
	l.clear()
	l.stopTicker()
	l.mu.Unlock()
}

// Width is the terminal width, or 80.
func (l *Live) Width() int {
	if w, _, err := term.GetSize(l.fd); err == nil && w > 0 {
		return w
	}
	return 80
}

// Println prints s above the live area.
func (l *Live) Println(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	_, _ = fmt.Fprintln(l.out, s)
	l.draw()
}

// Header prints a section header once; repeating the same header is a no-op.
func (l *Live) Header(title string) {
	l.mu.Lock()
	if l.lastHead == title {
		l.mu.Unlock()
		return
	}
	l.lastHead = title
	l.mu.Unlock()
	l.Println("\n" + Header(title, l.Width()))
}

// Start begins a task, or returns the running task with the same key.
func (l *Live) Start(key, title string) *Task {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.tasks {
		if t.key == key {
			t.title = title
			return t
		}
	}
	t := &Task{l: l, key: key, title: title, started: time.Now()}
	l.tasks = append(l.tasks, t)
	l.startTicker()
	l.draw()
	return t
}

// Task returns the running task for key, or the newest one when key is not
// running. It returns nil when nothing runs.
func (l *Live) Task(key string) *Task {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.tasks {
		if t.key == key {
			return t
		}
	}
	if len(l.tasks) > 0 {
		return l.tasks[len(l.tasks)-1]
	}
	return nil
}

// Running reports whether key is a running task.
func (l *Live) Running(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.tasks {
		if t.key == key {
			return true
		}
	}
	return false
}

// Detail sets the dim line under the task title.
func (t *Task) Detail(s string) {
	t.l.mu.Lock()
	t.detail = sanitize(s)
	t.l.mu.Unlock()
}

// Title replaces the task title.
func (t *Task) Title(s string) {
	t.l.mu.Lock()
	t.title = s
	t.l.mu.Unlock()
}

// Lines sets status lines shown under the task, such as resolver answers.
func (t *Task) Lines(lines []string) {
	t.l.mu.Lock()
	t.lines = append([]string(nil), lines...)
	t.l.mu.Unlock()
}

// Progress shows a progress bar under the task.
func (t *Task) Progress(pct float64, label string) {
	t.l.mu.Lock()
	t.pct, t.hasPct, t.pctLabel = min(max(pct, 0), 1), true, label
	t.l.mu.Unlock()
}

// Deadline shows a countdown to at next to the elapsed time.
func (t *Task) Deadline(at time.Time) {
	t.l.mu.Lock()
	t.deadline = at
	t.l.mu.Unlock()
}

// End states for a task.
const (
	EndOK   = "ok"
	EndFail = "fail"
	EndWarn = "warn"
	EndSkip = "skip"
)

// End stops the task and prints its final line.
func (t *Task) End(state, detail string) {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, other := range l.tasks {
		if other == t {
			l.tasks = append(l.tasks[:i], l.tasks[i+1:]...)
			break
		}
	}
	l.clear()
	_, _ = fmt.Fprintln(l.out, l.truncate(finalLine(state, t.title, detail, time.Since(t.started))))
	if len(l.tasks) == 0 {
		l.stopTicker()
	}
	l.draw()
}

// Line prints a finished line for something that never ran as a task, such
// as a skipped step.
func (l *Live) Line(state, title, detail string) {
	l.Println(l.truncate(finalLine(state, title, detail, 0)))
}

func finalLine(state, title, detail string, took time.Duration) string {
	var sym string
	switch state {
	case EndOK:
		sym = styleOK.Render(SymOK)
	case EndFail:
		sym = styleFail.Render(SymFail)
	case EndWarn:
		sym = styleWarn.Render(SymWarn)
	default:
		sym = styleDim.Render(SymSkip)
		return sym + " " + styleDim.Render(strings.TrimSpace(title+"  "+detail))
	}
	line := sym + " " + title
	if detail != "" {
		line += "  " + styleDim.Render(sanitize(detail))
	}
	if took >= time.Second {
		line += "  " + styleDim.Render(formatDuration(took))
	}
	return line
}

func (l *Live) startTicker() {
	if l.running {
		return
	}
	l.running = true
	l.stop = make(chan struct{})
	go func(stop chan struct{}) {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				l.mu.Lock()
				l.draw()
				l.mu.Unlock()
			}
		}
	}(l.stop)
}

func (l *Live) stopTicker() {
	if l.running {
		close(l.stop)
		l.running = false
	}
}

// clear erases the drawn live area. The caller holds l.mu.
func (l *Live) clear() {
	if l.drawn == 0 {
		return
	}
	seq := "\r"
	if l.drawn > 1 {
		seq += ansi.CursorUp(l.drawn - 1)
	}
	_, _ = io.WriteString(l.out, seq+ansi.EraseScreenBelow)
	l.drawn = 0
}

// draw redraws the live area. The caller holds l.mu.
func (l *Live) draw() {
	l.clear()
	if l.paused > 0 || len(l.tasks) == 0 {
		return
	}
	frames := l.spinner.Frames
	frame := frames[int(time.Now().UnixMilli()/int64(l.spinner.FPS/time.Millisecond))%len(frames)]
	var lines []string
	for _, t := range l.tasks {
		elapsed := formatDuration(time.Since(t.started))
		if !t.deadline.IsZero() {
			left := max(time.Until(t.deadline), 0)
			elapsed += ", " + formatDuration(left) + " left"
		}
		lines = append(lines, styleSpinner.Render(frame)+" "+t.title+"  "+styleDim.Render(elapsed))
		for _, s := range t.lines {
			lines = append(lines, "  "+s)
		}
		if t.hasPct {
			lines = append(lines, "  "+l.bar.ViewAs(t.pct)+" "+styleDim.Render(t.pctLabel))
		}
		if t.detail != "" {
			lines = append(lines, styleDim.Render("  └ "+t.detail))
		}
	}
	for i := range lines {
		lines[i] = l.truncate(lines[i])
	}
	_, _ = io.WriteString(l.out, strings.Join(lines, "\n"))
	l.drawn = len(lines)
}

func (l *Live) truncate(s string) string {
	return ansi.Truncate(s, l.Width()-1, "…")
}

func truncate(s string, width int) string {
	return ansi.Truncate(s, width, "…")
}

// sanitize keeps one printable line from script output.
func sanitize(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\t", "  ")
	if i := strings.LastIndex(s, "\r"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}
