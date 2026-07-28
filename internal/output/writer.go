// Package output writes extracted queries as "dashboardUID;query" lines,
// optionally gzip compressed and split across several files.
package output

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Separator divides the dashboard UID from the query. Consumers should
	// split on the first occurrence only, since a PromQL label value may
	// legitimately contain a semicolon.
	Separator = ';'

	bufferSize = 256 << 10
)

// Options configures a Writer.
type Options struct {
	// Path is the output file path. When splitting or compressing, a suffix and
	// an index are inserted before the extension.
	Path string
	// Compress gzips the output.
	Compress bool
	// DashboardsPerFile rotates to a new file after this many dashboards with
	// at least one query. Zero writes a single file.
	DashboardsPerFile int
	// Append adds to existing files instead of truncating them. Concatenated
	// gzip streams remain valid, so this works with Compress as well.
	Append bool
}

// FileInfo describes one written output file.
type FileInfo struct {
	Path       string
	Dashboards int
	Queries    int
}

// Writer streams queries to disk. It is not safe for concurrent use; the
// pipeline funnels every result through a single writer goroutine.
type Writer struct {
	opt Options

	file *os.File
	// buf buffers writes to the file; gzBuf buffers writes into the compressor
	// so that per-dashboard writes do not each pay deflate call overhead.
	buf   *bufio.Writer
	gz    *gzip.Writer
	gzBuf *bufio.Writer

	current FileInfo
	files   []FileInfo
	line    []byte
}

// New creates a Writer. No file is opened until the first dashboard with
// queries arrives, so a run that finds nothing leaves no empty files behind.
func New(opt Options) *Writer {
	return &Writer{opt: opt, line: make([]byte, 0, 1024)}
}

// WriteDashboard appends every query of one dashboard. Rotation happens between
// dashboards, so a dashboard's queries never span two files.
func (w *Writer) WriteDashboard(uid string, queries []string) error {
	if len(queries) == 0 {
		return nil
	}
	if w.opt.DashboardsPerFile > 0 && w.current.Dashboards >= w.opt.DashboardsPerFile {
		if err := w.closeCurrent(); err != nil {
			return err
		}
	}
	if w.file == nil {
		if err := w.openNext(); err != nil {
			return err
		}
	}

	safeUID := sanitizeUID(uid)
	w.line = w.line[:0]
	for _, query := range queries {
		w.line = append(w.line, safeUID...)
		w.line = append(w.line, Separator)
		w.line = append(w.line, query...)
		w.line = append(w.line, '\n')
	}
	if _, err := w.sink().Write(w.line); err != nil {
		return fmt.Errorf("writing to %s: %w", w.current.Path, err)
	}

	w.current.Dashboards++
	w.current.Queries += len(queries)
	return nil
}

// Close flushes and closes the current file.
func (w *Writer) Close() error {
	return w.closeCurrent()
}

// Files returns the files written so far, including the one still open.
func (w *Writer) Files() []FileInfo {
	files := append([]FileInfo(nil), w.files...)
	if w.file != nil {
		files = append(files, w.current)
	}
	return files
}

func (w *Writer) sink() *bufio.Writer {
	if w.gz != nil {
		return w.gzBuf
	}
	return w.buf
}

func (w *Writer) openNext() error {
	path := w.pathFor(len(w.files) + 1)
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating output directory %s: %w", dir, err)
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if w.opt.Append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return fmt.Errorf("creating output file %s: %w", path, err)
	}

	w.file = file
	w.buf = bufio.NewWriterSize(file, bufferSize)
	if w.opt.Compress {
		w.gz = gzip.NewWriter(w.buf)
		w.gzBuf = bufio.NewWriterSize(w.gz, bufferSize)
	}
	w.current = FileInfo{Path: path}
	return nil
}

func (w *Writer) closeCurrent() error {
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
		fail(w.gzBuf.Flush())
		fail(w.gz.Close())
	}
	fail(w.buf.Flush())
	fail(w.file.Close())

	w.files = append(w.files, w.current)
	w.file, w.buf, w.gz, w.gzBuf = nil, nil, nil, nil
	w.current = FileInfo{}

	if firstErr != nil {
		return fmt.Errorf("closing output file: %w", firstErr)
	}
	return nil
}

// pathFor builds the path of the nth output file. A single-file run keeps the
// requested name; a split run inserts a zero-padded index before the extension.
func (w *Writer) pathFor(index int) string {
	path := w.opt.Path
	if w.opt.DashboardsPerFile > 0 {
		dir, file := filepath.Split(path)
		ext := filepath.Ext(file)
		base := strings.TrimSuffix(file, ext)
		path = filepath.Join(dir, fmt.Sprintf("%s-%05d%s", base, index, ext))
	}
	if w.opt.Compress && !strings.HasSuffix(path, ".gz") {
		path += ".gz"
	}
	return path
}

// sanitizeUID keeps the line format parseable even if a UID ever contained a
// separator or a newline.
func sanitizeUID(uid string) string {
	if !strings.ContainsAny(uid, ";\n\r") {
		return uid
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case ';', '\n', '\r':
			return '_'
		}
		return r
	}, uid)
}
