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
)

const (
	defaultQueryWindow = 5 * time.Minute
	defaultStep        = "60s"
)

// ClientConfig configures an Elasticsearch PromQL client.
type ClientConfig struct {
	BaseURL string

	Timeout   time.Duration
	UserAgent string

	Start string
	End   string
	Step  string
}

// Client calls the Elasticsearch Prometheus-compatible query_range endpoint.
type Client struct {
	endpoint string
	http     *http.Client
	cfg      ClientConfig
}

type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// NewClient validates config and builds a Client.
func NewClient(cfg ClientConfig) (*Client, error) {
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return nil, errors.New("elasticsearch url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid elasticsearch url %q: %w", cfg.BaseURL, err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("invalid elasticsearch url %q: missing host", cfg.BaseURL)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "grafana-promql-extractor"
	}
	if cfg.Step == "" {
		cfg.Step = defaultStep
	}
	if cfg.Start != "" {
		if _, err := time.Parse(time.RFC3339, cfg.Start); err != nil {
			return nil, fmt.Errorf("invalid start time %q: %w", cfg.Start, err)
		}
	}
	if cfg.End != "" {
		if _, err := time.Parse(time.RFC3339, cfg.End); err != nil {
			return nil, fmt.Errorf("invalid end time %q: %w", cfg.End, err)
		}
	}
	if cfg.Start != "" && cfg.End != "" {
		start, _ := time.Parse(time.RFC3339, cfg.Start)
		end, _ := time.Parse(time.RFC3339, cfg.End)
		if !start.Before(end) {
			return nil, fmt.Errorf("start time %q must be before end time %q", cfg.Start, cfg.End)
		}
	}

	endpoint := base.ResolveReference(&url.URL{Path: "/_prometheus/api/v1/query_range"}).String()

	return &Client{
		endpoint: endpoint,
		cfg:      cfg,
		http: &http.Client{
			Timeout: cfg.Timeout,
		},
	}, nil
}

// QueryRange executes one PromQL range query. success is true when Elasticsearch
// returns Prometheus status success.
//
// Parameters are sent as a GET query string. Form-encoded POST would avoid URL
// length limits, but Elasticsearch accepts that only on authenticated HTTPS to
// a node with HTTP TLS enabled; the Docker analyze path is plain HTTP, so GET
// is the reliable choice. See
// https://www.elastic.co/docs/reference/query-languages/promql/promql-limitations#promql-limitations-form-post.
func (c *Client) QueryRange(ctx context.Context, query string) (success bool, errMsg string, httpStatus int, err error) {
	end := time.Now().UTC()
	if c.cfg.End != "" {
		end, _ = time.Parse(time.RFC3339, c.cfg.End)
	}
	start := end.Add(-defaultQueryWindow)
	if c.cfg.Start != "" {
		start, _ = time.Parse(time.RFC3339, c.cfg.Start)
	}

	params := url.Values{}
	params.Set("query", query)
	params.Set("start", start.Format(time.RFC3339))
	params.Set("end", end.Format(time.RFC3339))
	params.Set("step", c.cfg.Step)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return false, "", 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, "", resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}
	httpStatus = resp.StatusCode

	var parsed promResponse
	if err := json.Unmarshal(body, &parsed); err == nil {
		switch parsed.Status {
		case "success":
			return true, "", httpStatus, nil
		case "error":
			msg := strings.TrimSpace(parsed.Error)
			if msg == "" {
				msg = strings.TrimSpace(parsed.ErrorType)
			}
			if msg == "" {
				msg = "prometheus status error"
			}
			return false, ExtractErrorMessage(msg), httpStatus, nil
		case "":
			return false, "missing prometheus status", httpStatus, nil
		default:
			return false, fmt.Sprintf("unexpected prometheus status %q", parsed.Status), httpStatus, nil
		}
	}

	msg := ExtractErrorMessage(string(body))
	if msg == "" {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			msg = "missing prometheus status"
		} else {
			msg = resp.Status
		}
	}
	return false, msg, httpStatus, nil
}
