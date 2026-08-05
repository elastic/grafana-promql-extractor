//go:build corpus

package corpus

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/cli"
	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

// Thresholds guard against regressions. They sit below what the top 1000
// dashboards currently achieve, with room for the churn in that list, so that
// a failure means the extractor got worse rather than that grafana.com changed.
const (
	// minPromQLParseRate is the share of extracted queries that must parse as
	// PromQL once Grafana variables are interpolated. Measured: 98.7%, where
	// every failure but one comes from a variable in a position no fixed
	// substitution can fill, such as a function name.
	minPromQLParseRate = 0.97
	// maxLogQLLeakRate is the share of extracted queries that may contain a
	// pipe, which PromQL does not have. Measured: 0.007%, two queries from a
	// dashboard that declares a Prometheus datasource variable but holds LogQL.
	maxLogQLLeakRate = 0.001
	// minPrometheusRecall is the share of dashboards that grafana.com lists
	// under Prometheus from which at least one query must be extracted.
	// Measured: 100%.
	minPrometheusRecall = 0.95
	// maxSilentMisses is the share of expressions in a place Grafana executes
	// that may be dropped without an explanation. Measured: none.
	maxSilentMisses = 0.0
)

// TestCorpus extracts from the most downloaded community dashboards on
// grafana.com and checks the result against independent signals: a PromQL
// parser, the datasource grafana.com lists each dashboard under, and a
// schema-agnostic walk of the raw documents.
func TestCorpus(t *testing.T) {
	opts := OptionsFromEnv(t)
	dashboards := Load(t, opts)
	// Guard against a half-finished download quietly weakening every check
	// below. Some dashboards are always gone from grafana.com by now.
	if minimum := opts.Dashboards * 9 / 10; len(dashboards) < minimum {
		t.Fatalf("only %d of the %d requested dashboards could be loaded, expected at least %d",
			len(dashboards), opts.Dashboards, minimum)
	}

	extracted, stderr := extractAll(t, opts, dashboards, "extracted-queries.txt")
	t.Logf("extractor summary:\n%s", indent(stderr))

	report := analyze(dashboards, extracted)
	writeReport(t, opts, report)
	t.Log("\n" + report.String())

	t.Run("EveryDashboardWasProcessed", func(t *testing.T) {
		if strings.Contains(stderr, "failed dashboards") {
			t.Errorf("the run reported dashboard failures:\n%s", stderr)
		}
		if report.DashboardsWithQueries == 0 {
			t.Fatal("no queries were extracted from the whole corpus")
		}
	})

	t.Run("OutputIsWellFormed", func(t *testing.T) {
		for _, problem := range report.Malformed {
			t.Errorf("malformed output: %s", problem)
		}
	})

	t.Run("QueriesParseAsPromQL", func(t *testing.T) {
		rate := ratio(report.Parsed, report.Queries)
		t.Logf("%d of %d queries parse as PromQL (%.2f%%)", report.Parsed, report.Queries, rate*100)
		if rate < minPromQLParseRate {
			t.Errorf("parse rate %.2f%% is below the %.2f%% threshold; see the report for the failures",
				rate*100, minPromQLParseRate*100)
		}
	})

	t.Run("NoLogQLLeaked", func(t *testing.T) {
		rate := ratio(len(report.LogQLLeaks), report.Queries)
		if rate > maxLogQLLeakRate {
			t.Errorf("%d of %d extracted queries look like LogQL (%.3f%%), above the %.3f%% threshold",
				len(report.LogQLLeaks), report.Queries, rate*100, maxLogQLLeakRate*100)
			for _, leak := range firstN(report.LogQLLeaks, 10) {
				t.Errorf("  %s", leak)
			}
		}
	})

	t.Run("PrometheusDashboardsYieldQueries", func(t *testing.T) {
		stats, ok := report.ByDatasource["Prometheus"]
		if !ok {
			t.Skip("no dashboard in this corpus is listed under Prometheus")
		}
		rate := ratio(stats.DashboardsWithQueries, stats.Dashboards)
		t.Logf("%d of %d dashboards listed under Prometheus yielded queries (%.1f%%)",
			stats.DashboardsWithQueries, stats.Dashboards, rate*100)
		if rate < minPrometheusRecall {
			t.Errorf("recall %.1f%% is below the %.1f%% threshold; see the report for the dashboards that yielded nothing",
				rate*100, minPrometheusRecall*100)
		}
	})

	// Every expression Grafana would execute, in a panel target or in an
	// annotation, either comes out or is dropped for an explicable reason.
	t.Run("NoExecutableExpressionsLostSilently", func(t *testing.T) {
		rate := ratio(len(report.UnexplainedMisses), report.ExecutableExprs)
		if rate > maxSilentMisses {
			t.Errorf("%d of %d executable expressions went missing without an explanation (%.2f%%)",
				len(report.UnexplainedMisses), report.ExecutableExprs, rate*100)
			for _, miss := range firstN(report.UnexplainedMisses, 10) {
				t.Errorf("  %s", miss)
			}
		}
	})
}

// extractAll serves the corpus from a fake Grafana and runs the real command
// against it, so the whole pipeline is exercised, not just the extractor.
func extractAll(t *testing.T, opts Options, dashboards []Dashboard, name string, extraArgs ...string) (map[string][]string, string) {
	t.Helper()

	fixtures := make([]testsupport.Fixture, 0, len(dashboards))
	for _, dashboard := range dashboards {
		fixtures = append(fixtures, testsupport.Fixture{
			Name:  dashboard.Slug,
			UID:   dashboard.UID(),
			Title: dashboard.Name,
			JSON:  dashboard.JSON,
		})
	}

	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	out := filepath.Join(opts.CacheDir, name)

	cmd := cli.NewRootCmd()
	var stderr bytes.Buffer
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"extract",
		"--url", fake.URL,
		"-o", out,
		"--compress=false",
		"--progress", "never",
		"--page-size", "500",
	}, extraArgs...))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("extraction failed: %v\n%s", err, stderr.String())
	}
	t.Logf("extracted queries written to %s", out)

	return readQueries(t, out), stderr.String()
}

func readQueries(t *testing.T, path string) map[string][]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	queries := make(map[string][]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		uid, query, _ := strings.Cut(line, ";")
		queries[uid] = append(queries[uid], query)
	}
	return queries
}

// Report holds everything the corpus run learned.
type Report struct {
	Dashboards            int
	DashboardsWithQueries int
	Queries               int
	DistinctQueries       int
	Parsed                int
	// ExecutableExprs counts the expressions Grafana would run: panel targets
	// and annotations.
	ExecutableExprs int

	// ByDatasource groups dashboards by the datasource grafana.com lists them
	// under.
	ByDatasource map[string]*DatasourceStats

	// ExprsByLocation counts the expressions found in the raw documents,
	// grouped by where in the document they live.
	ExprsByLocation map[string]int
	// ExtractedByLocation counts how many of those the extractor produced.
	ExtractedByLocation map[string]int
	// ExcusedMisses counts the expressions left out for a known reason.
	ExcusedMisses map[string]int
	// FailuresWithoutVariables counts the queries that fail to parse even
	// though they contain no Grafana variable, which interpolation could
	// therefore not have broken.
	FailuresWithoutVariables int

	ParseFailures     []string
	LogQLLeaks        []string
	Malformed         []string
	UnexplainedMisses []string
	EmptyPrometheus   []string
}

// DatasourceStats summarizes one grafana.com datasource category.
type DatasourceStats struct {
	Dashboards            int
	DashboardsWithQueries int
	Queries               int
}

func analyze(dashboards []Dashboard, extracted map[string][]string) *Report {
	report := &Report{
		Dashboards:          len(dashboards),
		ByDatasource:        map[string]*DatasourceStats{},
		ExprsByLocation:     map[string]int{},
		ExtractedByLocation: map[string]int{},
		ExcusedMisses:       map[string]int{},
	}
	distinct := map[string]struct{}{}

	for _, dashboard := range dashboards {
		queries := extracted[dashboard.UID()]

		category := dashboard.Datasource
		if category == "" {
			category = "(unset)"
		}
		stats := report.ByDatasource[category]
		if stats == nil {
			stats = &DatasourceStats{}
			report.ByDatasource[category] = stats
		}
		stats.Dashboards++
		stats.Queries += len(queries)
		if len(queries) > 0 {
			stats.DashboardsWithQueries++
			report.DashboardsWithQueries++
		} else if category == "Prometheus" {
			report.EmptyPrometheus = append(report.EmptyPrometheus,
				fmt.Sprintf("%s %q", dashboard.URL(), dashboard.Name))
		}

		report.Queries += len(queries)
		for _, query := range queries {
			distinct[query] = struct{}{}
			checkQuery(report, dashboard, query)
		}

		compareWithRawDocument(report, dashboard, queries)
	}

	report.DistinctQueries = len(distinct)
	return report
}

// checkQuery validates a single extracted query.
func checkQuery(report *Report, dashboard Dashboard, query string) {
	switch {
	case query == "":
		report.Malformed = append(report.Malformed, fmt.Sprintf("%s: empty query", dashboard.URL()))
	case strings.ContainsAny(query, "\n\r"):
		report.Malformed = append(report.Malformed,
			fmt.Sprintf("%s: query spans several lines: %q", dashboard.URL(), query))
	}

	if LooksLikeLogQL(query) {
		report.LogQLLeaks = append(report.LogQLLeaks,
			fmt.Sprintf("%s [%s]: %s", dashboard.URL(), dashboard.Datasource, truncate(query, 120)))
	}

	if err := ParsesAsPromQL(query); err != nil {
		note := ""
		if Interpolate(query) == query {
			// No variable was substituted, so the expression is broken on its
			// own rather than by the interpolation this check applies.
			report.FailuresWithoutVariables++
			note = " [no variables]"
		}
		report.ParseFailures = append(report.ParseFailures,
			fmt.Sprintf("%s [%s]%s: %s\n      %v",
				dashboard.URL(), dashboard.Datasource, note, truncate(query, 160), err))
		return
	}
	report.Parsed++
}

// compareWithRawDocument cross-checks the extracted queries against a
// schema-agnostic walk of the document, to see what was left behind and where
// it lived.
func compareWithRawDocument(report *Report, dashboard Dashboard, queries []string) {
	produced := make(map[string]int, len(queries))
	for _, query := range queries {
		produced[query]++
	}

	for _, found := range FindExprs(dashboard.JSON) {
		location := classify(found.Location)
		report.ExprsByLocation[location]++

		executable := location == "panel target" || location == "annotation"
		if executable {
			report.ExecutableExprs++
		}

		if produced[found.Expr] > 0 {
			produced[found.Expr]--
			report.ExtractedByLocation[location]++
			continue
		}
		// Repeated expressions are deduplicated on purpose, so only an
		// expression that appears nowhere in the output is a candidate miss.
		if executable && !containsQuery(queries, found.Expr) {
			if reason := skipReason(dashboard, found.Expr); reason != "" {
				report.ExcusedMisses[reason]++
			} else {
				report.UnexplainedMisses = append(report.UnexplainedMisses,
					fmt.Sprintf("%s [%s]: %s", dashboard.URL(), location, truncate(found.Expr, 140)))
			}
		}
	}
}

// classify turns a document path into a short category. Only a chain of panels
// and rows ending in targets counts as a panel target: dashboards also carry
// leftover state from panel plugins under keys such as "CustomPanel", which
// Grafana itself never queries.
func classify(location string) string {
	segments := strings.Split(location, "/")
	last := segments[len(segments)-1]

	switch {
	case last == "targets" && isPanelPath(segments[:len(segments)-1]):
		return "panel target"
	case location == "annotations/list" || location == "annotations/list/target":
		return "annotation"
	case strings.HasPrefix(location, "spec/elements"):
		// The v2 dashboard schema, which the classic API this tool uses does
		// not serve.
		return "dashboard schema v2"
	case strings.HasPrefix(location, "__elements"):
		return "library panel definition"
	case strings.Contains(location, "alert"):
		return "legacy alert rule"
	case strings.Contains(location, "templating"):
		return "template variable"
	default:
		return "other: " + location
	}
}

func isPanelPath(segments []string) bool {
	if len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		if segment != "panels" && segment != "rows" {
			return false
		}
	}
	return true
}

// skipReason explains why an expression may legitimately be absent from the
// output. An empty string means the omission is unexplained and therefore a
// candidate bug.
func skipReason(dashboard Dashboard, expr string) string {
	switch {
	case LooksLikeLogQL(expr):
		return "log query, it has a pipe"
	case HasBareStreamSelector(expr):
		return "log query, it selects no metric"
	case ParsesAsPromQL(expr) != nil:
		// Whatever this is, leaving it out cannot be a missed PromQL query.
		return "not PromQL"
	case dashboard.Datasource != "" && !strings.Contains(dashboard.Datasource, "Prometheus"):
		return "grafana.com lists the dashboard under " + dashboard.Datasource
	}
	return ""
}

func containsQuery(queries []string, expr string) bool {
	for _, query := range queries {
		if query == expr {
			return true
		}
	}
	return false
}

func (r *Report) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "Corpus report\n")
	fmt.Fprintf(&b, "  dashboards:            %d (%d yielded queries)\n", r.Dashboards, r.DashboardsWithQueries)
	fmt.Fprintf(&b, "  queries:               %d (%d distinct)\n", r.Queries, r.DistinctQueries)
	fmt.Fprintf(&b, "  parse as PromQL:       %d (%.2f%%)\n", r.Parsed, ratio(r.Parsed, r.Queries)*100)
	fmt.Fprintf(&b, "  of the rest, broken without a variable to blame: %d\n", r.FailuresWithoutVariables)
	fmt.Fprintf(&b, "  look like LogQL:       %d\n", len(r.LogQLLeaks))
	fmt.Fprintf(&b, "  malformed lines:       %d\n", len(r.Malformed))

	fmt.Fprintf(&b, "\nExpressions in the raw documents, by location:\n")
	for _, location := range sortedKeys(r.ExprsByLocation) {
		total := r.ExprsByLocation[location]
		fmt.Fprintf(&b, "  %-26s found %6d, extracted %6d (%.1f%%)\n",
			location, total, r.ExtractedByLocation[location],
			ratio(r.ExtractedByLocation[location], total)*100)
	}

	fmt.Fprintf(&b, "\nExecutable expressions left out, by reason:\n")
	for _, reason := range sortedKeys(r.ExcusedMisses) {
		fmt.Fprintf(&b, "  %-42s %6d\n", reason, r.ExcusedMisses[reason])
	}
	fmt.Fprintf(&b, "  %-42s %6d\n", "unexplained", len(r.UnexplainedMisses))

	fmt.Fprintf(&b, "\nDashboards by the datasource grafana.com lists them under:\n")
	for _, category := range sortedByDashboards(r.ByDatasource) {
		stats := r.ByDatasource[category]
		fmt.Fprintf(&b, "  %-22s %4d dashboards, %4d yielded queries (%.0f%%), %6d queries\n",
			category, stats.Dashboards, stats.DashboardsWithQueries,
			ratio(stats.DashboardsWithQueries, stats.Dashboards)*100, stats.Queries)
	}

	section(&b, "Queries that do not parse as PromQL", r.ParseFailures, 30)
	section(&b, "Queries that look like LogQL", r.LogQLLeaks, 20)
	section(&b, "Executable expressions missing without explanation", r.UnexplainedMisses, 30)
	section(&b, "Dashboards listed under Prometheus that yielded nothing", r.EmptyPrometheus, 30)

	return b.String()
}

func section(b *strings.Builder, title string, entries []string, limit int) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s (%d):\n", title, len(entries))
	for _, entry := range firstN(entries, limit) {
		fmt.Fprintf(b, "  - %s\n", entry)
	}
	if len(entries) > limit {
		fmt.Fprintf(b, "  ... and %d more\n", len(entries)-limit)
	}
}

func writeReport(t *testing.T, opts Options, report *Report) {
	t.Helper()
	path := filepath.Join(opts.CacheDir, "report.txt")
	if err := os.WriteFile(path, []byte(report.String()), 0o644); err != nil {
		t.Fatalf("writing report: %v", err)
	}
	t.Logf("full report written to %s", path)
}

func ratio(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func firstN(entries []string, n int) []string {
	if len(entries) > n {
		return entries[:n]
	}
	return entries
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

func sortedByDashboards(m map[string]*DatasourceStats) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]].Dashboards != m[keys[j]].Dashboards {
			return m[keys[i]].Dashboards > m[keys[j]].Dashboards
		}
		return keys[i] < keys[j]
	})
	return keys
}
