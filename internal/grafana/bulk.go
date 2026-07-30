package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// bulkGroup is the Kubernetes-style API Grafana 12 introduced. Unlike
	// /api/search it returns whole dashboards, which turns one request per
	// dashboard into one request per page.
	//
	// The oldest version of the API is the one to ask, because it hands back
	// the document as stored, which is what /api/dashboards/uid/:uid does too.
	// Later versions migrate it to the current schema on the way out, and that
	// migration drops __inputs, the section that says which datasource a
	// reference such as ${DS_LOKI} stands for. Without it, exported dashboards
	// lose their datasource types and their Loki queries end up looking like
	// PromQL.
	bulkGroup = "/apis/dashboard.grafana.app/v0alpha1"
	// BulkPageSize is the page size asked for. Grafana caps a page well below
	// this, so the value only matters for instances that allow more.
	BulkPageSize = 500
)

// DashboardDocument is one dashboard from a bulk listing: the uid Grafana knows
// it by, and the document itself, in the shape extract.ParseEnvelope accepts.
// The document carries no uid of its own, which is why both travel together.
type DashboardDocument struct {
	UID string
	// Document is nil when the listing named a dashboard without carrying it,
	// leaving it to the caller to fetch that one by uid. Otherwise it is only
	// valid until the next dashboard is yielded.
	Document []byte
}

// ListOptions controls bulk enumeration.
type ListOptions struct {
	// PageSize is the number of dashboards asked for per request.
	PageSize int
	// Max limits how many dashboards are yielded. Zero means all of them.
	Max int
	// ContinueToken resumes where an earlier listing stopped.
	ContinueToken string
	// OnPage, when set, receives the token that fetched the page about to be
	// yielded, so an interrupted run can be resumed from the same page. The
	// first page reports an empty token.
	OnPage func(token string)
}

func (o ListOptions) pageSize() int {
	if o.PageSize <= 0 {
		return BulkPageSize
	}
	return o.PageSize
}

// BulkAvailable reports whether this instance serves dashboards in bulk.
//
// It asks for one dashboard. Releases before Grafana 12 answer 404. A release
// that knows the endpoint but returns nothing is either empty or keeps its
// dashboards in a namespace this client cannot name, and since a namespace
// nobody has heard of yields an empty list rather than an error, an empty
// answer is treated as a reason to stay with the search API, which needs no
// namespace at all.
func (c *Client) BulkAvailable(ctx context.Context) (bool, error) {
	q := url.Values{"limit": []string{"1"}}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := c.GetJSON(ctx, c.bulkPath(), q, &page); err != nil {
		if IsStatus(err, http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized,
			http.StatusMethodNotAllowed, http.StatusNotImplemented) {
			c.cfg.Logf("bulk dashboard API unavailable: %v", err)
			return false, nil
		}
		return false, err
	}
	return len(page.Items) > 0, nil
}

// ListDashboards streams whole dashboards, page by page. Pages are chained by
// the token the server returns with each one, so a listing cannot start in the
// middle the way search paging can; ContinueToken resumes one instead.
func (c *Client) ListDashboards(ctx context.Context, opt ListOptions, yield func(DashboardDocument) error) error {
	token := opt.ContinueToken
	yielded := 0
	// A listing ends on a page that carries no token to continue with, and a
	// page in the middle of one has been seen to come back empty when the
	// instance is busy, which looks exactly the same. Asking a second time
	// keeps a busy instance from cutting a run short without saying so.
	reasked := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if opt.OnPage != nil {
			opt.OnPage(token)
		}

		remaining := 0
		if opt.Max > 0 {
			remaining = opt.Max - yielded
		}
		page, err := c.listPage(ctx, opt, token, remaining, yield)
		yielded += page.yielded
		c.cfg.Logf("listed %d dashboards (%d so far), next page %s",
			page.yielded, yielded, describeToken(page.next))
		if err != nil {
			return fmt.Errorf("listing dashboards: %w", err)
		}
		if opt.Max > 0 && yielded >= opt.Max {
			return nil
		}
		if page.next == "" {
			if token == "" || page.yielded > 0 || reasked {
				return nil
			}
			reasked = true
			c.cfg.Logf("the listing ended on an empty page; asking for it once more")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.cfg.RetryBaseDelay):
			}
			continue
		}
		token = page.next
		reasked = false
	}
}

// page is what one request to the collection delivered.
type page struct {
	next    string
	yielded int
}

// listPage streams one page into yield, retrying a body that breaks halfway.
// Since a page is decoded as it arrives rather than buffered, a retry has to
// skip what the failed attempt already delivered instead of repeating it.
func (c *Client) listPage(ctx context.Context, opt ListOptions, token string, limit int,
	yield func(DashboardDocument) error) (page, error) {

	var result page
	for attempt := 0; ; attempt++ {
		delivered := result.yielded
		next, count, err := c.streamPage(ctx, opt, token, delivered, limit, yield)
		result.next = next
		result.yielded += count
		if err == nil {
			return result, nil
		}
		if attempt >= c.cfg.Retries || !retryable(err) {
			return result, err
		}
		if limit > 0 {
			limit -= count
		}

		delay := c.retryDelay(attempt, err)
		c.cfg.Logf("retrying dashboard page after %d dashboards in %s: %v",
			result.yielded, delay.Round(time.Millisecond), err)
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// streamPage decodes one page as it arrives, skipping the first skip
// dashboards, and returns the token that continues after it.
func (c *Client) streamPage(ctx context.Context, opt ListOptions, token string, skip, limit int,
	yield func(DashboardDocument) error) (string, int, error) {

	q := url.Values{}
	q.Set("limit", strconv.Itoa(opt.pageSize()))
	if token != "" {
		q.Set("continue", token)
	}
	body, err := c.Get(ctx, c.bulkPath(), q)
	if err != nil {
		return "", 0, err
	}
	defer body.Close()

	var (
		next     string
		yielded  int
		sawItems bool
		// incomplete counts dashboards the page named but did not carry.
		incomplete int
	)
	dec := json.NewDecoder(body)
	if err := expect(dec, json.Delim('{')); err != nil {
		return "", 0, err
	}
	for dec.More() {
		field, err := dec.Token()
		if err != nil {
			return next, yielded, err
		}
		switch field {
		case "items":
			sawItems = true
			seen := 0
			if err := expect(dec, json.Delim('[')); err != nil {
				return next, yielded, err
			}
			for dec.More() {
				var item struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Spec json.RawMessage `json:"spec"`
				}
				if err := dec.Decode(&item); err != nil {
					return next, yielded, err
				}
				seen++
				if seen <= skip {
					continue
				}
				if item.Metadata.Name == "" {
					// Nothing identifies this dashboard, so there is no way to
					// go and get it either.
					c.cfg.Logf("a listed dashboard carries no name and was left out")
					continue
				}
				// An item without a document still names a dashboard, and
				// dropping it here would lose it without a trace. Handing it
				// over without one asks the caller to fetch it by uid.
				document := item.Spec
				if len(document) == 0 || string(document) == "null" {
					document = nil
					incomplete++
				}
				if err := yield(DashboardDocument{UID: item.Metadata.Name, Document: document}); err != nil {
					return next, yielded, err
				}
				yielded++
				if limit > 0 && yielded >= limit {
					return next, yielded, nil
				}
			}
			if err := expect(dec, json.Delim(']')); err != nil {
				return next, yielded, err
			}
		case "metadata":
			var meta struct {
				Continue string `json:"continue"`
			}
			if err := dec.Decode(&meta); err != nil {
				return next, yielded, err
			}
			next = meta.Continue
		default:
			var ignored json.RawMessage
			if err := dec.Decode(&ignored); err != nil {
				return next, yielded, err
			}
		}
	}
	// A listing ends when a page carries no token, so a reply that is not a
	// dashboard list at all would end it too, quietly and several thousand
	// dashboards early.
	if !sawItems {
		return next, yielded, errors.New("the reply carries no dashboards and is not a dashboard list")
	}
	if incomplete > 0 {
		c.cfg.Logf("%d of the %d dashboards on this page arrived without a document and will be fetched one by one",
			incomplete, yielded)
	}
	return next, yielded, nil
}

// bulkPath is the collection of the organization this client talks to. Grafana
// calls org 1's namespace "default" and every other one "org-<id>".
func (c *Client) bulkPath() string {
	namespace := "default"
	if c.cfg.OrgID > 1 {
		namespace = "org-" + strconv.Itoa(c.cfg.OrgID)
	}
	return bulkGroup + "/namespaces/" + namespace + "/dashboards"
}

// describeToken keeps a log line readable: the tokens are long and opaque, and
// what matters is whether there is one at all.
func describeToken(token string) string {
	switch {
	case token == "":
		return "none, the listing is complete"
	case len(token) > 12:
		return token[:12] + "..."
	default:
		return token
	}
}

func expect(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != want {
		return fmt.Errorf("unexpected %v in the dashboard list, want %v", tok, want)
	}
	return nil
}

// ErrBulkUnavailable is returned when bulk listing was asked for explicitly but
// the instance does not serve it.
var ErrBulkUnavailable = errors.New("this Grafana does not serve dashboards in bulk")
