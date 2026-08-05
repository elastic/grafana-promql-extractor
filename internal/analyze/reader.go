package analyze

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
)

// Entry is one query read from an export file.
type Entry struct {
	LineNumber   int
	DashboardUID string
	Query        string
}

// ScanExport reads dashboardUID;query lines from path and calls fn for each
// non-empty entry. Gzip is detected from a .gz suffix. The file is opened once
// per call; scan it again for a second pass over the same export.
func ScanExport(path string, fn func(Entry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("opening gzip %s: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 10<<20)
	lineNumber := 0
	for sc.Scan() {
		lineNumber++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		uid, query, ok := strings.Cut(line, ";")
		if !ok {
			return fmt.Errorf("line %d: expected dashboardUID;query", lineNumber)
		}
		uid = strings.TrimSpace(uid)
		query = strings.TrimSpace(query)
		if uid == "" || query == "" {
			return fmt.Errorf("line %d: empty dashboard uid or query", lineNumber)
		}
		if err := fn(Entry{
			LineNumber:   lineNumber,
			DashboardUID: uid,
			Query:        query,
		}); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}
