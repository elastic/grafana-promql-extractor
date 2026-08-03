package analyze_test

import (
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestScrubQuery(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"rate(http_requests_total[$__rate_interval])", "rate(http_requests_total[1m])"},
		{"up{job=\"$job\"}", "up{job=\"job\"}"},
		{"up{job=\"${job}\"}", "up{job=\"job\"}"},
		{"sum by (cluster) (rate(metric[5m]))", "sum by (cluster) (rate(metric[5m]))"},
	}
	for _, tc := range tests {
		if got := analyze.ScrubQuery(tc.in); got != tc.want {
			t.Errorf("ScrubQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
