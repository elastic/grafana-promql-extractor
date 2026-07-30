package cli

import (
	"bytes"
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
	// list configures bulk enumeration, which is used when bulk is set.
	list grafana.ListOptions
	// bulk enumerates whole dashboards instead of fetching them one by one.
	bulk bool

	// concurrency is how many dashboards are fetched at once.
	concurrency int
	// failFast aborts the run on the first dashboard that cannot be fetched,
	// instead of counting it and moving on.
	failFast bool
}

// job is one dashboard on its way through the pipeline. A search-based run
// carries only what it takes to fetch the dashboard; a bulk run carries the
// document the listing already delivered.
type job struct {
	uid      string
	title    string
	document []byte
}

func (p *pipeline) run(ctx context.Context) (extract.Stats, error) {
	var stats extract.Stats

	jobs := make(chan job, p.concurrency*4)
	results := make(chan extract.Result, p.concurrency*4)

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		defer close(jobs)
		send := func(j job) error {
			select {
			case jobs <- j:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if p.bulk {
			return p.client.ListDashboards(ctx, p.list, func(doc grafana.DashboardDocument) error {
				// The document is only valid until the next one is decoded,
				// and it outlives this call in the channel. A dashboard the
				// listing named but did not carry arrives without one, and
				// the worker fetches that one by uid.
				return send(job{uid: doc.UID, document: bytes.Clone(doc.Document)})
			})
		}
		return p.client.SearchDashboards(ctx, p.search, func(hit grafana.DashboardHit) error {
			return send(job{uid: hit.UID, title: hit.Title})
		})
	})

	var workers sync.WaitGroup
	for range p.concurrency {
		workers.Add(1)
		group.Go(func() error {
			defer workers.Done()
			for j := range jobs {
				result, err := p.process(ctx, j)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if p.failFast {
						return fmt.Errorf("dashboard %s: %w", j.uid, err)
					}
					p.log.debugf("skipping dashboard %s (%s): %v", j.uid, j.title, err)
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

// process turns one job into queries, downloading the dashboard first unless
// the listing already delivered it. The document is decoded straight from the
// response body and dropped as soon as extraction is done.
func (p *pipeline) process(ctx context.Context, j job) (extract.Result, error) {
	if j.document != nil {
		return p.extract(bytes.NewReader(j.document), j.uid)
	}

	body, err := p.client.DashboardJSON(ctx, j.uid)
	if err != nil {
		return extract.Result{}, err
	}
	defer body.Close()

	result, err := p.extract(body, j.uid)
	// Drain the rest of the body so the connection can be reused.
	io.Copy(io.Discard, body)
	return result, err
}

func (p *pipeline) extract(document io.Reader, uid string) (extract.Result, error) {
	env, err := extract.ParseEnvelope(document)
	partial := errors.Is(err, extract.ErrPartialDecode)
	if err != nil && !partial {
		return extract.Result{}, err
	}

	result := p.extractor.Extract(env)
	// A bulk listing keeps the uid outside the document, so it comes from the
	// job rather than from what was parsed.
	if result.UID == "" {
		result.UID = uid
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
