package cli

import (
	"fmt"
	"io"

	"github.com/felixbarny/grafana-promql-extractor/internal/progress"
)

// logger routes warnings around the progress line so they are not overwritten.
type logger struct {
	out     io.Writer
	verbose bool
	// tracker owns the terminal once it is set, so messages have to go through
	// it to avoid landing on top of a half-drawn progress line.
	tracker *progress.Tracker
}

func (l *logger) warnf(format string, args ...any) {
	if l.tracker != nil {
		l.tracker.Logf(format, args...)
		return
	}
	fmt.Fprintf(l.out, format+"\n", args...)
}

func (l *logger) debugf(format string, args ...any) {
	if l.verbose {
		l.warnf(format, args...)
	}
}
