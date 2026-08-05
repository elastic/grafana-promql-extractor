package analyze_test

import (
	"reflect"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

const bootstrapLabel = "bootstrap"

func wantBootstrap(t *testing.T, labels map[string]string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if labels[k] != bootstrapLabel {
			t.Fatalf("labels[%q] = %q, want %q (full: %#v)", k, labels[k], bootstrapLabel, labels)
		}
	}
}

func seriesByMetric(series []analyze.SeriesSpec) map[string]analyze.SeriesSpec {
	byMetric := make(map[string]analyze.SeriesSpec, len(series))
	for _, s := range series {
		byMetric[s.Metric] = s
	}
	return byMetric
}

func TestSeriesCollector(t *testing.T) {
	c := analyze.NewSeriesCollector()
	for _, query := range []string{
		`sum(rate(http_requests_total{job="api"}[5m])) by (cluster)`,
		`node_cpu_seconds_total{instance="localhost"}`,
		`foo * on(instance) group_left(role) bar`,
		`disk_usage_bytes{device=~"/dev/.*",fstype!="tmpfs"}`,
		`{__name__="up",job=~"prometheus.*"}`,
	} {
		c.AddQuery(query)
	}
	if c.ParseSkipped() != 0 {
		t.Fatalf("ParseSkipped() = %d, want 0", c.ParseSkipped())
	}

	series := c.Series()
	if len(series) != 6 {
		t.Fatalf("got %d series, want 6: %+v", len(series), series)
	}
	if again := c.Series(); !reflect.DeepEqual(seriesByMetric(series), seriesByMetric(again)) {
		t.Fatalf("Series() not idempotent:\nfirst  = %+v\nsecond = %+v", series, again)
	}

	byMetric := seriesByMetric(series)
	tests := []struct {
		metric    string
		labels    map[string]string
		bootstrap []string
	}{
		{
			metric:    "http_requests_total",
			labels:    map[string]string{"job": "api"},
			bootstrap: []string{"cluster", "instance", "role"},
		},
		{
			metric:    "node_cpu_seconds_total",
			labels:    map[string]string{"instance": "localhost"},
			bootstrap: []string{"cluster", "role"},
		},
		{
			metric:    "foo",
			bootstrap: []string{"cluster", "instance", "role"},
		},
		{
			metric:    "bar",
			bootstrap: []string{"cluster", "instance", "role"},
		},
		{
			metric:    "disk_usage_bytes",
			bootstrap: []string{"cluster", "device", "fstype", "instance", "role"},
		},
		{
			metric:    "up",
			bootstrap: []string{"cluster", "instance", "job", "role"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.metric, func(t *testing.T) {
			spec, ok := byMetric[tc.metric]
			if !ok {
				t.Fatalf("missing %q in %+v", tc.metric, byMetric)
			}
			for k, want := range tc.labels {
				if got := spec.Labels[k]; got != want {
					t.Fatalf("labels[%q] = %q, want %q (full: %#v)", k, got, want, spec.Labels)
				}
			}
			wantBootstrap(t, spec.Labels, tc.bootstrap...)
		})
	}
}

func TestSeriesCollectorParseSkipped(t *testing.T) {
	c := analyze.NewSeriesCollector()
	c.AddQuery(`up`)
	c.AddQuery(`{`)
	if c.ParseSkipped() != 1 {
		t.Fatalf("ParseSkipped() = %d, want 1", c.ParseSkipped())
	}
	series := c.Series()
	if len(series) != 1 || series[0].Metric != "up" {
		t.Fatalf("Series() = %+v, want one series up", series)
	}
}
