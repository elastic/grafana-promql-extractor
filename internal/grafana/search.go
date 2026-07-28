package grafana

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

const (
	// MaxPageSize is the largest limit /api/search accepts.
	MaxPageSize = 5000
	// DefaultPageSize matches Grafana's own default.
	DefaultPageSize = 1000

	// defaultSort keeps paging stable across requests.
	defaultSort = "alpha-asc"
)

// DashboardHit is a single /api/search result.
type DashboardHit struct {
	UID       string `json:"uid"`
	Title     string `json:"title"`
	FolderUID string `json:"folderUid"`
}

// SearchOptions controls dashboard enumeration.
type SearchOptions struct {
	// PageSize is the number of dashboards per request, capped at MaxPageSize.
	PageSize int
	// Max limits the total number of dashboards returned. Zero means no limit.
	Max int
	// StartPage is the first page to request. Zero and one both mean the first page.
	StartPage int
	// FolderUIDs restricts the search to the given folders.
	FolderUIDs []string
	// Tags restricts the search to dashboards carrying all of the given tags.
	Tags []string
	// OnPage, when set, is called with the page number before its dashboards are
	// yielded, so an interrupted run can report where to resume.
	OnPage func(page int)
}

// ErrPagingStuck is returned when the server keeps returning the same page,
// which would otherwise loop forever.
var ErrPagingStuck = errors.New("grafana returned identical results for consecutive pages; the page parameter appears to be ignored")

func (o SearchOptions) pageSize() int {
	switch {
	case o.PageSize <= 0:
		return DefaultPageSize
	case o.PageSize > MaxPageSize:
		return MaxPageSize
	default:
		return o.PageSize
	}
}

func (o SearchOptions) firstPage() int {
	if o.StartPage > 1 {
		return o.StartPage
	}
	return 1
}

// FirstPage returns the page iteration starts at.
func (o SearchOptions) FirstPage() int { return o.firstPage() }

// SearchDashboards streams dashboard hits to yield, one page at a time, so that
// enumerating a large instance does not require holding every hit in memory.
// Iteration stops when a short page is returned, when Max is reached, or when
// yield returns an error.
func (c *Client) SearchDashboards(ctx context.Context, opt SearchOptions, yield func(DashboardHit) error) error {
	pageSize := opt.pageSize()
	sort := defaultSort
	seen := 0

	var prevFirstUID string
	for page := opt.firstPage(); ; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		hits, err := c.searchPage(ctx, opt, pageSize, page, sort)
		if err != nil {
			// Older Grafana releases reject unknown sort options; retry unsorted.
			if sort != "" && IsStatus(err, http.StatusBadRequest, http.StatusUnprocessableEntity) {
				c.cfg.Logf("search rejected sort=%s (%v), retrying without it", sort, err)
				sort = ""
				page--
				continue
			}
			return fmt.Errorf("searching dashboards (page %d): %w", page, err)
		}

		if len(hits) > 0 {
			if hits[0].UID != "" && hits[0].UID == prevFirstUID {
				return ErrPagingStuck
			}
			prevFirstUID = hits[0].UID
			if opt.OnPage != nil {
				opt.OnPage(page)
			}
		}

		for _, hit := range hits {
			if hit.UID == "" {
				continue
			}
			if err := yield(hit); err != nil {
				return err
			}
			seen++
			if opt.Max > 0 && seen >= opt.Max {
				return nil
			}
		}

		if len(hits) < pageSize {
			return nil
		}
	}
}

// CountDashboards walks the search pages without fetching dashboards, to
// establish a total for the progress display. It is cheap relative to the
// extraction itself: 50k dashboards is ten requests at the maximum page size.
func (c *Client) CountDashboards(ctx context.Context, opt SearchOptions) (int, error) {
	counting := opt
	counting.OnPage = nil
	// A page number only means something together with a page size, so a run
	// resuming at a later page has to be counted with the size it will use.
	if counting.firstPage() == 1 {
		counting.PageSize = MaxPageSize
	}
	count := 0
	err := c.SearchDashboards(ctx, counting, func(DashboardHit) error {
		count++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (c *Client) searchPage(ctx context.Context, opt SearchOptions, pageSize, page int, sort string) ([]DashboardHit, error) {
	q := url.Values{}
	q.Set("type", "dash-db")
	q.Set("limit", strconv.Itoa(pageSize))
	q.Set("page", strconv.Itoa(page))
	if sort != "" {
		q.Set("sort", sort)
	}
	for _, uid := range opt.FolderUIDs {
		q.Add("folderUIDs", uid)
	}
	for _, tag := range opt.Tags {
		q.Add("tag", tag)
	}

	var hits []DashboardHit
	if err := c.GetJSON(ctx, "/api/search", q, &hits); err != nil {
		return nil, err
	}
	return hits, nil
}

// DashboardJSON fetches the raw dashboard document for a UID. The caller must
// close the returned reader.
func (c *Client) DashboardJSON(ctx context.Context, uid string) (io.ReadCloser, error) {
	return c.Get(ctx, "/api/dashboards/uid/"+url.PathEscape(uid), nil)
}
