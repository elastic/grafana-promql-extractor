package analyze

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// StreamOptions configures a streaming analyze pass over an export file.
type StreamOptions struct {
	Client      *Client
	Concurrency int
	Report      *Report
	OnQuery     func()
}

// StreamAnalyze scans path a second time, checks each query against Elasticsearch,
// and records outcomes into opt.Report without retaining every result.
func StreamAnalyze(ctx context.Context, path string, opt StreamOptions) error {
	if opt.Client == nil {
		return fmt.Errorf("client is required")
	}
	if opt.Report == nil {
		return fmt.Errorf("report is required")
	}
	concurrency := opt.Concurrency
	if concurrency <= 0 {
		concurrency = 8
	}

	entries := make(chan Entry, concurrency)
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		defer close(entries)
		return ScanExport(path, func(e Entry) error {
			select {
			case entries <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})

	for range concurrency {
		g.Go(func() error {
			for entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				success, errMsg, _, err := checkQuery(ctx, opt.Client, entry.Query)
				if err != nil {
					return err
				}
				var groups []string
				if !success && errMsg != "" {
					groups = GroupErrors([]string{errMsg})
				}
				opt.Report.Record(entry.DashboardUID, entry.Query, success, groups)
				if opt.OnQuery != nil {
					opt.OnQuery()
				}
			}
			return nil
		})
	}

	return g.Wait()
}

func checkQuery(ctx context.Context, client *Client, query string) (success bool, errMsg string, httpStatus int, err error) {
	scrubbed := ScrubQuery(query)
	success, errMsg, httpStatus, err = client.QueryRange(ctx, scrubbed)
	if err != nil {
		return false, err.Error(), 0, err
	}
	return success, errMsg, httpStatus, nil
}
