package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	esPort           = "9200/tcp"
	esStartupTimeout = 3 * time.Minute
	esImagePrefix    = "docker.elastic.co/elasticsearch/elasticsearch:"

	// DefaultPrometheusDataStream is the Prometheus data stream queries run
	// against through metrics-*.
	DefaultPrometheusDataStream = "metrics-generic.prometheus-default"

	prometheusIndexTemplate = "metrics-prometheus@template"
)

func queryWindow(seed time.Time) (start, end time.Time) {
	end = seed.UTC()
	start = end.Add(-defaultQueryWindow)
	return start, end
}

// Cluster is a running Elasticsearch instance, usually in Docker.
type Cluster struct {
	URL string
	// QueryStart and QueryEnd are the fixed query_range bounds passed to each
	// PromQL check.
	QueryStart time.Time
	QueryEnd   time.Time
	// Close stops the underlying container. It is safe to call more than once.
	Close func(context.Context) error
}

// ResolveImage maps a version string or full image reference to a container
// image name. Bare major.minor versions get ".0" appended. An empty version
// defaults to 9.4.4.
func ResolveImage(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return esImagePrefix + "9.4.4"
	}
	if strings.Contains(version, "/") {
		return version
	}
	if strings.Count(version, ".") == 1 {
		version += ".0"
	}
	return esImagePrefix + version
}

// StartElasticsearch boots a single-node Elasticsearch container from image
// and waits until PromQL accepts requests.
func StartElasticsearch(ctx context.Context, image string) (*Cluster, error) {
	if err := requireDocker(ctx); err != nil {
		return nil, err
	}

	image = strings.TrimSpace(image)
	if image == "" {
		return nil, errors.New("elasticsearch image is required")
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{esPort},
			Env: map[string]string{
				"discovery.type":                                    "single-node",
				"ES_JAVA_OPTS":                                      "-Xms512m -Xmx512m",
				"xpack.license.self_generated.type":                 "trial",
				"xpack.security.enabled":                            "false",
				"cluster.routing.allocation.disk.threshold_enabled": "false",
			},
			WaitingFor: wait.ForHTTP("/").
				WithPort(esPort).
				WithStartupTimeout(esStartupTimeout),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("starting Elasticsearch %s: %w", image, err)
	}

	closeFn := func(ctx context.Context) error {
		return container.Terminate(ctx)
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = closeFn(context.Background())
		return nil, err
	}
	port, err := container.MappedPort(ctx, esPort)
	if err != nil {
		_ = closeFn(context.Background())
		return nil, err
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

	if err := waitForCluster(ctx, baseURL); err != nil {
		_ = closeFn(context.Background())
		return nil, fmt.Errorf("waiting for Elasticsearch cluster: %w", err)
	}
	if err := waitForPrometheusTemplate(ctx, baseURL); err != nil {
		_ = closeFn(context.Background())
		return nil, fmt.Errorf("waiting for %s: %w", prometheusIndexTemplate, err)
	}

	queryStart, queryEnd := queryWindow(time.Now().UTC())

	if err := waitForPromQL(ctx, baseURL, DefaultPrometheusDataStream, queryStart, queryEnd); err != nil {
		_ = closeFn(context.Background())
		return nil, fmt.Errorf("Elasticsearch %s started but PromQL endpoint is not ready: %w", image, err)
	}

	return &Cluster{
		URL:        baseURL,
		QueryStart: queryStart,
		QueryEnd:   queryEnd,
		Close:      closeFn,
	}, nil
}

func pollUntil(ctx context.Context, timeout time.Duration, check func(context.Context) (done bool, err error)) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := check(ctx)
		if done {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if !sleep(ctx, 500*time.Millisecond) {
			return ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("timed out")
	}
	return lastErr
}

func waitForCluster(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 45 * time.Second}
	url := strings.TrimRight(baseURL, "/") + "/_cluster/health?wait_for_status=yellow&wait_for_events=languid&timeout=30s"
	return pollUntil(ctx, esStartupTimeout, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return true, nil
		}
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	})
}

func waitForPrometheusTemplate(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	url := strings.TrimRight(baseURL, "/") + "/_index_template/" + prometheusIndexTemplate
	return pollUntil(ctx, esStartupTimeout, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return false, readErr
		}
		if resp.StatusCode == http.StatusOK {
			var parsed struct {
				IndexTemplates []json.RawMessage `json:"index_templates"`
			}
			if json.Unmarshal(body, &parsed) == nil && len(parsed.IndexTemplates) > 0 {
				return true, nil
			}
			return false, fmt.Errorf("index template response missing %s", prometheusIndexTemplate)
		}
		if resp.StatusCode == http.StatusNotFound {
			return false, fmt.Errorf("%s not installed yet", prometheusIndexTemplate)
		}
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, ExtractErrorMessage(string(body)))
	})
}

func requireDocker(ctx context.Context) error {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return fmt.Errorf("docker is required to start Elasticsearch: %w", err)
	}
	defer provider.Close()

	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := provider.Health(checkCtx); err != nil {
		return fmt.Errorf("docker is not available: %w", err)
	}
	return nil
}

func waitForPromQL(ctx context.Context, baseURL, index string, start, end time.Time) error {
	client := &http.Client{Timeout: 10 * time.Second}
	return pollUntil(ctx, esStartupTimeout, func(ctx context.Context) (bool, error) {
		params := url.Values{}
		params.Set("query", "1")
		params.Set("start", start.Format(time.RFC3339))
		params.Set("end", end.Format(time.RFC3339))
		params.Set("step", defaultStep)
		endpoint := strings.TrimRight(baseURL, "/") + "/_prometheus/" + index + "/api/v1/query_range?" + params.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return false, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var parsed struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(body, &parsed) == nil && parsed.Status == "success" {
				return true, nil
			}
			return false, fmt.Errorf("unexpected response: %s", strings.TrimSpace(string(body)))
		}
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, ExtractErrorMessage(string(body)))
	})
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
