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

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

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

func TestClientWithIndex(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	}))
	t.Cleanup(srv.Close)

	client, err := analyze.NewClient(analyze.ClientConfig{
		BaseURL: srv.URL,
		Index:   "metrics-*",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.QueryRange(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/_prometheus/metrics-*/api/v1/query_range" {
		t.Fatalf("path = %q", gotPath)
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
