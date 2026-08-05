package analyze

import (
	"net/http"
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
			labels: []string{metricLabel, "cluster", "job"},
		},
		{
			name:   "uppercase grouping label",
			spec:   SeriesSpec{Metric: "requests_total", Labels: map[string]string{"Cluster": "bootstrap"}},
			labels: []string{"Cluster", metricLabel},
		},
		{
			name:   "digit-prefixed label",
			spec:   SeriesSpec{Metric: "metric", Labels: map[string]string{"2xx": "bootstrap"}},
			labels: []string{"2xx", metricLabel},
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

func TestRemoteWriteResultPartialFailure(t *testing.T) {
	body := `Prometheus remote write request partially failed: 23 of 500 samples failed. Index [.ds-metrics-generic.prometheus-default] returned status [CONFLICT]`
	if err := remoteWriteResult(http.StatusBadRequest, body); err != nil {
		t.Fatalf("partial failure should not abort seeding: %v", err)
	}
}

func TestRemoteWriteResultHardFailure(t *testing.T) {
	if err := remoteWriteResult(http.StatusBadRequest, "invalid protobuf"); err == nil {
		t.Fatal("expected error for non-partial remote write failure")
	}
}

func TestRemoteWriteResultSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		if err := remoteWriteResult(status, ""); err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}
