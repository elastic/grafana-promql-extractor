package cli_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/cli"
	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

// runCLI drives the real command in process and returns everything it wrote to
// stderr.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := cli.NewRootCmd()
	var stderr bytes.Buffer
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)

	err := cmd.ExecuteContext(context.Background())
	return stderr.String(), err
}

func TestExtractsEveryFixtureDashboard(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	got := readLines(t, out)
	want := testsupport.ExpectedLines(fixtures)
	assertSameLines(t, got, want)

	if !strings.Contains(stderr, "queries written") {
		t.Errorf("summary missing from output:\n%s", stderr)
	}
}

func TestCompressesByDefault(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--progress", "never"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	if _, err := os.Stat(out); err == nil {
		t.Error("expected no uncompressed file")
	}
	assertSameLines(t, readGzipLines(t, out+".gz"), testsupport.ExpectedLines(fixtures))
}

func TestMaxDashboards(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(25)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--max-dashboards", "5", "--page-size", "10", "--progress", "never"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	lines := readLines(t, out)
	uids := distinctUIDs(lines)
	if len(uids) != 5 {
		t.Errorf("got %d dashboards, want 5: %v", len(uids), uids)
	}
	if len(lines) != 10 {
		t.Errorf("got %d lines, want 10 (2 queries per dashboard)", len(lines))
	}
}

func TestPaginationCoversEveryDashboard(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(25)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--page-size", "10", "--concurrency", "4", "--progress", "never"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
}

func TestSplitsIntoSeveralFiles(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(25)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	dir := t.TempDir()
	out := filepath.Join(dir, "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--dashboards-per-file", "10", "--concurrency", "1", "--progress", "never"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	var all []string
	for i := 1; i <= 3; i++ {
		path := filepath.Join(dir, fmt.Sprintf("queries-%05d.txt", i))
		lines := readLines(t, path)
		uids := distinctUIDs(lines)

		wantDashboards := 10
		if i == 3 {
			wantDashboards = 5
		}
		if len(uids) != wantDashboards {
			t.Errorf("%s holds %d dashboards, want %d", path, len(uids), wantDashboards)
		}
		// Both queries of every dashboard must land in the same file.
		if len(lines) != wantDashboards*2 {
			t.Errorf("%s holds %d lines, want %d", path, len(lines), wantDashboards*2)
		}
		all = append(all, lines...)
	}

	if _, err := os.Stat(filepath.Join(dir, "queries-00004.txt")); err == nil {
		t.Error("a fourth file should not exist")
	}
	assertSameLines(t, all, testsupport.ExpectedLines(dashboards))
}

// TestResumeWithStartPageAndAppend covers the documented recipe for continuing
// an interrupted run without losing or skipping dashboards.
func TestResumeWithStartPageAndAppend(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(25)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	out := filepath.Join(t.TempDir(), "queries.txt")

	// A first run that only gets through the first page.
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--page-size", "10", "--max-dashboards", "10", "--progress", "never"); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, stderr)
	}
	if got := len(distinctUIDs(readLines(t, out))); got != 10 {
		t.Fatalf("first run covered %d dashboards, want 10", got)
	}

	// Resuming at the next page appends the rest.
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--page-size", "10", "--start-page", "2", "--append", "--progress", "never"); err != nil {
		t.Fatalf("resumed run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
}

// TestResumeWithSplitFilesContinuesNumbering covers the same recipe with split
// output: the resumed run has to number its files after the ones already there,
// or it would append dashboards to a file a consumer may have processed already.
func TestResumeWithSplitFilesContinuesNumbering(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(25)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	dir := t.TempDir()
	out := filepath.Join(dir, "queries.txt")

	split := []string{"--url", fake.URL, "-o", out, "--compress=false",
		"--dashboards-per-file", "4", "--page-size", "10", "--progress", "never"}

	// A first run that only gets through the first page fills two files and
	// leaves the third one half full.
	if stderr, err := runCLI(t, append(split, "--max-dashboards", "10")...); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, stderr)
	}
	third := filepath.Join(dir, "queries-00003.txt")
	beforeResume := readLines(t, third)
	if len(distinctUIDs(beforeResume)) != 2 {
		t.Fatalf("the first run left %d dashboards in %s, want 2",
			len(distinctUIDs(beforeResume)), third)
	}

	if stderr, err := runCLI(t, append(split, "--start-page", "2", "--append")...); err != nil {
		t.Fatalf("resumed run failed: %v\n%s", err, stderr)
	}

	if got := readLines(t, third); !slices.Equal(got, beforeResume) {
		t.Errorf("%s changed from %v to %v", third, beforeResume, got)
	}
	var all []string
	for i := 1; ; i++ {
		path := filepath.Join(dir, fmt.Sprintf("queries-%05d.txt", i))
		if _, err := os.Stat(path); err != nil {
			if i <= 4 {
				t.Fatalf("the resumed run wrote no %s: %v", path, err)
			}
			break
		}
		lines := readLines(t, path)
		if uids := distinctUIDs(lines); len(uids) > 4 {
			t.Errorf("%s holds %d dashboards, more than the limit of 4", path, len(uids))
		}
		all = append(all, lines...)
	}
	assertSameLines(t, all, testsupport.ExpectedLines(dashboards))
}

// TestViewerTokenFallback covers a token that may not read /api/datasources.
func TestViewerTokenFallback(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:        fixtures,
		DatasourcesStatus: http.StatusForbidden,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "--token", "viewer-token", "-o", out,
		"--compress=false", "--progress", "never", "--verbose")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(fixtures))
	if !strings.Contains(stderr, "/api/frontend/settings") {
		t.Errorf("expected a note about the fallback in verbose output:\n%s", stderr)
	}
}

func TestFailsWhenDatasourcesCannotBeRead(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:             testsupport.GeneratedFixtures(1),
		DatasourcesStatus:      http.StatusForbidden,
		FrontendSettingsStatus: http.StatusForbidden,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--progress", "never")
	if err == nil {
		t.Fatalf("expected the run to fail\n%s", stderr)
	}
	if !strings.Contains(err.Error(), "datasource") {
		t.Errorf("error should explain the datasource problem, got %v", err)
	}
}

func TestSkipsUnreadableDashboardsAndKeepsGoing(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(3)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards: dashboards,
		// The dashboard endpoint fails permanently for the first request only.
		FailTimes:  map[string]int{"/api/dashboards/uid/gen-0002": 10},
		FailStatus: http.StatusNotFound,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--concurrency", "1", "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	got := distinctUIDs(readLines(t, out))
	if !slices.Equal(got, []string{"gen-0001", "gen-0003"}) {
		t.Errorf("extracted %v, want gen-0001 and gen-0003", got)
	}
	if !strings.Contains(stderr, "failed dashboards") {
		t.Errorf("summary should report the failure:\n%s", stderr)
	}
}

func TestFailFastStopsOnFirstError(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards: testsupport.GeneratedFixtures(3),
		FailTimes:  map[string]int{"/api/dashboards/uid/gen-0001": 10},
		FailStatus: http.StatusNotFound,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--concurrency", "1", "--fail-fast", "--progress", "never")
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if !strings.Contains(err.Error(), "gen-0001") {
		t.Errorf("error should name the dashboard, got %v", err)
	}
	// A run cut short by an error is as resumable as an interrupted one.
	if !strings.Contains(stderr, "--start-page 1 --append") {
		t.Errorf("summary should say where to resume:\n%s", stderr)
	}
}

func TestRetriesTransientDashboardFailures(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(2)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards: dashboards,
		FailTimes:  map[string]int{"/api/dashboards/uid/gen-0001": 2},
		FailStatus: http.StatusTooManyRequests,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--retries", "5", "--progress", "never"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
}

func TestNoOutputFileWhenNothingMatches(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(2)})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false",
		"--datasource-types", "elasticsearch", "--include-unresolved=false", "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	if _, err := os.Stat(out); err == nil {
		t.Error("no output file should be created when no query matches")
	}
	if !strings.Contains(stderr, "no output files written") {
		t.Errorf("summary should say nothing was written:\n%s", stderr)
	}
}

func TestReportsSkippedDatasourceTypes(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	for _, want := range []string{"skipped by datasource", "loki", "cloudwatch"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("summary should mention %q:\n%s", want, stderr)
		}
	}
}

// Every reason a target did not make it into the output is worth reporting,
// since it is what answers "why is this dashboard missing from the file".
func TestSummaryAccountsForEveryTarget(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	for _, want := range []string{
		"panels visited", "targets seen", "annotation queries", "duplicates dropped",
		"empty expressions", "logs panels", "built-in datasources", "unresolved datasource",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("summary should report %q:\n%s", want, stderr)
		}
	}
}

func TestEnvironmentVariablesSupplyConnectionDetails(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	out := filepath.Join(t.TempDir(), "queries.txt")

	t.Setenv("GRAFANA_URL", fake.URL)
	t.Setenv("GRAFANA_TOKEN", "from-env")

	if stderr, err := runCLI(t, "-o", out, "--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(fixtures))
}

func TestRequiresURL(t *testing.T) {
	t.Setenv("GRAFANA_URL", "")
	_, err := runCLI(t, "--progress", "never")
	if err == nil {
		t.Fatal("expected an error without a URL")
	}
	if !strings.Contains(err.Error(), "GRAFANA_URL") {
		t.Errorf("error should mention the environment variable, got %v", err)
	}
}

// TestAnonymize checks the flag end to end: nothing recognizable from the
// dashboards survives, while the queries keep their shape and stay attributable
// to a dashboard.
func TestAnonymize(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.txt")
	anonymous := filepath.Join(dir, "anonymous.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", plain,
		"--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("plain run failed: %v\n%s", err, stderr)
	}
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", anonymous, "--anonymize",
		"--anonymize-salt", "a-secret", "--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("anonymized run failed: %v\n%s", err, stderr)
	}

	plainLines := readLines(t, plain)
	anonymousLines := readLines(t, anonymous)

	if len(plainLines) != len(anonymousLines) {
		t.Fatalf("got %d anonymized lines, want the %d of the plain run",
			len(anonymousLines), len(plainLines))
	}
	if len(distinctUIDs(anonymousLines)) != len(distinctUIDs(plainLines)) {
		t.Error("anonymizing changed how many dashboards the output covers")
	}

	// Nothing from the fixtures may show through: not a uid, not a metric name,
	// not a label, not a value, not a variable name.
	joined := strings.Join(anonymousLines, "\n")
	for _, secret := range []string{
		"fx-", "prom-main", "dup_metric", "multiline_metric", "annotated_requests_total",
		"kube_deployment_status_observed_generation", "DCGM_FI_DEV_GPU_TEMP",
		"namespace", "instance", "job", "$node", "gpu",
	} {
		if strings.Contains(joined, secret) {
			t.Errorf("%q survived anonymization", secret)
		}
	}

	// The shape has to survive, or the output is not worth sharing.
	for _, kept := range []string{"rate(", "sum by (", "[5m]", "$__rate_interval"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%q did not survive anonymization", kept)
		}
	}

	// A second run with the same salt has to produce the same file, so that
	// separate exports stay comparable.
	again := filepath.Join(dir, "again.txt")
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", again, "--anonymize",
		"--anonymize-salt", "a-secret", "--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("repeat run failed: %v\n%s", err, stderr)
	}
	assertSameLines(t, readLines(t, again), anonymousLines)

	// Without a shared salt the pseudonyms must differ.
	different := filepath.Join(dir, "different.txt")
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", different, "--anonymize",
		"--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("random salt run failed: %v\n%s", err, stderr)
	}
	if strings.Join(readLines(t, different), "\n") == joined {
		t.Error("a random salt produced the same pseudonyms as the fixed one")
	}
}

func TestAnonymizeSaltFromEnvironment(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(3)})
	dir := t.TempDir()
	fromEnv := filepath.Join(dir, "env.txt")
	fromFlag := filepath.Join(dir, "flag.txt")

	t.Setenv("GRAFANA_ANONYMIZE_SALT", "shared-secret")
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", fromEnv, "--anonymize",
		"--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("run with the environment salt failed: %v\n%s", err, stderr)
	}

	t.Setenv("GRAFANA_ANONYMIZE_SALT", "")
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", fromFlag, "--anonymize",
		"--anonymize-salt", "shared-secret", "--compress=false", "--progress", "never"); err != nil {
		t.Fatalf("run with the flag salt failed: %v\n%s", err, stderr)
	}

	assertSameLines(t, readLines(t, fromEnv), readLines(t, fromFlag))
}

func TestRejectsInvalidFlags(t *testing.T) {
	tests := [][]string{
		{"--url", "http://localhost", "--page-size", "99999"},
		{"--url", "http://localhost", "--concurrency", "0"},
		{"--url", "http://localhost", "--max-dashboards", "-1"},
		{"--url", "http://localhost", "--datasource-types", ""},
		{"--url", "http://localhost", "--progress", "sometimes"},
		{"--url", "http://localhost", "-o", ""},
		// A salt without anonymizing is a misunderstanding worth reporting.
		{"--url", "http://localhost", "--anonymize-salt", "x"},
		// Appending with a random salt would mix two sets of pseudonyms.
		{"--url", "http://localhost", "--anonymize", "--append"},
	}
	for _, args := range tests {
		if _, err := runCLI(t, args...); err == nil {
			t.Errorf("%v should have been rejected", args)
		}
	}
}

func TestProgressWritesToStderr(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(3)})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--progress", "always", "--compress=false")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "dashboards") {
		t.Errorf("expected progress output on stderr:\n%s", stderr)
	}
}

func distinctUIDs(lines []string) []string {
	seen := map[string]bool{}
	var uids []string
	for _, line := range lines {
		uid, _, _ := strings.Cut(line, ";")
		if !seen[uid] {
			seen[uid] = true
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)
	return uids
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return splitLines(string(data))
}

func readGzipLines(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("reading %s as gzip: %v", path, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompressing %s: %v", path, err)
	}
	return splitLines(string(data))
}

func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertSameLines compares ignoring order, since dashboards are fetched
// concurrently.
func assertSameLines(t *testing.T, got, want []string) {
	t.Helper()

	gotSorted := slices.Clone(got)
	wantSorted := slices.Clone(want)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)

	if slices.Equal(gotSorted, wantSorted) {
		return
	}
	t.Errorf("output mismatch\nmissing: %v\nunexpected: %v",
		difference(wantSorted, gotSorted), difference(gotSorted, wantSorted))
}

func difference(a, b []string) []string {
	inB := make(map[string]int, len(b))
	for _, s := range b {
		inB[s]++
	}
	var diff []string
	for _, s := range a {
		if inB[s] > 0 {
			inB[s]--
			continue
		}
		diff = append(diff, s)
	}
	return diff
}
