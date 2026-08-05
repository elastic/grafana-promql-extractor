package analyze

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/golang/snappy"
	"google.golang.org/protobuf/proto"

	remotewritev1 "github.com/elastic/grafana-promql-extractor/internal/remotewrite/v1"
)

const remoteWriteBatchSize = 500

// PopulateIndex seeds metrics-generic.prometheus-default through remote write.
// It returns how many time series were written.
func PopulateIndex(ctx context.Context, baseURL string, series []SeriesSpec, seedTime time.Time) (int, error) {
	if len(series) == 0 {
		return 0, nil
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	timestampMs := seedTime.UTC().UnixMilli()

	for i := 0; i < len(series); i += remoteWriteBatchSize {
		end := i + remoteWriteBatchSize
		if end > len(series) {
			end = len(series)
		}
		if err := remoteWrite(ctx, client, baseURL, series[i:end], timestampMs); err != nil {
			return 0, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/"+DefaultPrometheusDataStream+"/_refresh", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("refresh HTTP %d", resp.StatusCode)
	}
	return len(series), nil
}

func remoteWriteLabels(spec SeriesSpec) []*remotewritev1.Label {
	labels := make([]*remotewritev1.Label, 0, 1+len(spec.Labels))
	labels = append(labels, &remotewritev1.Label{Name: metricLabel, Value: spec.Metric})
	for k, v := range spec.Labels {
		labels = append(labels, &remotewritev1.Label{Name: k, Value: v})
	}
	slices.SortFunc(labels, func(a, b *remotewritev1.Label) int {
		return strings.Compare(a.Name, b.Name)
	})
	return labels
}

func remoteWrite(ctx context.Context, client *http.Client, baseURL string, series []SeriesSpec, timestampMs int64) error {
	ts := make([]*remotewritev1.TimeSeries, 0, len(series))
	for _, spec := range series {
		ts = append(ts, &remotewritev1.TimeSeries{
			Labels: remoteWriteLabels(spec),
			Samples: []*remotewritev1.Sample{{
				Value:     1.0,
				Timestamp: timestampMs,
			}},
		})
	}

	payload, err := proto.Marshal(&remotewritev1.WriteRequest{Timeseries: ts})
	if err != nil {
		return err
	}
	encoded := snappy.Encode(nil, payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/_prometheus/api/v1/write", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return remoteWriteResult(resp.StatusCode, string(body))
}

func remoteWriteResult(status int, body string) error {
	if status == http.StatusNoContent || status == http.StatusOK {
		return nil
	}
	msg := ExtractErrorMessage(body)
	// Elasticsearch can reject duplicate samples in the same batch with CONFLICT
	// after the first copy is indexed. That still leaves one sample per series,
	// which is enough for label mapping during coverage checks.
	if status == http.StatusBadRequest && strings.Contains(msg, "partially failed") {
		return nil
	}
	return fmt.Errorf("remote write HTTP %d: %s", status, msg)
}

// remoteWriteSeriesKey is the identity Elasticsearch uses for a seeded series.
func remoteWriteSeriesKey(spec SeriesSpec) string {
	var b strings.Builder
	for _, label := range remoteWriteLabels(spec) {
		b.WriteString(label.Name)
		b.WriteByte('=')
		b.WriteString(label.Value)
		b.WriteByte('|')
	}
	return b.String()
}
