//go:build integration && corpus

package integration

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-dashboard-extractor/corpus"
)

// versionsUnderTest are the releases community dashboards are extracted from.
// One per major line the tool supports: the last before the Kubernetes-style
// API existed, the one that introduced it, and the current one.
var versionsUnderTest = []string{
	"grafana/grafana:11.6.6",
	"grafana/grafana:12.4.0",
	"grafana/grafana:13.0.1",
}

// versionCorpusSize is how many community dashboards each release is given.
// Enough to cover the shapes real dashboards come in, few enough that three
// containers and six extractions stay within a couple of minutes.
const versionCorpusSize = 250

// TestCorpusAcrossVersions stores real community dashboards in each supported
// Grafana release and checks that the same queries come back out of all of
// them, whichever way they are read.
//
// The unit tests serve dashboards from a fake, which cannot say whether a
// release stores a document the way the tool expects to read it back, and the
// throughput test covers only one release. Grafana rewrites a dashboard when it
// is saved and again when it is served, and those rules differ between
// releases, which is exactly what this test would notice.
func TestCorpusAcrossVersions(t *testing.T) {
	dashboards := corpus.Load(t, corpus.OptionsFromEnv(t))
	if len(dashboards) == 0 {
		t.Skip("the corpus cache is empty; run make test-corpus first")
	}
	if len(dashboards) > versionCorpusSize {
		dashboards = dashboards[:versionCorpusSize]
	}

	var results []versionResult

	for _, image := range versionsUnderTest {
		t.Run(shortVersion(image), func(t *testing.T) {
			instance := StartImage(t, image, 0)
			stored := instance.uploadCorpus(t, dashboards, FolderUID)
			searchable := instance.waitForSearchable(t, len(instance.All())+len(stored))
			t.Logf("stored %d of %d community dashboards, %d listed in total",
				len(stored), len(dashboards), searchable)

			// Whether this release serves whole dashboards a page at a time is
			// the difference between the releases, so the run is left to work
			// it out and then asked what it did.
			paged := instance.extract(t, "--bulk", "auto")
			oneByOne := instance.extract(t, "--bulk", "off")

			assertSameQueries(t, oneByOne.queries, paged.queries, "one by one", "in pages")
			if paged.dashboards != searchable {
				t.Errorf("processed %d dashboards, want the %d Grafana lists", paged.dashboards, searchable)
			}
			results = append(results, versionResult{
				version: shortVersion(image),
				queries: paged.queries,
				stored:  stored,
				pages:   paged.pages,
			})
		})
	}

	if len(results) < 2 {
		return
	}
	// A release that reads dashboards in pages has to produce what a release
	// without that API produces from the same documents, which is the property
	// the fake cannot check: it serves what it was given, while a real Grafana
	// stores and returns its own idea of the document.
	//
	// A dashboard one release refused to store is a community dashboard that
	// release cannot hold, not something the extractor did, so the comparison
	// leaves those out and says which.
	for _, r := range results[1:] {
		left, right, skipped := sharedDashboards(results[0], r)
		for _, uid := range skipped {
			t.Logf("dashboard %s is not on both %s and %s and was left out of the comparison",
				uid, results[0].version, r.version)
		}
		assertSameQueries(t, left, right, results[0].version, r.version)
	}
	for _, r := range results {
		t.Logf("%s: %d dashboards with queries, read %s", r.version, len(r.queries), readAs(r.pages))
	}
}

// versionResult is what one Grafana release yielded.
type versionResult struct {
	version string
	queries map[string][]string
	// stored are the uids the release accepted, which is not every community
	// dashboard on every release.
	stored map[string]bool
	// pages records whether the release served whole dashboards a page at a
	// time.
	pages bool
}

// sharedDashboards narrows two releases to the dashboards both of them hold,
// and names the ones left out. A uid neither release stored is one of the
// fixtures, provisioned identically on both; a uid only one of them stored is a
// community dashboard the other refused to save.
func sharedDashboards(a, b versionResult) (left, right map[string][]string, skipped []string) {
	onlyOne := func(uid string) bool { return a.stored[uid] != b.stored[uid] }

	left = make(map[string][]string, len(a.queries))
	right = make(map[string][]string, len(b.queries))
	for uid, queries := range a.queries {
		if onlyOne(uid) {
			skipped = append(skipped, uid)
			continue
		}
		left[uid] = queries
	}
	for uid, queries := range b.queries {
		if onlyOne(uid) {
			skipped = append(skipped, uid)
			continue
		}
		right[uid] = queries
	}
	slices.Sort(skipped)
	return left, right, slices.Compact(skipped)
}

// extraction is what one run over the instance produced.
type extraction struct {
	queries    map[string][]string
	dashboards int
	// pages records whether the run read dashboards a page at a time.
	pages bool
}

func (i *Instance) extract(t *testing.T, extra ...string) extraction {
	t.Helper()

	out := filepath.Join(t.TempDir(), "queries.txt")
	args := append([]string{
		"--url", i.URL,
		"--user", adminUser,
		"--password", adminPassword,
		"-o", out,
		"--compress=false",
		"--progress", "never",
		"--verbose",
	}, extra...)

	stderr, err := execute(t, args...)
	if err != nil {
		t.Fatalf("extraction failed: %v\n%s", err, stderr)
	}
	if repaired := repairedPattern.FindStringSubmatch(stderr); repaired != nil {
		t.Logf("Grafana left %s dashboards out of its pages, which the run fetched one by one",
			repaired[1])
	}
	return extraction{
		queries:    queriesByDashboard(t, out),
		dashboards: summaryCount(t, processedPattern, stderr),
		pages:      strings.Contains(stderr, "in pages of up to"),
	}
}

// assertSameQueries compares two extractions dashboard by dashboard, naming
// what each one has that the other does not, since a count of the difference
// says nothing about which side is wrong.
func assertSameQueries(t *testing.T, want, got map[string][]string, wantLabel, gotLabel string) {
	t.Helper()

	uids := make([]string, 0, len(want))
	for uid := range want {
		uids = append(uids, uid)
	}
	for uid := range got {
		if _, ok := want[uid]; !ok {
			uids = append(uids, uid)
		}
	}
	slices.Sort(uids)

	differing := 0
	for _, uid := range uids {
		missing := difference(want[uid], got[uid])
		extra := difference(got[uid], want[uid])
		if len(missing) == 0 && len(extra) == 0 {
			continue
		}
		differing++
		if differing > 5 {
			continue
		}
		t.Errorf("dashboard %s differs between %s and %s", uid, wantLabel, gotLabel)
		for _, query := range missing {
			t.Logf("    only in %s: %s", wantLabel, truncate(query))
		}
		for _, query := range extra {
			t.Logf("    only in %s: %s", gotLabel, truncate(query))
		}
	}
	if differing > 5 {
		t.Errorf("%d dashboards in total differ between %s and %s", differing, wantLabel, gotLabel)
	}
}

func shortVersion(image string) string {
	_, version, _ := strings.Cut(image, ":")
	return version
}

func readAs(pages bool) string {
	if pages {
		return "in pages"
	}
	return "one by one"
}
