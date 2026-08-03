package analyze_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestScanExport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")
	content := "dash1;up\n dash2 ; sum(rate(x[5m])) \n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var entries []analyze.Entry
	if err := analyze.ScanExport(path, func(e analyze.Entry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].DashboardUID != "dash1" || entries[0].Query != "up" {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[1].DashboardUID != "dash2" {
		t.Fatalf("second dashboard uid = %q", entries[1].DashboardUID)
	}
}

func TestScanExportInvalidLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")
	if err := os.WriteFile(path, []byte("no-separator\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := analyze.ScanExport(path, func(analyze.Entry) error { return nil }); err == nil {
		t.Fatal("expected error for invalid line")
	}
}
