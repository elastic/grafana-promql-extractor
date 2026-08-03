package analyze_test

import (
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestCollectSeries(t *testing.T) {
	series := analyze.CollectSeries([]string{
		`sum(rate(http_requests_total{job="api"}[5m])) by (cluster)`,
		`node_cpu_seconds_total{instance="localhost"}`,
		`foo * on(instance) group_left(role) bar`,
	})
	if len(series) < 2 {
		t.Fatalf("got %d series, want at least 2: %+v", len(series), series)
	}

	byMetric := map[string]analyze.SeriesSpec{}
	for _, s := range series {
		byMetric[s.Metric] = s
	}
	http, ok := byMetric["http_requests_total"]
	if !ok {
		t.Fatalf("missing http_requests_total in %+v", byMetric)
	}
	if http.Labels["job"] != "api" {
		t.Fatalf("labels = %#v", http.Labels)
	}
	if http.Labels["cluster"] != "bootstrap" {
		t.Fatalf("expected by() label cluster, got %#v", http.Labels)
	}
	if http.Labels["instance"] != "bootstrap" {
		t.Fatalf("expected on() label instance, got %#v", http.Labels)
	}
	if http.Labels["role"] != "bootstrap" {
		t.Fatalf("expected group_left() label role, got %#v", http.Labels)
	}
}
