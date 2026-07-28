//go:build corpus

package corpus

import (
	"strings"
	"testing"
)

// The corpus test is only as trustworthy as the checks it applies, so the
// checks are tested too.

func TestInterpolate(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"range selector", "rate(x[$__rate_interval])", "rate(x[5m])"},
		{"legacy syntax in a range selector", "rate(x[[[interval]]])", "rate(x[5m])"},
		{"braced variable in a range selector", "rate(x[${interval}])", "rate(x[5m])"},
		{"metric name", "$metric{job=\"api\"}", "grafanavar{job=\"api\"}"},
		{"label value", "x{job=\"$job\"}", "x{job=\"grafanavar\"}"},
		{"format specifier", "x{job=~\"${job:regex}\"}", "x{job=~\"grafanavar\"}"},
		{"millisecond variable", "x / $__interval_ms", "x / 1000"},
		{"pipe inside a label value is left alone", "x{job=~\"a|b\"}", "x{job=~\"a|b\"}"},
		{"no variables", "sum(rate(x[5m]))", "sum(rate(x[5m]))"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Interpolate(tt.query); got != tt.want {
				t.Errorf("Interpolate(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestParsesAsPromQL(t *testing.T) {
	valid := []string{
		"sum(rate(http_requests_total[5m]))",
		"100 - (avg by (cpu) (irate(node_cpu_seconds_total{mode=\"idle\"}[$__rate_interval])) * 100)",
		"topk(5, sum by (uri) (rate(x{job=~\"$job\"}[$interval])))",
		"node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes * 100",
		"sum by (realm) (increase(keycloak_logins[24h]))",
	}
	for _, query := range valid {
		if err := ParsesAsPromQL(query); err != nil {
			t.Errorf("ParsesAsPromQL(%q) = %v, want nil", query, err)
		}
	}

	// The parser has to reject other query languages, or the check is worthless.
	invalid := []string{
		`{app="api"} |= "error"`,
		`sum by (level) (count_over_time({job="x"} | json [5m]))`,
		"SELECT mean(value) FROM cpu",
		`from(bucket: "telegraf") |> range(start: -1h)`,
	}
	for _, query := range invalid {
		if err := ParsesAsPromQL(query); err == nil {
			t.Errorf("ParsesAsPromQL(%q) = nil, want an error", query)
		}
	}
}

func TestLooksLikeLogQL(t *testing.T) {
	logQL := []string{
		`{app="api"} |= "error"`,
		`{app="api"} |~ "5.."`,
		`sum(count_over_time({job="x"} | logfmt | duration > 10s [5m]))`,
	}
	for _, query := range logQL {
		if !LooksLikeLogQL(query) {
			t.Errorf("LooksLikeLogQL(%q) = false, want true", query)
		}
	}

	promQL := []string{
		`x{job=~"a|b"}`,
		`sum(rate(http_requests_total{code=~"4..|5.."}[5m]))`,
		`up == 1`,
	}
	for _, query := range promQL {
		if LooksLikeLogQL(query) {
			t.Errorf("LooksLikeLogQL(%q) = true, want false", query)
		}
	}
}

func TestFindExprs(t *testing.T) {
	document := []byte(`{
	  "annotations": {"list": [{"expr": "changes(x[5m])"}, {"name": "no expr"}]},
	  "panels": [
	    {"targets": [{"expr": "a"}, {"expr": "  "}, {"refId": "C"}]},
	    {"panels": [{"targets": [{"expr": "b"}]}]},
	    {"CustomPanel": {"targets": [{"expr": "stale"}]}}
	  ],
	  "rows": [{"panels": [{"targets": [{"expr": "c\n  + d"}]}]}]
	}`)

	found := FindExprs(document)
	got := make(map[string]string, len(found))
	for _, f := range found {
		got[f.Expr] = f.Location
	}

	want := map[string]string{
		"changes(x[5m])": "annotations/list",
		"a":              "panels/targets",
		"b":              "panels/panels/targets",
		"stale":          "panels/CustomPanel/targets",
		"c + d":          "rows/panels/targets",
	}
	if len(got) != len(want) {
		t.Fatalf("found %d expressions, want %d: %v", len(got), len(want), got)
	}
	for expr, location := range want {
		if got[expr] != location {
			t.Errorf("expression %q found at %q, want %q", expr, got[expr], location)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := map[string]string{
		"sum(  rate(x[5m])  )":                        "sum( rate(x[5m]) )",
		"sum(total)\n# only the paid plans\n- sum(x)": "sum(total) - sum(x)",
		"up == 1 # is it up?":                         "up == 1",
		`metric{path="/#/home"}`:                      `metric{path="/#/home"}`,
		"metric{note=\"a  b\"}":                       `metric{note="a  b"}`,
		"metric{note=\"a\nb\"}":                       `metric{note="a b"}`,
		`metric{path="a\"#b"} # comment`:              `metric{path="a\"#b"}`,
		"# nothing but a comment":                     "",
	}
	for expr, want := range tests {
		if got := Normalize(expr); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", expr, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := map[string]string{
		"panels/targets":                      "panel target",
		"panels/panels/targets":               "panel target",
		"rows/panels/targets":                 "panel target",
		"annotations/list":                    "annotation",
		"panels/CustomPanel/targets":          "other: panels/CustomPanel/targets",
		"targets":                             "other: targets",
		"panels/alert/conditions/query/model": "legacy alert rule",
	}
	for location, want := range tests {
		if got := classify(location); got != want {
			t.Errorf("classify(%q) = %q, want %q", location, got, want)
		}
	}
}

// TestSkipReasonNeedsEvidence guards the accounting check against excusing a
// miss on flimsy grounds.
func TestSkipReasonNeedsEvidence(t *testing.T) {
	prometheus := Dashboard{Entry: Entry{Datasource: "Prometheus"}}
	if reason := skipReason(prometheus, "sum(rate(x[5m]))"); reason != "" {
		t.Errorf("a Prometheus expression was excused as %q", reason)
	}
	mixed := Dashboard{Entry: Entry{Datasource: "Loki, Prometheus"}}
	if reason := skipReason(mixed, "sum(rate(x[5m]))"); reason != "" {
		t.Errorf("an expression from a partly Prometheus dashboard was excused as %q", reason)
	}
	if reason := skipReason(prometheus, `{app="x"} |= "e"`); !strings.Contains(reason, "log") {
		t.Errorf("a log query was not recognized, got %q", reason)
	}
}
