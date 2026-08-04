package analyze_test

import (
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestResolveImage(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"9.4.4", "docker.elastic.co/elasticsearch/elasticsearch:9.4.4"},
		{"9.5", "docker.elastic.co/elasticsearch/elasticsearch:9.5.0"},
		{"", "docker.elastic.co/elasticsearch/elasticsearch:9.4.4"},
		{"my.registry/es:8.17.0", "my.registry/es:8.17.0"},
	}
	for _, tc := range tests {
		if got := analyze.ResolveImage(tc.version); got != tc.want {
			t.Errorf("ResolveImage(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}
