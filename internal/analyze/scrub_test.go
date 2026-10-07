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
		{"sum(increase(${prefix:raw}rpc_server_duration_bucket{job=\"$job\"}[$__rate_interval])) by (le)",
			"sum(increase(prefixrpc_server_duration_bucket{job=\"job\"}[1m])) by (le)"},
		{"sum(${metric:value}(x[5m]))", "sum(rate(x[5m]))"},
		{"sum(${metric:value}(x[$__rate_interval]))", "sum(rate(x[1m]))"},
		{"${agg}(rate(x[5m])) by (job)", "avg(rate(x[5m])) by (job)"},
		{"${agg} (x{job=\"$job\"})", "avg (x{job=\"job\"})"},
		{"x{job=~\"${job:regex}\"}", "x{job=~\"job\"}"},
		{"up{job=\"[[job]]\"}", "up{job=\"job\"}"},
		{"rate(x[${interval}])", "rate(x[1m])"},
		{"rate(x[${__range_s}s])", "rate(x[1m])"},
		{"rate(x[ $__rate_interval])", "rate(x[1m])"},
		{`max(otelcol_process_uptime${suffix_seconds}${suffix_total}{"service${divider:raw}instance${divider:raw}id"=~".*"})`,
			`max(otelcol_process_uptimesuffix_secondssuffix_total{"servicedividerinstancedividerid"=~".*"})`},
		{`label_replace(x{ns="$namespace"}, "version", "$1", "image", ".+:(.+)")`,
			`label_replace(x{ns="namespace"}, "version", "$1", "image", ".+:(.+)")`},
		{`label_replace(x, "dst", "${1}", "src", "(.*)")`,
			`label_replace(x, "dst", "${1}", "src", "(.*)")`},
		{`${__to:date:seconds} - metric`, "__to - metric"},
		{`x{p=~"[a-z]+"}`, `x{p=~"[a-z]+"}`},
	}
	for _, tc := range tests {
		if got := analyze.ScrubQuery(tc.in); got != tc.want {
			t.Errorf("ScrubQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
