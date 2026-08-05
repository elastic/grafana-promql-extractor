package analyze

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeriesCollectorDeduplicatesAfterMaterialization(t *testing.T) {
	c := NewSeriesCollector()
	for _, query := range []string{
		`container_memory_working_set_bytes{namespace="kube-system"}`,
		`container_memory_working_set_bytes{pod="foo"}`,
		`container_network_receive_bytes_total`,
		`sum by (namespace, pod, container, cluster, job, instance) (rate(http_requests_total[5m]))`,
		`container_memory_working_set_bytes`,
	} {
		c.AddQuery(query)
	}
	assertUniqueRemoteWriteKeys(t, c.Series())
}

func TestSeriesCollectorUniqueRemoteWriteKeysFromExport(t *testing.T) {
	path := filepath.Join("testdata", "collapsing-series-queries.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSeriesCollector()
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		_, query, ok := strings.Cut(line, ";")
		if !ok {
			t.Fatalf("malformed line: %q", line)
		}
		c.AddQuery(query)
	}
	assertUniqueRemoteWriteKeys(t, c.Series())
}

func assertUniqueRemoteWriteKeys(t *testing.T, series []SeriesSpec) {
	t.Helper()
	seen := make(map[string]struct{}, len(series))
	for _, spec := range series {
		key := remoteWriteSeriesKey(spec)
		if _, ok := seen[key]; ok {
			t.Fatalf("duplicate remote-write key %q after materialization", key)
		}
		seen[key] = struct{}{}
	}
}
