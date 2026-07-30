//go:build corpus

// Package corpus validates the extractor against the most downloaded community
// dashboards on grafana.com. Build with -tags=corpus to include it.
//
// Dashboards are cached on disk and never committed, and requests are made one
// at a time with a delay between them, so a repeated run costs grafana.com
// nothing.
package corpus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	listURL     = "https://grafana.com/api/dashboards"
	downloadURL = "https://grafana.com/api/dashboards/%d/revisions/latest/download"

	// apiPageSize is the largest page grafana.com serves.
	apiPageSize = 100

	defaultDashboards      = 1000
	defaultRequestInterval = 500 * time.Millisecond
	requestTimeout         = 30 * time.Second
	maxAttempts            = 3

	userAgent = "grafana-promql-extractor-corpus-test/1.0 " +
		"(+https://github.com/felixbarny/grafana-promql-extractor)"
)

// Entry is one dashboard in the grafana.com catalog.
type Entry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	// Downloads is how often the dashboard was downloaded, the ranking used here.
	Downloads int64 `json:"downloads"`
	Revision  int   `json:"revision"`
	// Datasource is the datasource grafana.com lists the dashboard under. It is
	// maintained independently of the dashboard JSON, which makes it a useful
	// cross-check for the extractor's own datasource resolution.
	Datasource string `json:"datasource"`
}

// UID is the dashboard uid used when serving the corpus. Community dashboards
// share or omit uids, so a unique one is assigned, exactly as a real Grafana
// does on import.
func (e Entry) UID() string { return fmt.Sprintf("gcom-%d", e.ID) }

// URL is the dashboard's page on grafana.com.
func (e Entry) URL() string { return fmt.Sprintf("https://grafana.com/grafana/dashboards/%d", e.ID) }

// Dashboard is a catalog entry together with its downloaded document.
type Dashboard struct {
	Entry
	// JSON is the dashboard document with its uid replaced by Entry.UID.
	JSON []byte
}

// Options configures the corpus download.
type Options struct {
	// Dashboards is how many of the most downloaded dashboards to include.
	Dashboards int
	// CacheDir holds the catalog pages and the downloaded dashboards.
	CacheDir string
	// RequestInterval is the minimum delay between requests to grafana.com.
	RequestInterval time.Duration
	// Refresh ignores cached files and downloads everything again.
	Refresh bool
}

// OptionsFromEnv reads the corpus settings from the environment.
func OptionsFromEnv(t *testing.T) Options {
	t.Helper()

	opts := Options{
		Dashboards:      defaultDashboards,
		CacheDir:        defaultCacheDir(t),
		RequestInterval: defaultRequestInterval,
		Refresh:         os.Getenv("CORPUS_REFRESH") != "",
	}
	if raw := os.Getenv("CORPUS_DASHBOARDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("CORPUS_DASHBOARDS=%q is not a positive number", raw)
		}
		opts.Dashboards = n
	}
	if raw := os.Getenv("CORPUS_CACHE_DIR"); raw != "" {
		opts.CacheDir = raw
	}
	if raw := os.Getenv("CORPUS_REQUEST_INTERVAL"); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval < 0 {
			t.Fatalf("CORPUS_REQUEST_INTERVAL=%q is not a duration", raw)
		}
		opts.RequestInterval = interval
	}
	return opts
}

// defaultCacheDir keeps the cache inside the repository, where .gitignore
// excludes it, so that it survives between runs but is never committed.
func defaultCacheDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	return filepath.Join(filepath.Dir(wd), ".cache", "grafana-com")
}

// client fetches from grafana.com no faster than one request per interval.
type client struct {
	http     *http.Client
	interval time.Duration
	last     time.Time
	requests int
}

func newClient(interval time.Duration) *client {
	return &client{http: &http.Client{Timeout: requestTimeout}, interval: interval}
}

// errNotFound marks a dashboard that grafana.com no longer serves.
var errNotFound = errors.New("not found")

func (c *client) get(url string) ([]byte, error) {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		c.wait()

		body, err := c.attempt(url)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, errNotFound) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// wait spaces requests out, so that a full corpus download stays gentle.
func (c *client) wait() {
	if elapsed := time.Since(c.last); elapsed < c.interval {
		time.Sleep(c.interval - elapsed)
	}
	c.last = time.Now()
	c.requests++
}

func (c *client) attempt(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", url, errNotFound)
	case resp.StatusCode == http.StatusTooManyRequests:
		if delay := retryAfter(resp.Header.Get("Retry-After")); delay > 0 {
			time.Sleep(delay)
		}
		return nil, fmt.Errorf("%s: rate limited", url)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func retryAfter(header string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, time.Minute)
	}
	return 0
}

// Load returns the most downloaded dashboards, fetching whatever the cache is
// missing. The first run takes a while by design; later runs read from disk.
func Load(t *testing.T, opts Options) []Dashboard {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(opts.CacheDir, "dashboards"), 0o755); err != nil {
		t.Fatalf("creating cache directory: %v", err)
	}
	c := newClient(opts.RequestInterval)

	entries := loadCatalog(t, c, opts)
	t.Logf("catalog: %d dashboards, cache %s", len(entries), opts.CacheDir)

	dashboards := make([]Dashboard, 0, len(entries))
	var downloaded, cached, missing int
	start := time.Now()

	for i, entry := range entries {
		raw, err := loadDashboard(c, opts, entry, &downloaded, &cached)
		switch {
		case errors.Is(err, errNotFound):
			missing++
			continue
		case err != nil:
			t.Fatalf("downloading dashboard %d (%s): %v", entry.ID, entry.Name, err)
		}

		document, err := withUID(raw, entry.UID())
		if err != nil {
			// A document that is not even a JSON object cannot be served; record
			// it as missing rather than failing the whole corpus.
			t.Logf("skipping dashboard %d (%s): %v", entry.ID, entry.Name, err)
			missing++
			continue
		}
		dashboards = append(dashboards, Dashboard{Entry: entry, JSON: document})

		if downloaded > 0 && (i+1)%100 == 0 {
			t.Logf("fetched %d/%d dashboards (%d from cache, %s elapsed)",
				i+1, len(entries), cached, time.Since(start).Round(time.Second))
		}
	}

	t.Logf("corpus ready: %d dashboards (%d downloaded, %d from cache, %d unavailable), %d requests to grafana.com",
		len(dashboards), downloaded, cached, missing, c.requests)
	return dashboards
}

func loadCatalog(t *testing.T, c *client, opts Options) []Entry {
	t.Helper()

	var entries []Entry
	seen := make(map[int]bool)
	for page := 1; len(entries) < opts.Dashboards; page++ {
		path := filepath.Join(opts.CacheDir, fmt.Sprintf("catalog-page-%03d.json", page))

		raw, err := readCache(path, opts.Refresh)
		if err != nil {
			url := fmt.Sprintf("%s?orderBy=downloads&direction=desc&page=%d&pageSize=%d",
				listURL, page, apiPageSize)
			if raw, err = c.get(url); err != nil {
				t.Fatalf("listing dashboards (page %d): %v", page, err)
			}
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatalf("caching catalog page %d: %v", page, err)
			}
		}

		var response struct {
			Items []Entry `json:"items"`
			Pages int     `json:"pages"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatalf("decoding catalog page %d: %v", page, err)
		}
		if len(response.Items) == 0 {
			break
		}

		// grafana.com ranks by a download count that keeps moving while the
		// pages are fetched, so a dashboard can show up on two of them. Keeping
		// the first occurrence makes the corpus a set of distinct dashboards.
		for _, item := range response.Items {
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			entries = append(entries, item)
		}
		if page >= response.Pages {
			break
		}
	}

	if len(entries) > opts.Dashboards {
		entries = entries[:opts.Dashboards]
	}
	return entries
}

func loadDashboard(c *client, opts Options, entry Entry, downloaded, cached *int) ([]byte, error) {
	path := filepath.Join(opts.CacheDir, "dashboards", fmt.Sprintf("%d.json", entry.ID))
	tombstone := path + ".unavailable"

	if !opts.Refresh {
		if _, err := os.Stat(tombstone); err == nil {
			return nil, errNotFound
		}
	}
	if raw, err := readCache(path, opts.Refresh); err == nil {
		*cached++
		return raw, nil
	}

	raw, err := c.get(fmt.Sprintf(downloadURL, entry.ID))
	if errors.Is(err, errNotFound) {
		// Remember the gap so later runs do not ask again.
		_ = os.WriteFile(tombstone, []byte(time.Now().Format(time.RFC3339)), 0o644)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return nil, fmt.Errorf("caching dashboard %d: %w", entry.ID, err)
	}
	*downloaded++
	return raw, nil
}

func readCache(path string, refresh bool) ([]byte, error) {
	if refresh {
		return nil, errors.New("cache bypassed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty cache file")
	}
	return raw, nil
}

// withUID replaces the document's uid so every dashboard in the corpus is
// addressable, since community dashboards frequently omit or reuse uids.
func withUID(raw []byte, uid string) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("not a json object: %w", err)
	}
	encoded, err := json.Marshal(uid)
	if err != nil {
		return nil, err
	}
	document["uid"] = encoded
	return json.Marshal(document)
}
