//go:build integration

package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/cli"
	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

// generatedDashboards is chosen so that pagination and file splitting both need
// several pages and files at the page sizes used below.
const generatedDashboards = 25

// TestExtractor runs every case against a single Grafana container, since
// starting one costs far more than the extraction itself.
func TestExtractor(t *testing.T) {
	instance := Start(t, generatedDashboards)

	t.Run("AdminBasicAuth", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		stderr := run(t, instance, out,
			"--user", instance.AdminUser(),
			"--password", instance.AdminPassword(),
			"--compress=false")

		assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(instance.All()))
		if !strings.Contains(stderr, "queries written") {
			t.Errorf("missing summary:\n%s", stderr)
		}
	})

	t.Run("ViewerServiceAccountToken", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		stderr := run(t, instance, out,
			"--token", instance.ViewerToken,
			"--compress=false",
			"--verbose")

		assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(instance.All()))
		t.Log(datasourceSourceLine(stderr))
	})

	// Proves that the real /api/frontend/settings payload carries enough
	// information to classify every query, for instances where the token may not
	// read /api/datasources.
	t.Run("FallsBackToFrontendSettings", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")

		stderr, err := execute(t, "--url", instance.URLWithoutDatasourcesAPI(t),
			"-o", out, "--progress", "never", "--compress=false", "--verbose",
			"--token", instance.ViewerToken)
		if err != nil {
			t.Fatalf("run failed: %v\n%s", err, stderr)
		}

		assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(instance.All()))
		if !strings.Contains(stderr, "/api/frontend/settings") {
			t.Errorf("expected the fallback to be reported:\n%s", stderr)
		}
	})

	// Resolution paths that only a real instance can confirm: the provisioned
	// default datasource, a reference by name, and a dangling uid.
	t.Run("DatasourceResolution", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, out, "--compress=false")
		lines := readLines(t, out)

		for _, name := range []string{
			"default-datasource",
			"datasource-by-name",
			"datasource-variable",
			"mixed-datasource",
			"nested-row-panels",
			"real-dcgm-exporter",
			"real-legacy-rows",
		} {
			fixture := findFixture(t, instance, name)
			assertSameLines(t, linesFor(lines, fixture.UID), fixture.Expected)
		}
	})

	t.Run("ExcludesNonPrometheusQueries", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, out, "--compress=false")
		content := strings.Join(readLines(t, out), "\n")

		for _, unwanted := range []string{
			"should_not_appear_logs_panel",
			"should_not_appear_cloudwatch",
			"should_not_appear_dashboard_ds",
			"should_not_appear_grafana_ds",
			`|= "error"`,
		} {
			if strings.Contains(content, unwanted) {
				t.Errorf("output contains %q, which is not a Prometheus query", unwanted)
			}
		}
	})

	t.Run("CompressesByDefault", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, out)

		if _, err := os.Stat(out); err == nil {
			t.Error("the uncompressed path should not exist")
		}
		assertSameLines(t, readGzipLines(t, out+".gz"), testsupport.ExpectedLines(instance.All()))
	})

	t.Run("Pagination", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, out, "--compress=false", "--page-size", "3", "--concurrency", "4")

		assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(instance.All()))
	})

	t.Run("MaxDashboards", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, out, "--compress=false", "--max-dashboards", "5", "--page-size", "2")

		// A dashboard without queries contributes no lines, so at most five
		// dashboards may appear.
		if uids := distinctUIDs(readLines(t, out)); len(uids) > 5 {
			t.Errorf("got %d dashboards, want at most 5: %v", len(uids), uids)
		}
	})

	t.Run("SplitsIntoSeveralFiles", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "queries.txt")
		run(t, instance, out, "--compress=false", "--dashboards-per-file", "5", "--concurrency", "1")

		files, err := filepath.Glob(filepath.Join(dir, "queries-*.txt"))
		if err != nil {
			t.Fatalf("globbing output: %v", err)
		}
		if len(files) < 2 {
			t.Fatalf("got %d files, want several", len(files))
		}
		sort.Strings(files)

		var all []string
		seen := map[string]string{}
		for _, path := range files {
			lines := readLines(t, path)
			uids := distinctUIDs(lines)
			if len(uids) > 5 {
				t.Errorf("%s holds %d dashboards, want at most 5", filepath.Base(path), len(uids))
			}
			for _, uid := range uids {
				if other, ok := seen[uid]; ok {
					t.Errorf("dashboard %s appears in both %s and %s: a dashboard must not span files",
						uid, filepath.Base(other), filepath.Base(path))
				}
				seen[uid] = path
			}
			all = append(all, lines...)
		}
		assertSameLines(t, all, testsupport.ExpectedLines(instance.All()))
	})

	t.Run("FolderFilter", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, out, "--compress=false", "--folder-uid", FolderUID)

		assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(instance.Generated))
	})

	t.Run("IncludeUnresolvedDisabled", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, out, "--compress=false", "--include-unresolved=false")
		content := strings.Join(readLines(t, out), "\n")

		if strings.Contains(content, "unresolved_metric") {
			t.Error("a dangling datasource reference should be dropped with --include-unresolved=false")
		}
		// Everything with a resolvable Prometheus datasource still comes through.
		fixture := findFixture(t, instance, "basic-prometheus")
		assertSameLines(t, linesFor(readLines(t, out), fixture.UID), fixture.Expected)
	})

	// Grafana 12 serves whole dashboards in pages, and returns them migrated to
	// the current schema rather than as stored. Only a real instance can show
	// that this yields the same queries as fetching each dashboard by uid.
	t.Run("BulkListing", func(t *testing.T) {
		perDashboard := filepath.Join(t.TempDir(), "queries.txt")
		run(t, instance, perDashboard, "--compress=false", "--bulk", "off")

		bulk := filepath.Join(t.TempDir(), "queries.txt")
		stderr, err := execute(t, "--url", instance.URL, "-o", bulk, "--progress", "never",
			"--user", instance.AdminUser(), "--password", instance.AdminPassword(),
			"--compress=false", "--bulk", "on", "--verbose")
		if err != nil {
			if strings.Contains(err.Error(), "Grafana 12") {
				t.Skipf("this Grafana does not serve dashboards in bulk: %v", err)
			}
			t.Fatalf("bulk run failed: %v\n%s", err, stderr)
		}

		assertSameLines(t, readLines(t, bulk), readLines(t, perDashboard))
		assertSameLines(t, readLines(t, bulk), testsupport.ExpectedLines(instance.All()))
	})

	// Whichever strategy a release supports, the default has to produce the
	// documented output without being told which one to use.
	t.Run("BulkAuto", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		stderr := run(t, instance, out, "--compress=false", "--verbose", "--bulk", "auto")

		assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(instance.All()))
		t.Logf("strategy: %s", strategyLine(stderr))
	})

	t.Run("RejectsBadCredentials", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "queries.txt")
		_, err := execute(t, "--url", instance.URL, "-o", out,
			"--user", "admin", "--password", "wrong-password",
			"--progress", "never")
		if err == nil {
			t.Fatal("expected the run to fail with bad credentials")
		}
	})
}

// strategyLine picks the verbose line naming how dashboards were enumerated,
// which depends on the Grafana version under test.
func strategyLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "dashboards one by one") || strings.Contains(line, "in pages of up to") {
			return strings.TrimSpace(line)
		}
		if strings.Contains(line, "pages cannot honor") {
			return strings.TrimSpace(line)
		}
	}
	return "not reported"
}

// datasourceSourceLine picks the verbose line naming the endpoint the
// datasource types came from, which differs by Grafana version and token role.
func datasourceSourceLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "datasources from") {
			return strings.TrimSpace(line)
		}
	}
	return "no datasource source reported"
}

// run executes the extractor against the instance and returns its stderr.
func run(t *testing.T, instance *Instance, out string, extra ...string) string {
	t.Helper()

	args := []string{"--url", instance.URL, "-o", out, "--progress", "never"}
	if !slices.ContainsFunc(extra, func(arg string) bool {
		return arg == "--token" || arg == "--user"
	}) {
		args = append(args, "--user", instance.AdminUser(), "--password", instance.AdminPassword())
	}
	args = append(args, extra...)

	stderr, err := execute(t, args...)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	return stderr
}

func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := cli.NewRootCmd()
	var stderr bytes.Buffer
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"extract"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stderr.String(), err
}

func findFixture(t *testing.T, instance *Instance, name string) testsupport.Fixture {
	t.Helper()
	for _, fixture := range instance.All() {
		if fixture.Name == name {
			return fixture
		}
	}
	t.Fatalf("fixture %q not provisioned", name)
	return testsupport.Fixture{}
}

// linesFor returns the output lines belonging to one dashboard.
func linesFor(lines []string, uid string) []string {
	var out []string
	for _, line := range lines {
		if lineUID, _, _ := strings.Cut(line, ";"); lineUID == uid {
			out = append(out, line)
		}
	}
	return out
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

func assertSameLines(t *testing.T, got, want []string) {
	t.Helper()

	gotSorted := slices.Clone(got)
	wantSorted := slices.Clone(want)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)

	if slices.Equal(gotSorted, wantSorted) {
		return
	}
	t.Errorf("output mismatch\nmissing (%d): %s\nunexpected (%d): %s",
		len(difference(wantSorted, gotSorted)), format(difference(wantSorted, gotSorted)),
		len(difference(gotSorted, wantSorted)), format(difference(gotSorted, wantSorted)))
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

func format(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	if len(lines) > 10 {
		return "\n  " + strings.Join(lines[:10], "\n  ") + fmt.Sprintf("\n  ... and %d more", len(lines)-10)
	}
	return "\n  " + strings.Join(lines, "\n  ")
}
