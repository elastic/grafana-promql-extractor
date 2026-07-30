package grafana_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felixbarny/grafana-promql-extractor/internal/grafana"
)

func TestNewNormalizesURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://grafana.example.com", "https://grafana.example.com"},
		{"https://grafana.example.com/", "https://grafana.example.com"},
		{"grafana.example.com", "https://grafana.example.com"},
		{"http://localhost:3000/grafana/", "http://localhost:3000/grafana"},
	}
	for _, tc := range tests {
		client, err := grafana.New(grafana.Config{BaseURL: tc.in})
		if err != nil {
			t.Fatalf("New(%q): %v", tc.in, err)
		}
		if got := client.BaseURL(); got != tc.want {
			t.Errorf("New(%q).BaseURL() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewRejectsInvalidURL(t *testing.T) {
	for _, in := range []string{"", "   ", "https://"} {
		if _, err := grafana.New(grafana.Config{BaseURL: in}); err == nil {
			t.Errorf("New(%q) succeeded, want an error", in)
		}
	}
}

// TestSubPathBaseURL covers Grafana served under a path prefix.
func TestSubPathBaseURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"database":"ok","version":"12.0.0"}`))
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{BaseURL: server.URL + "/grafana"})
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if gotPath != "/grafana/api/health" {
		t.Errorf("requested %q, want /grafana/api/health", gotPath)
	}
}

func TestBearerAuth(t *testing.T) {
	var auth, orgID, userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		orgID = r.Header.Get("X-Grafana-Org-Id")
		userAgent = r.Header.Get("User-Agent")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{
		BaseURL:   server.URL,
		Token:     "glsa_token",
		OrgID:     7,
		UserAgent: "test-agent",
	})
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if auth != "Bearer glsa_token" {
		t.Errorf("Authorization = %q", auth)
	}
	if orgID != "7" {
		t.Errorf("X-Grafana-Org-Id = %q, want 7", orgID)
	}
	if userAgent != "test-agent" {
		t.Errorf("User-Agent = %q", userAgent)
	}
}

func TestBasicAuthUsedWithoutToken(t *testing.T) {
	var user, pass string
	var ok bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{BaseURL: server.URL, User: "admin", Password: "secret"})
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !ok || user != "admin" || pass != "secret" {
		t.Errorf("basic auth = %q/%q (ok=%t)", user, pass, ok)
	}
}

func TestTokenWinsOverBasicAuth(t *testing.T) {
	var auth string
	var basicOK bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _, basicOK = r.BasicAuth()
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{BaseURL: server.URL, Token: "tok", User: "admin", Password: "secret"})
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if auth != "Bearer tok" || basicOK {
		t.Errorf("Authorization = %q, basic auth present = %t", auth, basicOK)
	}
}

func TestStatusErrorCarriesDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Permission denied"}`, http.StatusForbidden)
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{BaseURL: server.URL})
	_, err := client.Health(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !grafana.IsStatus(err, http.StatusForbidden) {
		t.Errorf("IsStatus(403) = false for %v", err)
	}
	if grafana.IsStatus(err, http.StatusNotFound) {
		t.Errorf("IsStatus(404) = true for %v", err)
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("error should include the response body, got %v", err)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= 2 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"database":"ok","version":"12.0.0"}`))
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{
		BaseURL:        server.URL,
		Retries:        3,
		RetryBaseDelay: time.Millisecond,
	})
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3", got)
	}
}

func TestRetriesGiveUpAfterLimit(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{BaseURL: server.URL, Retries: 2, RetryBaseDelay: time.Millisecond})
	if _, err := client.Health(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3 (initial plus 2 retries)", got)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{BaseURL: server.URL, Retries: 5, RetryBaseDelay: time.Millisecond})
	if _, err := client.Health(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1: 4xx responses must not be retried", got)
	}
}

func TestHonorsRetryAfterHeader(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"database":"ok"}`))
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{
		BaseURL: server.URL,
		Retries: 2,
		// A base delay far below the Retry-After value, so the elapsed time
		// shows which one was honored.
		RetryBaseDelay: time.Millisecond,
	})

	start := time.Now()
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("waited %s, want at least the 1s from Retry-After", elapsed)
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := mustClient(t, grafana.Config{BaseURL: server.URL, Retries: 100, RetryBaseDelay: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, err := client.Health(ctx)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected a context error, got %v", err)
	}
}

func mustClient(t *testing.T, cfg grafana.Config) *grafana.Client {
	t.Helper()
	client, err := grafana.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}
