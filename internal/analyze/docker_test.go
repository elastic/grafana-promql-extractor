package analyze_test

import (
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestResolveImage(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"9.4.4", "docker.elastic.co/elasticsearch/elasticsearch:9.4.4"},
		{"9.5.0", "docker.elastic.co/elasticsearch/elasticsearch:9.5.0"},
		{"9.5.0-SNAPSHOT", "docker.elastic.co/elasticsearch/elasticsearch:9.5.0-SNAPSHOT"},
		{"", "docker.elastic.co/elasticsearch/elasticsearch:9.4.4"},
		{"my.registry/es:8.17.0", "my.registry/es:8.17.0"},
	}
	for _, tc := range tests {
		got, err := analyze.ResolveImage(tc.version)
		if err != nil {
			t.Errorf("ResolveImage(%q) unexpected error: %v", tc.version, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveImage(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestResolveImageRejectsIncompleteVersion(t *testing.T) {
	for _, version := range []string{"9.5", "9", "latest", "9.5-SNAPSHOT"} {
		_, err := analyze.ResolveImage(version)
		if err == nil {
			t.Errorf("ResolveImage(%q) succeeded, want error", version)
			continue
		}
		if !strings.Contains(err.Error(), "full version") {
			t.Errorf("ResolveImage(%q) error = %v, want full version message", version, err)
		}
	}
}
