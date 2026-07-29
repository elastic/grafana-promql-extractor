//go:build integration && corpus

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
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
	searchable := instance.waitForFolder(t, FolderUID, uploaded)
	t.Logf("provisioned %d of %d dashboards in %s, %d of them listed by search",
		uploaded, len(dashboards), upload.Round(time.Millisecond), searchable)

	// A first pass lets Grafana fill its caches, so that the numbers below
	// compare the extractor's settings rather than the state of the instance.
	instance.measure(t, "warm-up", 8)

	runs := []struct {
		label       string
		concurrency int
		args        []string
	}{
		{label: "sequential", concurrency: 1},
		{label: "default", concurrency: 8},
		{label: "concurrency 16", concurrency: 16},
		{label: "anonymized", concurrency: 8, args: []string{"--anonymize"}},
		{label: "anonymized, gzipped", concurrency: 8, args: []string{"--anonymize", "--compress=true"}},
	}

	results := make([]measurement, 0, len(runs))
	for _, run := range runs {
		results = append(results, instance.measure(t, run.label, run.concurrency, run.args...))
	}

	t.Logf("extracting %d grafana.com dashboards from Grafana %s:", searchable, image())
	for _, r := range results {
		projected := r.elapsed / time.Duration(max(r.dashboards, 1)) * projectedSize
		t.Logf("  %-20s %8s  %5.0f dashboards/s  %6d queries  ~%s for %d dashboards",
			r.label, r.elapsed.Round(10*time.Millisecond), r.rate(), r.queries,
			projected.Round(time.Second), projectedSize)
	}

	// The settings must not change the result, only how fast it arrives.
	// Anonymization runs after deduplication, so it cannot merge queries either.
	for _, r := range results[1:] {
		if r.dashboards != results[0].dashboards || r.queries != results[0].queries {
			t.Errorf("%s extracted %d queries from %d dashboards, but %s extracted %d from %d",
				r.label, r.queries, r.dashboards, results[0].label, results[0].queries, results[0].dashboards)
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
	elapsed    time.Duration
	dashboards int
	queries    int
}

func (m measurement) rate() float64 {
	if m.elapsed <= 0 {
		return 0
	}
	return float64(m.dashboards) / m.elapsed.Seconds()
}

// measure runs one extraction over the corpus folder and times it. The folder
// filter keeps the curated fixtures out, so the numbers cover the corpus only.
func (i *Instance) measure(t *testing.T, label string, concurrency int, extra ...string) measurement {
	t.Helper()

	out := filepath.Join(t.TempDir(), "queries.txt")
	args := append([]string{
		"--url", i.URL,
		"--token", i.ViewerToken,
		"--folder-uid", FolderUID,
		"-o", out,
		"--compress=false",
		"--progress", "never",
		"--concurrency", strconv.Itoa(concurrency),
	}, extra...)

	start := time.Now()
	stderr, err := execute(t, args...)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("%s run failed: %v\n%s", label, err, stderr)
	}
	t.Logf("%s (concurrency %d) took %s\n%s", label, concurrency, elapsed.Round(time.Millisecond), stderr)

	return measurement{
		label:      label,
		elapsed:    elapsed,
		dashboards: summaryCount(t, processedPattern, stderr),
		queries:    summaryCount(t, queriesPattern, stderr),
	}
}

var (
	// processedPattern counts every dashboard fetched, including those that
	// hold no PromQL and therefore produce no output.
	processedPattern = regexp.MustCompile(`Processed ([\d,]+) dashboard`)
	queriesPattern   = regexp.MustCompile(`queries written\D+([\d,]+)`)
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

// waitForFolder waits until Grafana lists want dashboards in the folder and
// returns how many it ends up listing. Indexing is asynchronous, and a release
// that never catches up should still yield a measurement over the dashboards it
// does serve, so a shortfall is only reported.
func (i *Instance) waitForFolder(t *testing.T, folderUID string, want int) int {
	t.Helper()

	deadline := time.Now().Add(indexingTimeout)
	for {
		count := i.countInFolder(t, folderUID)
		if count >= want {
			return count
		}
		if time.Now().After(deadline) {
			t.Logf("Grafana lists %d of the %d uploaded dashboards after %s", count, want, indexingTimeout)
			return count
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (i *Instance) countInFolder(t *testing.T, folderUID string) int {
	t.Helper()

	path := fmt.Sprintf("/api/search?type=dash-db&limit=5000&folderUIDs=%s", url.QueryEscape(folderUID))
	resp, err := i.client.Do(i.request(t, http.MethodGet, path, nil))
	if err != nil {
		t.Fatalf("searching folder %s: %v", folderUID, err)
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
