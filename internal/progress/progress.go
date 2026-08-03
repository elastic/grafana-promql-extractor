// Package progress renders extraction progress to a terminal or log stream.
package progress

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// Mode selects how progress is rendered.
type Mode string

const (
	// ModeAuto renders an updating line on a terminal and periodic lines otherwise.
	ModeAuto Mode = "auto"
	// ModeAlways forces the updating line.
	ModeAlways Mode = "always"
	// ModeNever disables progress output.
	ModeNever Mode = "never"

	ttyInterval = 200 * time.Millisecond
	logInterval = 15 * time.Second
)

// ParseMode validates a progress mode.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeAuto:
		return ModeAuto, nil
	case ModeAlways:
		return ModeAlways, nil
	case ModeNever:
		return ModeNever, nil
	}
	return "", fmt.Errorf("invalid progress mode %q: want auto, always or never", s)
}

const (
	// UnitDashboards labels the primary counter for extract runs.
	UnitDashboards = "dashboards"
	// UnitQueries labels the primary counter for analyze runs.
	UnitQueries = "queries"
)

// Tracker counts progress and renders it periodically.
type Tracker struct {
	out      io.Writer
	tty      bool
	enabled  bool
	interval time.Duration
	width    int
	unit     string

	// total is fixed at construction; zero means unknown.
	total      int64
	dashboards atomic.Int64
	queries    atomic.Int64
	failures   atomic.Int64

	start time.Time

	mu      sync.Mutex
	started bool
	stopped bool
	stop    chan struct{}
	done    chan struct{}
	lastLen int
}

// New builds a Tracker writing to out. A total of zero means unknown.
func New(out io.Writer, total int, mode Mode) *Tracker {
	tty := isTerminal(out)
	t := &Tracker{
		out:      out,
		tty:      tty || mode == ModeAlways,
		enabled:  mode != ModeNever,
		unit:     UnitDashboards,
		total:    int64(total),
		start:    time.Now(),
		width:    terminalWidth(out),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		interval: logInterval,
	}
	if t.tty {
		t.interval = ttyInterval
	}
	return t
}

// SetUnit sets the label for the primary counter. Call before Start.
func (t *Tracker) SetUnit(unit string) {
	if unit == "" {
		unit = UnitDashboards
	}
	t.unit = unit
}

// Start begins rendering until Stop is called. Calling Stop without Start is
// allowed and does nothing.
func (t *Tracker) Start() {
	t.mu.Lock()
	if t.started || t.stopped || !t.enabled {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()

	go func() {
		defer close(t.done)
		ticker := time.NewTicker(t.interval)
		defer ticker.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-ticker.C:
				t.render()
			}
		}
	}()
}

// Stop halts rendering and clears the progress line. It is safe to call more
// than once.
func (t *Tracker) Stop() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	started := t.started
	close(t.stop)
	t.mu.Unlock()

	if started {
		<-t.done
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tty && t.lastLen > 0 {
		fmt.Fprintf(t.out, "\r%s\r", strings.Repeat(" ", t.lastLen))
		t.lastLen = 0
	}
}

// Logf writes a message on its own line, without leaving a partially drawn
// progress line behind it.
func (t *Tracker) Logf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tty && t.lastLen > 0 {
		fmt.Fprintf(t.out, "\r%s\r", strings.Repeat(" ", t.lastLen))
		t.lastLen = 0
	}
	fmt.Fprintf(t.out, format+"\n", args...)
}

// AddDashboard records a processed dashboard and the queries it yielded.
func (t *Tracker) AddDashboard(queries int) {
	t.dashboards.Add(1)
	if queries > 0 {
		t.queries.Add(int64(queries))
	}
}

// AddQuery records one processed query when the tracker unit is queries.
func (t *Tracker) AddQuery() {
	t.dashboards.Add(1)
	t.queries.Add(1)
}

// AddFailure records a dashboard that could not be processed.
func (t *Tracker) AddFailure() {
	t.failures.Add(1)
	t.dashboards.Add(1)
}

// Elapsed returns the time since the tracker was created.
func (t *Tracker) Elapsed() time.Duration { return time.Since(t.start) }

// Counts returns the current dashboard, query and failure counts.
func (t *Tracker) Counts() (dashboards, queries, failures int) {
	return int(t.dashboards.Load()), int(t.queries.Load()), int(t.failures.Load())
}

func (t *Tracker) render() {
	line := t.line()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tty {
		pad := ""
		if n := t.lastLen - len(line); n > 0 {
			pad = strings.Repeat(" ", n)
		}
		fmt.Fprintf(t.out, "\r%s%s", line, pad)
		t.lastLen = len(line)
		return
	}
	fmt.Fprintln(t.out, line)
}

func (t *Tracker) line() string {
	done := t.dashboards.Load()
	queries := t.queries.Load()
	failures := t.failures.Load()
	total := t.total
	elapsed := time.Since(t.start)
	unit := t.unit
	if unit == "" {
		unit = UnitDashboards
	}

	var b strings.Builder
	if total > 0 {
		percent := float64(done) / float64(total) * 100
		fmt.Fprintf(&b, "%5.1f%% | %s/%s %s", percent, HumanInt(done), HumanInt(total), unit)
	} else {
		fmt.Fprintf(&b, "%s %s", HumanInt(done), unit)
	}
	if unit == UnitDashboards {
		fmt.Fprintf(&b, " | %s queries", HumanInt(queries))
	}

	rate := float64(done) / elapsed.Seconds()
	if elapsed > time.Second && rate > 0 {
		fmt.Fprintf(&b, " | %.0f/s", rate)
	}
	fmt.Fprintf(&b, " | %s", truncateDuration(elapsed))
	if total > 0 && rate > 0 && done < total {
		remaining := time.Duration(float64(total-done)/rate) * time.Second
		fmt.Fprintf(&b, " | ~%s left", truncateDuration(remaining))
	}
	if failures > 0 {
		fmt.Fprintf(&b, " | %s failed", HumanInt(failures))
	}

	line := b.String()
	if t.tty && t.width > 0 && len(line) > t.width-1 {
		line = line[:t.width-1]
	}
	return line
}

func truncateDuration(d time.Duration) string {
	if d >= time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// HumanInt formats n with thousands separators.
func HumanInt[T int | int64](n T) string {
	s := strconv.FormatInt(int64(n), 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return sign + b.String()
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}
