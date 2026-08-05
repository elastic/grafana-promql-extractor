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

func remoteWrite(ctx context.Context, client *http.Client, baseURL string, series []SeriesSpec, timestampMs int64) error {
	ts := make([]*remotewritev1.TimeSeries, 0, len(series))
	for _, spec := range series {
		labels := make([]*remotewritev1.Label, 0, 1+len(spec.Labels))
		labels = append(labels, &remotewritev1.Label{Name: "__name__", Value: spec.Metric})
		keys := make([]string, 0, len(spec.Labels))
		for k := range spec.Labels {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			labels = append(labels, &remotewritev1.Label{Name: k, Value: spec.Labels[k]})
		}
		ts = append(ts, &remotewritev1.TimeSeries{
			Labels: labels,
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

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote write HTTP %d: %s", resp.StatusCode, ExtractErrorMessage(string(body)))
	}
	return nil
}
