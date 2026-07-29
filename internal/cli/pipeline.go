package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/anonymize"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/extract"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/grafana"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/output"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/progress"
)

// pipeline streams dashboards from search into a worker pool and funnels the
// extracted queries through a single writer, so that only a bounded number of
// dashboard documents is ever in memory.
type pipeline struct {
	client     *grafana.Client
	extractor  *extract.Extractor
	anonymizer *anonymize.Anonymizer
	writer     *output.Writer
	tracker    *progress.Tracker
	log        *logger
	search     grafana.SearchOptions

	// concurrency is how many dashboards are fetched at once.
	concurrency int
	// failFast aborts the run on the first dashboard that cannot be fetched,
	// instead of counting it and moving on.
	failFast bool
}

func (p *pipeline) run(ctx context.Context) (extract.Stats, error) {
	var stats extract.Stats

	hits := make(chan grafana.DashboardHit, p.concurrency*4)
	results := make(chan extract.Result, p.concurrency*4)

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		defer close(hits)
		return p.client.SearchDashboards(ctx, p.search, func(hit grafana.DashboardHit) error {
			select {
			case hits <- hit:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})

	var workers sync.WaitGroup
	for range p.concurrency {
		workers.Add(1)
		group.Go(func() error {
			defer workers.Done()
			for hit := range hits {
				result, err := p.fetch(ctx, hit)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if p.failFast {
						return fmt.Errorf("dashboard %s: %w", hit.UID, err)
					}
					p.log.debugf("skipping dashboard %s (%s): %v", hit.UID, hit.Title, err)
					p.tracker.AddFailure()
					continue
				}
				select {
				case results <- result:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}

	group.Go(func() error {
		workers.Wait()
		close(results)
		return nil
	})

	group.Go(func() error {
		for result := range results {
			stats.Merge(result.Stats)
			if err := p.writer.WriteDashboard(result.UID, result.Queries); err != nil {
				return err
			}
			p.tracker.AddDashboard(len(result.Queries))
		}
		return nil
	})

	// Wait first: it is what synchronizes the writer goroutine's stats with
	// this one, and a return statement may evaluate stats before the call.
	err := group.Wait()
	return stats, err
}

// fetch downloads one dashboard and extracts its queries. The document is
// decoded straight from the response body and dropped as soon as extraction is
// done.
func (p *pipeline) fetch(ctx context.Context, hit grafana.DashboardHit) (extract.Result, error) {
	body, err := p.client.DashboardJSON(ctx, hit.UID)
	if err != nil {
		return extract.Result{}, err
	}
	defer body.Close()

	env, err := extract.ParseEnvelope(body)
	partial := errors.Is(err, extract.ErrPartialDecode)
	if err != nil && !partial {
		return extract.Result{}, err
	}
	// Drain the rest of the body so the connection can be reused.
	io.Copy(io.Discard, body)

	result := p.extractor.Extract(env)
	if result.UID == "" {
		result.UID = hit.UID
	}
	if partial {
		result.Stats.PartialDecodes = 1
	}
	if p.anonymizer != nil {
		// Pseudonymize in the worker, so the hashing spreads over the pool.
		result.UID = p.anonymizer.UID(result.UID)
		for i, query := range result.Queries {
			result.Queries[i] = p.anonymizer.Query(query)
		}
	}
	return result, nil
}
