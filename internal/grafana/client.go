// Package grafana provides a minimal HTTP client for the Grafana API, covering
// the endpoints needed to enumerate dashboards and resolve datasource types.
package grafana

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultUserAgent identifies the tool to Grafana access logs.
	DefaultUserAgent = "grafana-promql-extractor"

	maxRetryDelay      = 30 * time.Second
	errorBodyMaxLength = 512

	defaultMaxIdleConns = 64
)

// Config configures a Client.
type Config struct {
	BaseURL string

	// Token is a Grafana service account token, sent as a bearer token.
	Token string
	// User and Password are used for basic auth when Token is empty.
	User     string
	Password string

	// OrgID, when non-zero, selects the organization via X-Grafana-Org-Id.
	OrgID int

	Timeout            time.Duration
	Retries            int
	RetryBaseDelay     time.Duration
	InsecureSkipVerify bool
	UserAgent          string

	// MaxIdleConns caps the connections kept alive for reuse. Below the number
	// of requests in flight, connections get closed and reopened constantly, so
	// a caller running a worker pool should set this to the pool size.
	MaxIdleConns int

	// Logf receives warnings such as retry notices. It may be nil.
	Logf func(format string, args ...any)
}

// Client talks to a single Grafana instance.
type Client struct {
	base *url.URL
	http *http.Client
	cfg  Config
}

// StatusError is returned for non-2xx responses.
type StatusError struct {
	StatusCode int
	Status     string
	URL        string
	Body       string
	// RetryAfter holds the Retry-After header, if any.
	RetryAfter string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: %s", e.URL, e.Status)
	}
	return fmt.Sprintf("%s: %s: %s", e.URL, e.Status, e.Body)
}

// IsStatus reports whether err is a StatusError with one of the given codes.
func IsStatus(err error, codes ...int) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.StatusCode == c {
			return true
		}
	}
	return false
}

// New validates the config and builds a Client.
func New(cfg Config) (*Client, error) {
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return nil, errors.New("grafana url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid grafana url %q: %w", cfg.BaseURL, err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("invalid grafana url %q: missing host", cfg.BaseURL)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = 500 * time.Millisecond
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = defaultMaxIdleConns
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Only ever one host, so the per-host cap is the only one that matters, but
	// the total has to keep up with it.
	transport.MaxIdleConnsPerHost = cfg.MaxIdleConns
	transport.MaxIdleConns = cfg.MaxIdleConns
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Client{
		base: base,
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout, Transport: transport},
	}, nil
}

// BaseURL returns the normalized base URL.
func (c *Client) BaseURL() string { return c.base.String() }

func (c *Client) urlFor(path string, q url.Values) string {
	u := c.base.JoinPath(path)
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// Get performs a GET request and returns the response body, which the caller
// must close. Non-2xx responses are returned as *StatusError.
func (c *Client) Get(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
	target := c.urlFor(path, q)

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, target)
		if err == nil {
			return resp.Body, nil
		}
		lastErr = err

		if attempt >= c.cfg.Retries || !retryable(err) {
			return nil, lastErr
		}
		delay := c.retryDelay(attempt, err)
		c.cfg.Logf("retrying %s in %s after error: %v", target, delay.Round(time.Millisecond), err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *Client) attempt(ctx context.Context, target string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	} else if c.cfg.User != "" || c.cfg.Password != "" {
		req.SetBasicAuth(c.cfg.User, c.cfg.Password)
	}
	if c.cfg.OrgID > 0 {
		req.Header.Set("X-Grafana-Org-Id", strconv.Itoa(c.cfg.OrgID))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMaxLength))
	resp.Body.Close()
	return nil, &StatusError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		URL:        target,
		Body:       strings.TrimSpace(string(body)),
		RetryAfter: resp.Header.Get("Retry-After"),
	}
}

// GetJSON performs a GET request and decodes the JSON response into out.
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	body, err := c.Get(ctx, path, q)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", path, err)
	}
	return nil
}

// Health verifies that the instance is reachable and returns its version.
func (c *Client) Health(ctx context.Context) (string, error) {
	var health struct {
		Database string `json:"database"`
		Version  string `json:"version"`
	}
	if err := c.GetJSON(ctx, "/api/health", nil, &health); err != nil {
		return "", fmt.Errorf("grafana health check failed: %w", err)
	}
	return health.Version, nil
}

// retryable reports whether a request should be retried. Transport errors and
// 429/5xx responses are transient; 4xx responses are not.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode == http.StatusTooManyRequests || se.StatusCode >= 500
	}
	return true
}

func (c *Client) retryDelay(attempt int, err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) {
		if d, ok := retryAfter(se); ok {
			return d
		}
	}
	delay := c.cfg.RetryBaseDelay << attempt
	if delay > maxRetryDelay || delay <= 0 {
		delay = maxRetryDelay
	}
	// Jitter by +/-20% so concurrent workers do not retry in lockstep.
	jitter := 1 + (rand.Float64()*0.4 - 0.2)
	return time.Duration(float64(delay) * jitter)
}

func retryAfter(se *StatusError) (time.Duration, bool) {
	if se.RetryAfter == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(se.RetryAfter)); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		if d > maxRetryDelay {
			d = maxRetryDelay
		}
		return d, true
	}
	if t, err := http.ParseTime(strings.TrimSpace(se.RetryAfter)); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		if d > maxRetryDelay {
			d = maxRetryDelay
		}
		return d, true
	}
	return 0, false
}
