//go:build integration && corpus

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felixbarny/grafana-dashboard-extractor/corpus"
)

const (
	// uploadWorkers post dashboards in parallel, since provisioning a thousand
	// of them one at a time dominates the runtime of this test.
	uploadWorkers = 8
	// projectedSize is the instance size the measured rate is extrapolated to,
	// which is the scale the tool is built for.
	projectedSize = 50_000
)

// TestCorpusThroughput stores the cached community dashboards in a real Grafana
// and measures how long extracting them takes. The corpus test serves the same
// documents from an in-process fake, which says nothing about how fast Grafana
// itself answers, so the runtime of a real extraction can only be measured here.
//
// It is a measurement rather than an assertion: the numbers land in the test log
// and only a pathological regression fails the test.
func TestCorpusThroughput(t *testing.T) {
	dashboards := corpus.Load(t, corpus.OptionsFromEnv(t))
	if len(dashboards) == 0 {
		t.Skip("the corpus cache is empty; run make test-corpus first")
	}

	instance := Start(t, 0)
	uploadStart := time.Now()
	uploaded := instance.uploadCorpus(t, dashboards, FolderUID)
	upload := time.Since(uploadStart)
	// Bulk listing covers the whole instance, so the measurement does too,
	// fixtures included, and every run sees the same set.
	searchable := instance.waitForSearchable(t, len(instance.All())+uploaded)
	t.Logf("provisioned %d of %d dashboards in %s, %d dashboards listed in total",
		uploaded, len(dashboards), upload.Round(time.Millisecond), searchable)

	// A first pass lets Grafana fill its caches, so that the numbers below
	// compare the extractor's settings rather than the state of the instance.
	if _, err := instance.measure(t, "warm-up", 8, "--bulk", "off"); err != nil {
		t.Fatalf("warm-up run failed: %v", err)
	}

	runs := []struct {
		label       string
		concurrency int
		args        []string
	}{
		{label: "one by one, 1 worker", concurrency: 1, args: []string{"--bulk", "off"}},
		{label: "one by one, 8 workers", concurrency: 8, args: []string{"--bulk", "off"}},
		{label: "one by one, 16 workers", concurrency: 16, args: []string{"--bulk", "off"}},
		{label: "one by one, anonymized", concurrency: 8, args: []string{"--bulk", "off", "--anonymize"}},
		{label: "in pages", concurrency: 8, args: []string{"--bulk", "on"}},
		{label: "in pages, anonymized", concurrency: 8, args: []string{"--bulk", "on", "--anonymize"}},
		{label: "in pages, gzipped", concurrency: 8, args: []string{"--bulk", "on", "--anonymize", "--compress=true"}},
	}

	results := make([]measurement, 0, len(runs))
	for _, run := range runs {
		result, err := instance.measure(t, run.label, run.concurrency, run.args...)
		if err != nil {
			t.Fatalf("%s did not finish: %v", run.label, err)
		}
		results = append(results, result)
	}

	t.Logf("extracting %d grafana.com dashboards from Grafana %s:", searchable, image())
	for _, r := range results {
		projected := r.elapsed / time.Duration(max(r.dashboards, 1)) * projectedSize
		note := ""
		if r.repaired > 0 {
			note = fmt.Sprintf("  (%d fetched one by one after Grafana skipped them)", r.repaired)
		}
		t.Logf("  %-20s %8s  %5.0f dashboards/s  %6d queries  ~%s for %d dashboards%s",
			r.label, r.elapsed.Round(10*time.Millisecond), r.rate(), r.queries,
			projected.Round(time.Second), projectedSize, note)
	}

	// The settings must not change the result, only how fast it arrives. This
	// is also the parity check between the two ways of reading dashboards, over
	// a thousand real ones: listing them in pages hands back documents Grafana
	// has migrated to the current schema, and that has to yield exactly the
	// queries that fetching each dashboard by uid does. Anonymization runs
	// after deduplication, so it cannot merge queries either.
	for _, r := range results[1:] {
		if r.dashboards != results[0].dashboards || r.queries != results[0].queries {
			t.Errorf("%s extracted %d queries from %d dashboards, but %s extracted %d from %d",
				r.label, r.queries, r.dashboards, results[0].label, results[0].queries, results[0].dashboards)
			if r.plain && results[0].plain {
				reportDifferences(t, results[0], r)
			}
		}
	}
	if results[0].dashboards != searchable {
		t.Errorf("processed %d dashboards, want the %d Grafana lists", results[0].dashboards, searchable)
	}
	// Loopback HTTP against a container on the same machine; anything slower
	// than this points at a stall rather than at a slow environment.
	if rate := results[1].rate(); rate < 10 {
		t.Errorf("extraction ran at %.1f dashboards/s, which is too slow to be explained by the environment", rate)
	}
}

// measurement is the outcome of one extraction run.
type measurement struct {
	label      string
	path       string
	elapsed    time.Duration
	dashboards int
	queries    int
	// repaired is how many dashboards Grafana left out of its pages and the
	// run fetched one by one instead.
	repaired int
	// plain marks a run whose queries can be compared with another run's,
	// which pseudonymized ones cannot.
	plain bool
}

func (m measurement) rate() float64 {
	if m.elapsed <= 0 {
		return 0
	}
	return float64(m.dashboards) / m.elapsed.Seconds()
}

// measure runs one extraction over the whole instance and times it.
func (i *Instance) measure(t *testing.T, label string, concurrency int, extra ...string) (measurement, error) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "queries.txt")
	args := append([]string{
		"--url", i.URL,
		"--token", i.ViewerToken,
		"-o", out,
		"--compress=false",
		"--progress", "never",
		"--verbose",
		"--concurrency", strconv.Itoa(concurrency),
	}, extra...)

	start := time.Now()
	stderr, err := execute(t, args...)
	elapsed := time.Since(start)
	t.Logf("%s (concurrency %d) took %s\n%s", label, concurrency, elapsed.Round(time.Millisecond), stderr)
	if err != nil {
		return measurement{}, err
	}

	repaired := 0
	if match := repairedPattern.FindStringSubmatch(stderr); match != nil {
		repaired = summaryCount(t, repairedPattern, stderr)
		t.Logf("%s: Grafana left %d dashboards out of its pages, so this run measures "+
			"the fallback more than the fast path", label, repaired)
	}

	return measurement{
		label:      label,
		path:       out,
		elapsed:    elapsed,
		dashboards: summaryCount(t, processedPattern, stderr),
		queries:    summaryCount(t, queriesPattern, stderr),
		repaired:   repaired,
		plain:      !slices.Contains(extra, "--anonymize"),
	}, nil
}

// reportDifferences names the dashboards two runs disagree about, since a bare
// count says nothing about whether the extra queries are a gain or a loss.
func reportDifferences(t *testing.T, a, b measurement) {
	t.Helper()

	left, right := queriesByDashboard(t, a.path), queriesByDashboard(t, b.path)
	uids := make(map[string]bool, len(left)+len(right))
	for uid := range left {
		uids[uid] = true
	}
	for uid := range right {
		uids[uid] = true
	}

	reported := 0
	for uid := range uids {
		only := difference(left[uid], right[uid])
		extra := difference(right[uid], left[uid])
		if len(only) == 0 && len(extra) == 0 {
			continue
		}
		reported++
		if reported > 10 {
			continue
		}
		t.Logf("dashboard %s: %d queries only in %q, %d only in %q", uid, len(only), a.label, len(extra), b.label)
		for _, query := range only {
			t.Logf("    only %s: %s", a.label, truncate(query))
		}
		for _, query := range extra {
			t.Logf("    only %s: %s", b.label, truncate(query))
		}
	}
	t.Logf("%d dashboards differ between %q and %q", reported, a.label, b.label)
}

func queriesByDashboard(t *testing.T, path string) map[string][]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	queries := make(map[string][]string)
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		uid, query, found := strings.Cut(line, ";")
		if found {
			queries[uid] = append(queries[uid], query)
		}
	}
	return queries
}

func truncate(query string) string {
	if len(query) > 160 {
		return query[:160] + "..."
	}
	return query
}

var (
	// processedPattern counts every dashboard fetched, including those that
	// hold no PromQL and therefore produce no output.
	processedPattern = regexp.MustCompile(`Processed ([\d,]+) dashboard`)
	queriesPattern   = regexp.MustCompile(`queries written\D+([\d,]+)`)
	// repairedPattern counts the dashboards Grafana left out of its pages,
	// which the run then fetched one by one. A run that repaired most of the
	// instance measures the speed of the slow path, not the fast one.
	repairedPattern = regexp.MustCompile(`left out of the pages:\s+([\d,]+)`)
)

func summaryCount(t *testing.T, pattern *regexp.Regexp, stderr string) int {
	t.Helper()

	match := pattern.FindStringSubmatch(stderr)
	if match == nil {
		t.Fatalf("no %q in the summary:\n%s", pattern, stderr)
	}
	value, err := strconv.Atoi(strings.ReplaceAll(match[1], ",", ""))
	if err != nil {
		t.Fatalf("summary reported %q: %v", match[1], err)
	}
	return value
}

// waitForSearchable waits until Grafana lists want dashboards and returns how
// many it ends up listing. Indexing is asynchronous, and a release that never
// catches up should still yield a measurement over the dashboards it does
// serve, so a shortfall is only reported.
func (i *Instance) waitForSearchable(t *testing.T, want int) int {
	t.Helper()

	deadline := time.Now().Add(indexingTimeout)
	for {
		count := i.searchableCount(t)
		if count >= want {
			return count
		}
		if time.Now().After(deadline) {
			t.Logf("Grafana lists %d of the %d dashboards uploaded after %s", count, want, indexingTimeout)
			return count
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (i *Instance) searchableCount(t *testing.T) int {
	t.Helper()

	resp, err := i.client.Do(i.request(t, http.MethodGet, "/api/search?type=dash-db&limit=5000", nil))
	if err != nil {
		t.Fatalf("searching dashboards: %v", err)
	}
	defer resp.Body.Close()

	var hits []struct{}
	if err := json.NewDecoder(resp.Body).Decode(&hits); err != nil {
		t.Fatalf("decoding search response: %v", err)
	}
	return len(hits)
}

// uploadCorpus stores every dashboard in folderUID and returns how many landed.
// Community dashboards are written for a range of Grafana versions, so the ones
// the instance under test rejects are reported and skipped rather than failing
// the measurement.
func (i *Instance) uploadCorpus(t *testing.T, dashboards []corpus.Dashboard, folderUID string) int {
	t.Helper()

	var (
		mu       sync.Mutex
		rejected []string
		wg       sync.WaitGroup
	)
	queue := make(chan corpus.Dashboard)
	for range uploadWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dashboard := range queue {
				if err := i.uploadDocument(dashboard.JSON, folderUID); err != nil {
					mu.Lock()
					rejected = append(rejected, fmt.Sprintf("%s: %v", dashboard.URL(), err))
					mu.Unlock()
				}
			}
		}()
	}
	for _, dashboard := range dashboards {
		queue <- dashboard
	}
	close(queue)
	wg.Wait()

	for _, reason := range rejected {
		t.Logf("Grafana rejected %s", reason)
	}
	return len(dashboards) - len(rejected)
}

// uploadDocument stores one dashboard document as it is, apart from the two
// fields Grafana owns: the numeric id it assigns itself, and the title, which
// has to be unique within a folder and is not something the extractor reads.
func (i *Instance) uploadDocument(document []byte, folderUID string) error {
	var dashboard map[string]any
	if err := json.Unmarshal(document, &dashboard); err != nil {
		return fmt.Errorf("decoding dashboard: %w", err)
	}
	delete(dashboard, "id")
	uid, _ := dashboard["uid"].(string)
	title, _ := dashboard["title"].(string)
	dashboard["title"] = strings.TrimSpace(title + " " + uid)

	payload := map[string]any{"dashboard": dashboard, "overwrite": true}
	if folderUID != "" {
		payload["folderUid"] = folderUID
	}

	status, response, err := i.postJSON("/api/dashboards/db", payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status %d: %s", status, firstLine(response))
	}
	return nil
}

// firstLine keeps a rejection readable when Grafana answers with a page instead
// of a message.
func firstLine(response []byte) string {
	line := string(response)
	if index := strings.IndexAny(line, "\r\n"); index >= 0 {
		line = line[:index]
	}
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return line
}
