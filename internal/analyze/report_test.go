package analyze_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestWriteMarkdownReport(t *testing.T) {
	report := analyze.NewReport()
	report.Record("d1", "up", true, nil)
	report.Record("d2", "a unless b", false, []string{"set operator [...] is not supported at this time"})

	var buf bytes.Buffer
	if err := report.WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Successful Queries") {
		t.Fatalf("missing summary:\n%s", out)
	}
	if !strings.Contains(out, "unless") {
		t.Fatalf("missing error group:\n%s", out)
	}
}

func TestReportCounts(t *testing.T) {
	report := analyze.NewReport()
	report.Record("d1", "up", true, nil)
	report.Record("d1", "down", true, nil)
	report.Record("d2", "bad", false, []string{"Function foo is not yet implemented"})

	if got := report.TotalQueries(); got != 3 {
		t.Fatalf("total = %d", got)
	}
	if got := report.SuccessfulQueries(); got != 2 {
		t.Fatalf("successful = %d", got)
	}
}
