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

	report := analyze.NewReport()
	if err := analyze.StreamAnalyze(context.Background(), path, analyze.StreamOptions{
		Client:      client,
		Concurrency: 2,
		Report:      report,
	}); err != nil {
		t.Fatal(err)
	}
	if report.TotalQueries() != 2 {
		t.Fatalf("total = %d", report.TotalQueries())
	}
	if report.SuccessfulQueries() != 1 {
		t.Fatalf("successful = %d", report.SuccessfulQueries())
	}
}
