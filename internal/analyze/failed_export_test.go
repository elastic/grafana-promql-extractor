package analyze_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestFailedExportPlain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.txt")
	w := &analyze.FailedExport{Path: path}
	if err := w.Write("d1", "up"); err != nil {
		t.Fatal(err)
	}
	if err := w.Write("d2", "a unless b"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.Count() != 2 {
		t.Fatalf("count = %d", w.Count())
	}

	var got []analyze.Entry
	if err := analyze.ScanExport(path, func(e analyze.Entry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d", len(got))
	}
	if got[0].DashboardUID != "d1" || got[0].Query != "up" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].DashboardUID != "d2" || got[1].Query != "a unless b" {
		t.Fatalf("got[1] = %+v", got[1])
	}
}

func TestFailedExportGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.txt.gz")
	w := &analyze.FailedExport{Path: path}
	if err := w.Write("dash", `rate(http_requests_total[5m])`); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var got []analyze.Entry
	if err := analyze.ScanExport(path, func(e analyze.Entry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DashboardUID != "dash" || got[0].Query != `rate(http_requests_total[5m])` {
		t.Fatalf("got = %+v", got)
	}
}

func TestFailedExportLazyOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.txt")
	w := &analyze.FailedExport{Path: path}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file, stat err = %v", err)
	}
	if w.Count() != 0 {
		t.Fatalf("count = %d", w.Count())
	}
}

func TestFailedExportOpenTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.txt")
	w := &analyze.FailedExport{Path: path}
	if err := w.Write("d1", "up"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w = &analyze.FailedExport{Path: path}
	if err := w.Open(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var got []analyze.Entry
	if err := analyze.ScanExport(path, func(e analyze.Entry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("entries = %d, want 0 after truncate", len(got))
	}
}

func TestFailedExportCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "failed.txt")
	w := &analyze.FailedExport{Path: path}
	if err := w.Write("d1", "up"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var got []analyze.Entry
	if err := analyze.ScanExport(path, func(e analyze.Entry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DashboardUID != "d1" || got[0].Query != "up" {
		t.Fatalf("got = %+v", got)
	}
}

func TestFailedExportCloseDoesNotReplayWriteError(t *testing.T) {
	w := &analyze.FailedExport{Path: ""}
	if err := w.Write("d1", "up"); err == nil {
		t.Fatal("expected write error for empty path")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}
