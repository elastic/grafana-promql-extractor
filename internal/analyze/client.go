package analyze

import (
	"context"
	"crypto/tls"
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

	prometheusIndexTemplate = "metrics-prometheus@template"
)

// queryWindow returns a fixed range ending at seed so seeded samples fall inside it.
func queryWindow(seed time.Time) (start, end time.Time) {
	end = seed.UTC()
	start = end.Add(-defaultQueryWindow)
	return start, end
}

// ClientConfig configures an Elasticsearch PromQL client.
type ClientConfig struct {
	BaseURL string
	// Index, when set, scopes requests to /_prometheus/{index}/api/v1/query_range.
	Index string

	User     string
	Password string
	// APIKey is sent as Authorization: ApiKey <key>.
	APIKey string

	Timeout            time.Duration
	InsecureSkipVerify bool
	UserAgent          string

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

	path := "/_prometheus/api/v1/query_range"
	if idx := strings.TrimSpace(cfg.Index); idx != "" {
		path = "/_prometheus/" + idx + "/api/v1/query_range"
	}
	endpoint := base.ResolveReference(&url.URL{Path: path}).String()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // user-controlled for dev clusters
	}

	return &Client{
		endpoint: endpoint,
		cfg:      cfg,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
		},
	}, nil
}

// QueryRange executes one PromQL range query. success is true when Elasticsearch
// returns Prometheus status success.
func (c *Client) QueryRange(ctx context.Context, query string) (success bool, errMsg string, httpStatus int, err error) {
	end := time.Now().UTC()
	start := end.Add(-defaultQueryWindow)
	if c.cfg.End != "" {
		if parsed, err := time.Parse(time.RFC3339, c.cfg.End); err == nil {
			end = parsed
		}
	}
	if c.cfg.Start != "" {
		if parsed, err := time.Parse(time.RFC3339, c.cfg.Start); err == nil {
			start = parsed
		}
	} else if c.cfg.End == "" {
		start = end.Add(-defaultQueryWindow)
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
	if key := strings.TrimSpace(c.cfg.APIKey); key != "" {
		req.Header.Set("Authorization", "ApiKey "+key)
	} else if c.cfg.User != "" {
		req.SetBasicAuth(c.cfg.User, c.cfg.Password)
	}

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
