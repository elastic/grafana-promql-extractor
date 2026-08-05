package analyze

import (
	"testing"
)

func TestRemoteWriteLabelsSorted(t *testing.T) {
	tests := []struct {
		name   string
		spec   SeriesSpec
		labels []string
	}{
		{
			name:   "lowercase labels",
			spec:   SeriesSpec{Metric: "http_requests_total", Labels: map[string]string{"cluster": "bootstrap", "job": "api"}},
			labels: []string{"__name__", "cluster", "job"},
		},
		{
			name:   "uppercase grouping label",
			spec:   SeriesSpec{Metric: "requests_total", Labels: map[string]string{"Cluster": "bootstrap"}},
			labels: []string{"Cluster", "__name__"},
		},
		{
			name:   "digit-prefixed label",
			spec:   SeriesSpec{Metric: "metric", Labels: map[string]string{"2xx": "bootstrap"}},
			labels: []string{"2xx", "__name__"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := remoteWriteLabels(tc.spec)
			if len(got) != len(tc.labels) {
				t.Fatalf("got %d labels, want %d: %+v", len(got), len(tc.labels), got)
			}
			for i, want := range tc.labels {
				if got[i].Name != want {
					t.Fatalf("label[%d].Name = %q, want %q (full set: %+v)", i, got[i].Name, want, got)
				}
			}
			for i := 1; i < len(got); i++ {
				if got[i-1].Name >= got[i].Name {
					t.Fatalf("labels not strictly sorted: %+v", got)
				}
			}
		})
	}
}
