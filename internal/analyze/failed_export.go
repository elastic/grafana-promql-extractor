package analyze

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FailedExport streams failed dashboardUID;query lines to a file. It is safe
// for concurrent use. Open creates and truncates the file; Write also opens
// on first use. If Close is called with no Open and no writes, nothing is
// created on disk. Gzip is used when Path ends with .gz.
type FailedExport struct {
	Path string

	mu    sync.Mutex
	file  *os.File
	buf   *bufio.Writer
	gz    *gzip.Writer
	count int
	err   error
}

// Open creates parent directories and truncates Path. StreamAnalyze calls it
// before checking queries so a previous run's failures are not left behind
// when this run has none. Write calls it on first use if Open was not called.
func (w *FailedExport) Open() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.file != nil {
		return nil
	}
	if err := w.open(); err != nil {
		w.err = err
		return err
	}
	return nil
}

// Write appends one failed query in the same format as an extract export.
func (w *FailedExport) Write(dashboardUID, query string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.file == nil {
		if err := w.open(); err != nil {
			w.err = err
			return err
		}
	}
	line := dashboardUID + ";" + query + "\n"
	if _, err := io.WriteString(w.sink(), line); err != nil {
		w.err = fmt.Errorf("writing failed queries to %s: %w", w.Path, err)
		return w.err
	}
	w.count++
	return nil
}

// Count returns how many failed queries have been written.
func (w *FailedExport) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// Close flushes and closes the file if one was opened. It reports only
// flush and close errors, not a sticky error from an earlier Write.
func (w *FailedExport) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	var firstErr error
	fail := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if w.gz != nil {
		fail(w.gz.Close())
	}
	fail(w.buf.Flush())
	fail(w.file.Close())
	w.file, w.buf, w.gz = nil, nil, nil
	if firstErr != nil {
		err := fmt.Errorf("closing failed queries file %s: %w", w.Path, firstErr)
		if w.err == nil {
			w.err = err
		}
		return err
	}
	return nil
}

func (w *FailedExport) open() error {
	path := strings.TrimSpace(w.Path)
	if path == "" {
		return fmt.Errorf("failed export path is empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating failed queries directory %s: %w", dir, err)
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating failed queries file %s: %w", path, err)
	}
	w.file = file
	w.buf = bufio.NewWriter(file)
	if strings.HasSuffix(path, ".gz") {
		w.gz = gzip.NewWriter(w.buf)
	}
	return nil
}

func (w *FailedExport) sink() io.Writer {
	if w.gz != nil {
		return w.gz
	}
	return w.buf
}

// SameExportPath reports whether a and b name the same path after trimming
// space and resolving to an absolute location.
func SameExportPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return absA == absB
}
