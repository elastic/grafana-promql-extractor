package testsupport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// FakeOptions configures a FakeGrafana.
type FakeOptions struct {
	// Dashboards are served by /api/search and /api/dashboards/uid/:uid.
	Dashboards []Fixture
	// DatasourcesStatus overrides the status of /api/datasources, so the
	// /api/frontend/settings fallback can be exercised.
	DatasourcesStatus int
	// FrontendSettingsStatus overrides the status of /api/frontend/settings.
	FrontendSettingsStatus int
	// RejectSort makes /api/search reject requests carrying a sort parameter,
	// as older Grafana releases do.
	RejectSort bool
	// FailTimes maps a request path to the number of times it should fail with
	// FailStatus before succeeding.
	FailTimes map[string]int
	// FailStatus is the status returned while FailTimes is not exhausted.
	FailStatus int
	// RetryAfter is sent with failure responses.
	RetryAfter string
	// IgnorePageParam makes /api/search always return the first page, as a
	// misconfigured proxy might.
	IgnorePageParam bool
	// SyntheticDashboards serves this many dashboards without holding any of
	// them in memory, so that scale tests measure only the extractor.
	SyntheticDashboards int
}

// SyntheticUID is the uid of the nth synthetic dashboard, one-based.
func SyntheticUID(n int) string { return fmt.Sprintf("syn-%06d", n) }

// SyntheticQueries are the queries of the nth synthetic dashboard.
func SyntheticQueries(n int) []string {
	uid := SyntheticUID(n)
	return []string{
		fmt.Sprintf("syn_metric_total{dashboard=%q}", uid),
		fmt.Sprintf("rate(syn_metric_total{dashboard=%q}[5m])", uid),
	}
}

func syntheticDashboardJSON(n int) string {
	queries := SyntheticQueries(n)
	return fmt.Sprintf(`{"uid":%q,"title":"Synthetic %d","schemaVersion":39,"panels":[
		{"type":"timeseries","title":"Synthetic","datasource":{"type":"prometheus","uid":%q},
		 "targets":[{"refId":"A","expr":%q},{"refId":"B","expr":%q}]}]}`,
		SyntheticUID(n), n, PrometheusUID, queries[0], queries[1])
}

// FakeGrafana is an in-process stand-in for a Grafana instance.
type FakeGrafana struct {
	URL    string
	Server *httptest.Server

	opts       FakeOptions
	byUID      map[string]Fixture
	mu         sync.Mutex
	requests   map[string]int
	failsLeft  map[string]int
	lastSearch map[string]string
}

// NewFakeGrafana starts a fake Grafana and registers cleanup.
func NewFakeGrafana(t *testing.T, opts FakeOptions) *FakeGrafana {
	t.Helper()

	if opts.FailStatus == 0 {
		opts.FailStatus = http.StatusServiceUnavailable
	}
	fake := &FakeGrafana{
		opts:       opts,
		byUID:      make(map[string]Fixture, len(opts.Dashboards)),
		requests:   make(map[string]int),
		failsLeft:  make(map[string]int, len(opts.FailTimes)),
		lastSearch: make(map[string]string),
	}
	for _, d := range opts.Dashboards {
		fake.byUID[d.UID] = d
	}
	for path, n := range opts.FailTimes {
		fake.failsLeft[path] = n
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", fake.handleHealth)
	mux.HandleFunc("/api/datasources", fake.handleDatasources)
	mux.HandleFunc("/api/frontend/settings", fake.handleFrontendSettings)
	mux.HandleFunc("/api/search", fake.handleSearch)
	mux.HandleFunc("/api/dashboards/uid/", fake.handleDashboard)

	fake.Server = httptest.NewServer(fake.instrument(mux))
	fake.URL = fake.Server.URL
	t.Cleanup(fake.Server.Close)
	return fake
}

// Requests returns how often a path was requested.
func (f *FakeGrafana) Requests(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

// LastSearchQuery returns the value a search parameter had on the last request.
func (f *FakeGrafana) LastSearchQuery(param string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSearch[param]
}

// dashboardRoute is the key request counts are recorded under for dashboard
// fetches, so that counting stays bounded when serving many dashboards.
const dashboardRoute = "/api/dashboards/uid/"

func routeKey(path string) string {
	if strings.HasPrefix(path, dashboardRoute) {
		return dashboardRoute
	}
	return path
}

func (f *FakeGrafana) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests[routeKey(r.URL.Path)]++
		remaining := f.failsLeft[r.URL.Path]
		if remaining > 0 {
			f.failsLeft[r.URL.Path] = remaining - 1
		}
		f.mu.Unlock()

		if remaining > 0 {
			if f.opts.RetryAfter != "" {
				w.Header().Set("Retry-After", f.opts.RetryAfter)
			}
			http.Error(w, "injected failure", f.opts.FailStatus)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *FakeGrafana) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"database": "ok", "version": "12.0.0-fake"})
}

func (f *FakeGrafana) handleDatasources(w http.ResponseWriter, _ *http.Request) {
	if status := f.opts.DatasourcesStatus; status != 0 && status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	writeJSON(w, Datasources())
}

func (f *FakeGrafana) handleFrontendSettings(w http.ResponseWriter, _ *http.Request) {
	if status := f.opts.FrontendSettingsStatus; status != 0 && status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	datasources := make(map[string]any)
	defaultName := ""
	for _, ds := range Datasources() {
		datasources[ds.Name] = map[string]any{
			"uid":       ds.UID,
			"name":      ds.Name,
			"type":      ds.Type,
			"isDefault": ds.IsDefault,
		}
		if ds.IsDefault {
			defaultName = ds.Name
		}
	}
	writeJSON(w, map[string]any{
		"datasources":       datasources,
		"defaultDatasource": defaultName,
	})
}

func (f *FakeGrafana) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	f.mu.Lock()
	for _, param := range []string{"limit", "page", "sort", "type"} {
		f.lastSearch[param] = query.Get(param)
	}
	f.mu.Unlock()

	if f.opts.RejectSort && query.Get("sort") != "" {
		http.Error(w, "Invalid sort option", http.StatusBadRequest)
		return
	}

	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 1000
	}
	page, err := strconv.Atoi(query.Get("page"))
	if err != nil || page <= 0 {
		page = 1
	}
	if f.opts.IgnorePageParam {
		page = 1
	}

	type hit struct {
		UID       string `json:"uid"`
		Title     string `json:"title"`
		Type      string `json:"type"`
		FolderUID string `json:"folderUid"`
	}
	hits := []hit{}
	start := (page - 1) * limit
	if total := f.opts.SyntheticDashboards; total > 0 {
		for i := start; i < start+limit && i < total; i++ {
			hits = append(hits, hit{UID: SyntheticUID(i + 1), Title: "Synthetic", Type: "dash-db"})
		}
	} else {
		for i := start; i < start+limit && i < len(f.opts.Dashboards); i++ {
			hits = append(hits, hit{
				UID:   f.opts.Dashboards[i].UID,
				Title: f.opts.Dashboards[i].Title,
				Type:  "dash-db",
			})
		}
	}
	writeJSON(w, hits)
}

func (f *FakeGrafana) handleDashboard(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/")

	if n, ok := syntheticIndex(uid); ok && n <= f.opts.SyntheticDashboards {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"meta":{"url":"/d/%s"},"dashboard":%s}`, uid, syntheticDashboardJSON(n))
		return
	}

	fixture, ok := f.byUID[uid]
	if !ok {
		http.Error(w, `{"message":"Dashboard not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"meta":{"folderTitle":"General","url":"/d/%s"},"dashboard":%s}`, uid, fixture.JSON)
}

// syntheticIndex parses a synthetic dashboard uid back into its index.
func syntheticIndex(uid string) (int, bool) {
	rest, ok := strings.CutPrefix(uid, "syn-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// GeneratedFixtures builds n synthetic dashboards, for tests that need more
// dashboards than the curated fixture set provides.
func GeneratedFixtures(n int) []Fixture {
	fixtures := make([]Fixture, 0, n)
	for i := 1; i <= n; i++ {
		uid := fmt.Sprintf("gen-%04d", i)
		title := fmt.Sprintf("Generated dashboard %d", i)
		queries := []string{
			fmt.Sprintf("gen_metric_total{dashboard=%q}", uid),
			fmt.Sprintf("rate(gen_metric_total{dashboard=%q}[5m])", uid),
		}

		doc := map[string]any{
			"uid":           uid,
			"title":         title,
			"schemaVersion": 39,
			"panels": []any{
				map[string]any{
					"type":       "timeseries",
					"title":      "Generated",
					"datasource": map[string]string{"type": "prometheus", "uid": PrometheusUID},
					"targets": []any{
						map[string]string{"refId": "A", "expr": queries[0]},
						map[string]string{"refId": "B", "expr": queries[1]},
					},
				},
			},
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			panic(err)
		}

		expected := make([]string, 0, len(queries))
		for _, q := range queries {
			expected = append(expected, uid+";"+q)
		}
		fixtures = append(fixtures, Fixture{
			Name:     uid,
			UID:      uid,
			Title:    title,
			JSON:     raw,
			Expected: expected,
		})
	}
	return fixtures
}
