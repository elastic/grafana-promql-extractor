package extract_test

import (
	"slices"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/extract"
	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

func TestExtractRule(t *testing.T) {
	tests := []struct {
		name  string
		rule  extract.Rule
		want  []string
		check func(t *testing.T, stats extract.Stats)
	}{
		{
			name: "expressions are skipped",
			rule: extract.Rule{Queries: []extract.RuleQuery{
				{DatasourceUID: testsupport.PrometheusUID, Expr: "up"},
				{DatasourceUID: "__expr__", Expr: "$A > 1"},
				{DatasourceUID: "-100", Expr: "$A > 1"},
			}},
			want: []string{"up"},
			check: func(t *testing.T, stats extract.Stats) {
				if stats.SkippedSpecial != 2 || stats.AlertRules != 1 || stats.RuleQueries != 3 {
					t.Errorf("stats = %+v", stats)
				}
			},
		},
		{
			name: "other query languages are skipped",
			rule: extract.Rule{Recording: true, Queries: []extract.RuleQuery{
				{DatasourceUID: testsupport.LokiUID, Expr: `count_over_time({job="api"}[5m])`},
				{DatasourceUID: testsupport.CloudWatchUID},
			}},
			check: func(t *testing.T, stats extract.Stats) {
				if stats.SkippedByType["loki"] != 1 || stats.SkippedByType["cloudwatch"] != 1 || stats.RecordingRules != 1 {
					t.Errorf("stats = %+v", stats)
				}
			},
		},
		{
			name: "a deleted datasource is unresolved",
			rule: extract.Rule{Queries: []extract.RuleQuery{{DatasourceUID: "gone", Expr: "up"}}},
			want: []string{"up"},
			check: func(t *testing.T, stats extract.Stats) {
				if stats.UnresolvedIncluded != 1 {
					t.Errorf("stats = %+v", stats)
				}
			},
		},
		{
			name: "duplicates and blanks are dropped",
			rule: extract.Rule{Queries: []extract.RuleQuery{
				{DatasourceUID: testsupport.PrometheusUID, Expr: "rate(x[5m])"},
				{DatasourceUID: testsupport.PrometheusSecondaryUID, Expr: "rate(x[5m])\n"},
				{DatasourceUID: testsupport.PrometheusUID, Expr: "  "},
			}},
			want: []string{"rate(x[5m])"},
			check: func(t *testing.T, stats extract.Stats) {
				if stats.Duplicates != 1 || stats.SkippedEmpty != 1 {
					t.Errorf("stats = %+v", stats)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.rule.ID = "rule:test"
			result := defaultExtractor().ExtractRule(tt.rule)
			if result.UID != "rule:test" {
				t.Errorf("UID = %q", result.UID)
			}
			if !slices.Equal(result.Queries, tt.want) {
				t.Errorf("queries = %q, want %q", result.Queries, tt.want)
			}
			tt.check(t, result.Stats)
		})
	}
}

// The shared fixtures yield what they say they do, so that the fake and a
// real Grafana are held to the same expectation.
func TestExtractRuleFixtures(t *testing.T) {
	for _, fixture := range testsupport.GrafanaRuleFixtures() {
		rule := extract.Rule{ID: fixture.UID, Recording: fixture.Record != ""}
		for _, q := range fixture.Queries {
			expr, _ := q.Model["expr"].(string)
			rule.Queries = append(rule.Queries, extract.RuleQuery{DatasourceUID: q.DatasourceUID, Expr: expr})
		}
		got := defaultExtractor().ExtractRule(rule).Queries
		if !slices.Equal(got, fixture.Expected) {
			t.Errorf("%s yields %q, want %q", fixture.UID, got, fixture.Expected)
		}
	}
}
