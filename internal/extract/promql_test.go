package extract_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/extract"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/testsupport"
)

var update = flag.Bool("update", false, "rewrite the .expected files of the dashboard fixtures")

// defaultExtractor mirrors the extractor the CLI builds with default flags.
func defaultExtractor() *extract.Extractor {
	return &extract.Extractor{
		Lookup:            testsupport.Registry(),
		Allowed:           extract.NewTypeSet(extract.DefaultDatasourceTypes),
		IncludeUnresolved: true,
		Dedupe:            true,
	}
}

func lines(uid string, queries []string) []string {
	out := make([]string, 0, len(queries))
	for _, q := range queries {
		out = append(out, uid+";"+q)
	}
	return out
}

func TestExtractFixtures(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures found")
	}

	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			env, err := extract.ParseEnvelope(bytes.NewReader(fixture.JSON))
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}

			result := defaultExtractor().Extract(env)
			got := lines(result.UID, result.Queries)

			if *update {
				writeGolden(t, fixture.Name, got)
				return
			}
			assertLines(t, got, fixture.Expected)
		})
	}
}

// TestExtractFixturesDocumentShape re-parses each fixture wrapped in the
// /api/dashboards/uid/:uid envelope, which is how the CLI sees them.
func TestExtractFixturesInEnvelope(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}

	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			body := `{"meta":{"folderTitle":"General"},"dashboard":` + string(fixture.JSON) + `}`
			env, err := extract.ParseEnvelope(strings.NewReader(body))
			if err != nil {
				t.Fatalf("parsing envelope: %v", err)
			}
			result := defaultExtractor().Extract(env)
			assertLines(t, lines(result.UID, result.Queries), fixture.Expected)
		})
	}
}

func TestIncludeUnresolvedDisabled(t *testing.T) {
	fixture := findFixture(t, "unknown-datasource")

	extractor := defaultExtractor()
	extractor.IncludeUnresolved = false

	env, err := extract.ParseEnvelope(bytes.NewReader(fixture.JSON))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	result := extractor.Extract(env)

	if len(result.Queries) != 0 {
		t.Errorf("expected no queries with IncludeUnresolved disabled, got %v", result.Queries)
	}
	if result.Stats.UnresolvedSkipped != 2 {
		t.Errorf("UnresolvedSkipped = %d, want 2", result.Stats.UnresolvedSkipped)
	}
}

func TestDedupeDisabled(t *testing.T) {
	fixture := findFixture(t, "duplicates-and-multiline")

	extractor := defaultExtractor()
	extractor.Dedupe = false

	env, err := extract.ParseEnvelope(bytes.NewReader(fixture.JSON))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	result := extractor.Extract(env)

	want := []string{
		"sum(rate(dup_metric[5m]))",
		"sum(rate(dup_metric[5m]))",
		"sum(rate(dup_metric[5m]))",
		`sum by (job) ( rate( multiline_metric{code=~"5.."}[5m] ) )`,
		`sum(commented_metric_total) - sum(commented_metric_total{plan="free"})`,
		`hash_in_a_label{path="/#/dashboards"}`,
	}
	assertLines(t, result.Queries, want)
	if result.Stats.Duplicates != 0 {
		t.Errorf("Duplicates = %d, want 0 when deduplication is off", result.Stats.Duplicates)
	}
}

func TestAllowlistControlsDatasourceTypes(t *testing.T) {
	fixture := findFixture(t, "loki-and-logs")

	extractor := defaultExtractor()
	extractor.Allowed = extract.NewTypeSet([]string{"loki"})
	extractor.IncludeUnresolved = false

	env, err := extract.ParseEnvelope(bytes.NewReader(fixture.JSON))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	result := extractor.Extract(env)

	assertLines(t, result.Queries, []string{`sum(rate({app="api"} |= "error" [5m]))`})
	if got := result.Stats.SkippedByType["prometheus"]; got != 1 {
		t.Errorf("SkippedByType[prometheus] = %d, want 1", got)
	}
}

func TestSkippedByTypeStats(t *testing.T) {
	fixture := findFixture(t, "non-prometheus-datasource")

	env, err := extract.ParseEnvelope(bytes.NewReader(fixture.JSON))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	result := defaultExtractor().Extract(env)

	if got := result.Stats.SkippedByType["cloudwatch"]; got != 1 {
		t.Errorf("SkippedByType[cloudwatch] = %d, want 1", got)
	}
	if result.Stats.Queries != 1 {
		t.Errorf("Queries = %d, want 1", result.Stats.Queries)
	}
}

func TestLogsAndSpecialDatasourceStats(t *testing.T) {
	logs := findFixture(t, "loki-and-logs")
	env, err := extract.ParseEnvelope(bytes.NewReader(logs.JSON))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	result := defaultExtractor().Extract(env)
	if result.Stats.SkippedLogs != 1 {
		t.Errorf("SkippedLogs = %d, want 1", result.Stats.SkippedLogs)
	}

	special := findFixture(t, "special-datasources")
	env, err = extract.ParseEnvelope(bytes.NewReader(special.JSON))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	result = defaultExtractor().Extract(env)
	// The expression target plus the -- Dashboard -- and -- Grafana -- panels.
	if result.Stats.SkippedSpecial != 3 {
		t.Errorf("SkippedSpecial = %d, want 3", result.Stats.SkippedSpecial)
	}
}

func TestExtractWithoutRegistry(t *testing.T) {
	// Without a datasource registry only references that carry their own type
	// resolve; the rest fall back to the unresolved heuristic.
	extractor := &extract.Extractor{
		Allowed:           extract.NewTypeSet(extract.DefaultDatasourceTypes),
		IncludeUnresolved: false,
		Dedupe:            true,
	}
	fixture := findFixture(t, "datasource-by-name")

	env, err := extract.ParseEnvelope(bytes.NewReader(fixture.JSON))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	if result := extractor.Extract(env); len(result.Queries) != 0 {
		t.Errorf("expected no queries without a registry, got %v", result.Queries)
	}
}

func TestParseEnvelopeTolerateTypeErrors(t *testing.T) {
	// A dashboard where a datasource is an array and expr is a number: the
	// former is ignored, the latter reported as a partial decode.
	body := `{"dashboard":{"uid":"weird","title":"Weird","panels":[
		{"type":"timeseries","datasource":["not","an","object"],"targets":[
			{"refId":"A","expr":"weird_metric"},
			{"refId":"B","expr":42}
		]}
	]}}`

	env, err := extract.ParseEnvelope(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected a partial decode error")
	}
	if !strings.Contains(err.Error(), "type errors") {
		t.Fatalf("unexpected error: %v", err)
	}
	if env == nil {
		t.Fatal("expected a partial dashboard alongside the error")
	}

	result := defaultExtractor().Extract(env)
	// The unparseable datasource is treated as absent, so the default
	// datasource applies and the well-formed expression still comes through.
	assertLines(t, result.Queries, []string{"weird_metric"})
}

func TestParseEnvelopeRejectsGarbage(t *testing.T) {
	if _, err := extract.ParseEnvelope(strings.NewReader("not json at all")); err == nil {
		t.Fatal("expected an error for malformed json")
	}
}

func TestNormalizationKeepsSemicolonsParseable(t *testing.T) {
	// A semicolon inside a label value is legal PromQL. The output format stays
	// parseable because consumers split on the first separator only.
	body := `{"dashboard":{"uid":"semi","panels":[{"type":"timeseries",
		"datasource":{"type":"prometheus","uid":"prom-main"},
		"targets":[{"refId":"A","expr":"metric{path=\"a;b\"}"}]}]}}`

	env, err := extract.ParseEnvelope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	result := defaultExtractor().Extract(env)
	assertLines(t, result.Queries, []string{`metric{path="a;b"}`})

	line := result.UID + ";" + result.Queries[0]
	uid, query, _ := strings.Cut(line, ";")
	if uid != "semi" || query != `metric{path="a;b"}` {
		t.Errorf("cutting on the first separator gave %q / %q", uid, query)
	}
}

// A PromQL comment reaches to the end of its line. Since the output format
// puts every query on one line, a comment has to be removed rather than folded
// into the line below it, which would comment out the rest of the expression.
func TestNormalizationDropsComments(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "comment between the operands of a multi-line expression",
			expr: "sum(total)\n# keep only the paid plans\n- sum(free)",
			want: "sum(total) - sum(free)",
		},
		{
			name: "trailing comment",
			expr: "up == 1 # is it up?",
			want: "up == 1",
		},
		{
			name: "hash inside a label value is not a comment",
			expr: `metric{path="/#/home", other="a#b"}`,
			want: `metric{path="/#/home", other="a#b"}`,
		},
		{
			name: "hash inside a backtick string is not a comment",
			expr: "metric{path=~`/#/.*`}",
			want: "metric{path=~`/#/.*`}",
		},
		{
			name: "escaped quote does not end the string",
			expr: `metric{path="a\"#b"} # comment`,
			want: `metric{path="a\"#b"}`,
		},
		{
			name: "a backtick string is raw, so a trailing backslash still ends it",
			expr: "metric{path=~`a\\`} # comment",
			want: "metric{path=~`a\\`}",
		},
		{
			name: "line break inside a label value cannot break the format",
			expr: "metric{note=\"first\nsecond\"}",
			want: `metric{note="first second"}`,
		},
		{
			name: "a query that is only a comment yields nothing",
			expr: "# TODO write the query",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"dashboard":{"uid":"c","panels":[{"type":"timeseries",
				"datasource":{"type":"prometheus","uid":"prom-main"},
				"targets":[{"refId":"A","expr":%s}]}]}}`, mustJSON(tt.expr))

			env, err := extract.ParseEnvelope(strings.NewReader(body))
			if err != nil {
				t.Fatalf("parsing: %v", err)
			}
			var want []string
			if tt.want != "" {
				want = []string{tt.want}
			}
			assertLines(t, defaultExtractor().Extract(env).Queries, want)
		})
	}
}

// A dashboard remembers the datasource type it had when it was last saved,
// which goes stale as soon as the panel is pointed somewhere else. Grafana
// resolves the uid at query time, so the instance's own registry has to
// outrank the recorded type, which stays the fallback for a uid nobody knows.
func TestRegistryOutranksTheRecordedType(t *testing.T) {
	tests := []struct {
		name       string
		datasource string
		variable   string
		want       []string
	}{
		{
			name:       "a prometheus type on a loki uid is not PromQL",
			datasource: fmt.Sprintf(`{"type":"prometheus","uid":%q}`, testsupport.LokiUID),
		},
		{
			name:       "a loki type on a prometheus uid is PromQL",
			datasource: fmt.Sprintf(`{"type":"loki","uid":%q}`, testsupport.PrometheusUID),
			want:       []string{"up"},
		},
		{
			name:       "a datasource variable outranks the recorded type",
			datasource: `{"type":"prometheus","uid":"${ds}"}`,
			variable:   `{"name":"ds","type":"datasource","query":"loki"}`,
		},
		{
			name:       "the recorded type resolves a uid the instance does not have",
			datasource: `{"type":"prometheus","uid":"deleted-datasource"}`,
			want:       []string{"up"},
		},
		{
			name:       "the recorded type resolves a variable the dashboard does not declare",
			datasource: `{"type":"prometheus","uid":"${missing}"}`,
			want:       []string{"up"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"dashboard":{"uid":"c","templating":{"list":[%s]},
				"panels":[{"type":"timeseries","datasource":%s,
				"targets":[{"refId":"A","expr":"up"}]}]}}`, tt.variable, tt.datasource)

			env, err := extract.ParseEnvelope(strings.NewReader(body))
			if err != nil {
				t.Fatalf("parsing: %v", err)
			}
			result := defaultExtractor().Extract(env)

			assertLines(t, result.Queries, tt.want)
			if result.Stats.UnresolvedIncluded != 0 {
				t.Errorf("UnresolvedIncluded = %d, want 0: every reference here resolves to a type",
					result.Stats.UnresolvedIncluded)
			}
		})
	}
}

func mustJSON(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestStatsMerge(t *testing.T) {
	var total extract.Stats
	total.Merge(extract.Stats{Dashboards: 1, Queries: 3, SkippedByType: map[string]int{"loki": 2}})
	total.Merge(extract.Stats{Dashboards: 1, Queries: 1, SkippedByType: map[string]int{"loki": 1, "cloudwatch": 5}})

	if total.Dashboards != 2 || total.Queries != 4 {
		t.Errorf("unexpected totals: %+v", total)
	}
	top := total.TopSkippedTypes(2)
	if len(top) != 2 || top[0].Type != "cloudwatch" || top[0].Count != 5 || top[1].Type != "loki" || top[1].Count != 3 {
		t.Errorf("TopSkippedTypes = %+v", top)
	}
}

func findFixture(t *testing.T, name string) testsupport.Fixture {
	t.Helper()
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	for _, f := range fixtures {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("fixture %q not found", name)
	return testsupport.Fixture{}
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d lines, want %d\ngot:\n  %s\nwant:\n  %s",
			len(got), len(want), strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got: %s\nwant: %s", i+1, got[i], want[i])
		}
	}
}

// writeGolden rewrites a fixture's .expected file when -update is passed.
func writeGolden(t *testing.T, name string, got []string) {
	t.Helper()
	path := filepath.Join("..", "testsupport", "testdata", "dashboards", name+".expected")
	content := ""
	if len(got) > 0 {
		content = strings.Join(got, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("updated %s (%d lines)", path, len(got))
}
