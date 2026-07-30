package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-promql-extractor/internal/testsupport"
)

// TestBulkListingExtractsTheSameQueries is the parity check between the two
// ways of enumerating dashboards: whatever the fetching strategy, the output
// has to be the one the fixtures declare.
func TestBulkListingExtractsTheSameQueries(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:   fixtures,
		Bulk:         true,
		BulkPageSize: 3,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--bulk", "on")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(fixtures))
	if got := fake.Requests(testsupport.DashboardRoute); got != 0 {
		t.Errorf("fetched %d dashboards one by one, want none once they arrive in pages", got)
	}
	if got := fake.Requests(testsupport.BulkRoute); got < 2 {
		t.Errorf("made %d bulk requests, want several pages", got)
	}
}

func TestFallsBackToFetchingOneByOne(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}

	cases := map[string]struct {
		options testsupport.FakeOptions
		args    []string
		reason  string
	}{
		"older grafana": {
			options: testsupport.FakeOptions{Dashboards: fixtures},
			reason:  "the endpoint does not exist before Grafana 12",
		},
		"another organization's namespace": {
			options: testsupport.FakeOptions{Dashboards: fixtures, Bulk: true, BulkNamespace: "org-4"},
			reason:  "an unknown namespace answers with an empty list, not an error",
		},
		"folder filter": {
			options: testsupport.FakeOptions{Dashboards: fixtures, Bulk: true},
			args:    []string{"--folder-uid", "whatever"},
			reason:  "listing cannot filter by folder",
		},
		"resuming a search run": {
			options: testsupport.FakeOptions{Dashboards: fixtures, Bulk: true},
			args:    []string{"--start-page", "2", "--page-size", "5"},
			reason:  "page numbers belong to the search API",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := testsupport.NewFakeGrafana(t, tc.options)
			out := filepath.Join(t.TempDir(), "queries.txt")

			args := append([]string{"--url", fake.URL, "-o", out,
				"--compress=false", "--progress", "never", "--bulk", "auto"}, tc.args...)
			if stderr, err := runCLI(t, args...); err != nil {
				t.Fatalf("run failed: %v\n%s", err, stderr)
			}

			// The folder filter case asks for a folder the fake ignores, so
			// only the fetching strategy is asserted here, not the contents.
			if got := fake.Requests(testsupport.DashboardRoute); got == 0 {
				t.Errorf("no dashboard was fetched one by one, although %s", tc.reason)
			}
		})
	}
}

// TestStartPageIsNotSilentlyIgnored guards the interaction that would cost
// dashboards: a run resuming at a later search page must not end up on the bulk
// path, which knows nothing about page numbers.
func TestStartPageIsNotSilentlyIgnored(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(20)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards, Bulk: true})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--bulk", "auto", "--page-size", "10", "--start-page", "2")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	lines := readLines(t, out)
	if got := len(distinctUIDs(lines)); got != 10 {
		t.Errorf("resumed run covered %d dashboards, want the 10 of the second page", got)
	}
}

func TestBulkOnRequiresAnInstanceThatServesIt(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards: testsupport.GeneratedFixtures(2),
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--bulk", "on")
	if err == nil {
		t.Fatalf("run succeeded, want a refusal:\n%s", stderr)
	}
	if !strings.Contains(err.Error(), "Grafana 12") {
		t.Errorf("error does not say what is missing: %v", err)
	}
}

// TestPagesAreTakenWhenOffered covers the choice the tool makes when nothing is
// asked of it: an instance that serves whole dashboards a page at a time is
// read that way, since the result is checked afterwards either way.
func TestPagesAreTakenWhenOffered(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(5)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards, Bulk: true})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out,
		"--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
	if got := fake.Requests(testsupport.BulkRoute); got == 0 {
		t.Error("fetched dashboards one by one although the instance offered them in pages")
	}
	if got := fake.Requests(testsupport.DashboardRoute); got != 0 {
		t.Errorf("fetched %d dashboards by uid although the pages carried them all", got)
	}
}

func TestBulkOffKeepsFetchingOneByOne(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(5)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards, Bulk: true})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--bulk", "off"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
	if got := fake.Requests(testsupport.BulkRoute); got != 0 {
		t.Errorf("made %d bulk requests with --bulk=off", got)
	}
}

// TestFetchesDashboardsAListingOnlyNames covers pages that name dashboards
// without carrying them. Leaving those out would lose dashboards silently,
// since a page says nothing about what it failed to include, so they are
// fetched the old way instead.
func TestFetchesDashboardsAListingOnlyNames(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(7)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:           dashboards,
		Bulk:                 true,
		BulkPageSize:         3,
		BulkWithoutDocuments: true,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--bulk", "on"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
	if got := fake.Requests(testsupport.DashboardRoute); got != len(dashboards) {
		t.Errorf("fetched %d dashboards by uid, want all %d the listing could not carry",
			got, len(dashboards))
	}
}

// TestFetchesWhatThePagesLeftOut covers the failure that looks like success: an
// instance that ends a listing early leaves the run believing it is done. What
// the pages delivered is compared against what /api/search says the instance
// holds, and the difference is fetched one by one, so the run ends up complete
// rather than merely finished.
func TestFetchesWhatThePagesLeftOut(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(20)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:    dashboards,
		Bulk:          true,
		BulkPageSize:  5,
		BulkStopAfter: 10,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--bulk", "on")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
	if got := fake.Requests(testsupport.DashboardRoute); got != 10 {
		t.Errorf("fetched %d dashboards by uid, want the 10 the pages never carried", got)
	}
	if !strings.Contains(stderr, "left out of the pages") {
		t.Errorf("the summary does not say the instance left dashboards out:\n%s", stderr)
	}
}

// TestASampleIsTakenAtItsWord covers the one case the check cannot cover: a run
// that stops at --max-dashboards has no idea how many it should have seen, so
// it says as much rather than treating the rest of the instance as missing.
func TestASampleIsTakenAtItsWord(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(20)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:   dashboards,
		Bulk:         true,
		BulkPageSize: 5,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--bulk", "on", "--max-dashboards", "5")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	if got := len(distinctUIDs(readLines(t, out))); got != 5 {
		t.Errorf("wrote %d dashboards, want the 5 that were asked for", got)
	}
	if got := fake.Requests(testsupport.SearchRoute); got != 0 {
		t.Errorf("made %d search requests to check a run that stopped early", got)
	}
	if !strings.Contains(stderr, "--max-dashboards") {
		t.Errorf("the run does not say the sample went unchecked:\n%s", stderr)
	}
}

// TestContinueTokenResumesAListing covers the recipe an interrupted bulk run
// prints, including its refusal to fall back to a strategy that would ignore
// the token and start over.
func TestContinueTokenResumesAListing(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(10)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:   dashboards,
		Bulk:         true,
		BulkPageSize: 4,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--progress", "never", "--continue-token", "8"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards[8:]))

	plain := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	_, err := runCLI(t, "--url", plain.URL, "-o", filepath.Join(t.TempDir(), "queries.txt"),
		"--compress=false", "--progress", "never", "--continue-token", "8")
	if err == nil {
		t.Fatal("a token was accepted against an instance that cannot resume it")
	}
}
