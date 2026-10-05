package analyze_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestNewClientRejectsInvalidTimes(t *testing.T) {
	_, err := analyze.NewClient(analyze.ClientConfig{
		BaseURL: "http://localhost:9200",
		Start:   "not-a-time",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid start time") {
		t.Fatalf("err = %v", err)
	}

	_, err = analyze.NewClient(analyze.ClientConfig{
		BaseURL: "http://localhost:9200",
		End:     "also-bad",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid end time") {
		t.Fatalf("err = %v", err)
	}

	_, err = analyze.NewClient(analyze.ClientConfig{
		BaseURL: "http://localhost:9200",
		Start:   "2026-01-02T00:00:00Z",
		End:     "2026-01-01T00:00:00Z",
	})
	if err == nil || !strings.Contains(err.Error(), "must be before") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientQueryRangeEndOnlyWindow(t *testing.T) {
	var gotStart, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		gotStart = r.URL.Query().Get("start")
		gotEnd = r.URL.Query().Get("end")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	}))
	t.Cleanup(srv.Close)

	end := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	client, err := analyze.NewClient(analyze.ClientConfig{
		BaseURL: srv.URL,
		End:     end.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, _, _, err := client.QueryRange(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected success")
	}
	if gotEnd != end.Format(time.RFC3339) {
		t.Fatalf("end = %q, want %q", gotEnd, end.Format(time.RFC3339))
	}
	wantStart := end.Add(-5 * time.Minute).Format(time.RFC3339)
	if gotStart != wantStart {
		t.Fatalf("start = %q, want %q", gotStart, wantStart)
	}
}

func TestClientQueryRangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":    "error",
			"errorType": "bad_data",
			"error":     "Subquery queries are not supported at this time [foo[5m:]]",
		})
	}))
	t.Cleanup(srv.Close)

	client, err := analyze.NewClient(analyze.ClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ok, msg, _, err := client.QueryRange(context.Background(), "foo[5m:]")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected failure")
	}
	if !strings.Contains(msg, "Subquery queries are not supported") {
		t.Fatalf("msg = %q", msg)
	}
}

func TestClientQueryRangeMissingStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"resultType":"matrix","result":[]}}`))
	}))
	t.Cleanup(srv.Close)

	client, err := analyze.NewClient(analyze.ClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ok, msg, _, err := client.QueryRange(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected failure without prometheus status")
	}
	if msg != "missing prometheus status" {
		t.Fatalf("msg = %q", msg)
	}
}

func TestStreamAnalyze(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "unless") {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "error",
				"error":  "set operator [unless] is not supported at this time",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	}))
	t.Cleanup(srv.Close)

	client, err := analyze.NewClient(analyze.ClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")
	content := "d1;up\nd2;a unless b\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	failedPath := filepath.Join(dir, "failed.txt")
	failed := &analyze.FailedExport{Path: failedPath}

	report := analyze.NewReport()
	if err := analyze.StreamAnalyze(context.Background(), path, analyze.StreamOptions{
		Client:      client,
		Concurrency: 2,
		Report:      report,
		Failed:      failed,
	}); err != nil {
		t.Fatal(err)
	}
	if report.TotalQueries() != 2 {
		t.Fatalf("total = %d", report.TotalQueries())
	}
	if report.SuccessfulQueries() != 1 {
		t.Fatalf("successful = %d", report.SuccessfulQueries())
	}
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	if failed.Count() != 1 {
		t.Fatalf("failed count = %d", failed.Count())
	}

	var failedEntries []analyze.Entry
	if err := analyze.ScanExport(failedPath, func(e analyze.Entry) error {
		failedEntries = append(failedEntries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(failedEntries) != 1 {
		t.Fatalf("failed entries = %d", len(failedEntries))
	}
	if failedEntries[0].DashboardUID != "d2" || failedEntries[0].Query != "a unless b" {
		t.Fatalf("failed entry = %+v", failedEntries[0])
	}
}

func TestStreamAnalyzeRejectsFailedOutputSameAsInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")
	content := "d1;up\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := analyze.NewClient(analyze.ClientConfig{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	failed := &analyze.FailedExport{Path: filepath.Join(dir, ".", "queries.txt")}
	err = analyze.StreamAnalyze(context.Background(), path, analyze.StreamOptions{
		Client: client,
		Report: analyze.NewReport(),
		Failed: failed,
	})
	if err == nil || !strings.Contains(err.Error(), "same as the input") {
		t.Fatalf("err = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("input was modified: %q", got)
	}
}

func TestStreamAnalyzeReplacesFailedOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	}))
	t.Cleanup(srv.Close)

	client, err := analyze.NewClient(analyze.ClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")
	if err := os.WriteFile(path, []byte("d1;up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failedPath := filepath.Join(dir, "failed.txt")
	if err := os.WriteFile(failedPath, []byte("old;a unless b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	failed := &analyze.FailedExport{Path: failedPath}
	if err := analyze.StreamAnalyze(context.Background(), path, analyze.StreamOptions{
		Client: client,
		Report: analyze.NewReport(),
		Failed: failed,
	}); err != nil {
		t.Fatal(err)
	}
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}

	var got []analyze.Entry
	if err := analyze.ScanExport(failedPath, func(e analyze.Entry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("entries = %d, want 0 after a clean run", len(got))
	}
}
