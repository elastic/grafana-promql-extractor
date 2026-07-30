package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/felixbarny/grafana-promql-extractor/internal/anonymize"
	"github.com/felixbarny/grafana-promql-extractor/internal/extract"
	"github.com/felixbarny/grafana-promql-extractor/internal/grafana"
	"github.com/felixbarny/grafana-promql-extractor/internal/output"
	"github.com/felixbarny/grafana-promql-extractor/internal/progress"
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
	// verify checks a listing against /api/search afterwards and fetches
	// whatever it left out.
	verify bool

	// concurrency is how many dashboards are fetched at once.
	concurrency int
	// failFast aborts the run on the first dashboard that cannot be fetched,
	// instead of counting it and moving on.
	failFast bool

	// enumerated and repaired report what the check after a listing found.
	// They are written by the producer and read once the run is over.
	enumerated int
	repaired   int
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
			return p.listDashboards(ctx, send)
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

// listDashboards reads whole dashboards in pages and then, unless the run is a
// sample or a resumed one, picks up whatever the pages left out.
func (p *pipeline) listDashboards(ctx context.Context, send func(job) error) error {
	// Grafana assembles a page by reading a batch of dashboards and checking
	// which of them the caller may see, and when that check fails it drops the
	// whole batch while still answering 200. Remembering what arrived is what
	// makes the difference visible afterwards; a uid is a few dozen bytes, so
	// even a very large instance costs a few megabytes for the length of the
	// run.
	delivered := make(map[string]struct{})
	err := p.client.ListDashboards(ctx, p.list, func(doc grafana.DashboardDocument) error {
		delivered[doc.UID] = struct{}{}
		// The document is only valid until the next one is decoded, and it
		// outlives this call in the channel. A dashboard the listing named but
		// did not carry arrives without one, and the worker fetches that one
		// by uid.
		return send(job{uid: doc.UID, document: bytes.Clone(doc.Document)})
	})
	if err != nil || !p.verify {
		return err
	}
	return p.fetchMissing(ctx, delivered, send)
}

// fetchMissing asks /api/search what the instance holds and hands over every
// dashboard the listing did not deliver. Search answers from a different code
// path, one that has not been seen to lose dashboards, so a page Grafana
// dropped costs the run the time to fetch those dashboards one by one rather
// than the queries in them.
func (p *pipeline) fetchMissing(ctx context.Context, delivered map[string]struct{}, send func(job) error) error {
	enumerate := p.search
	enumerate.OnPage = nil
	enumerate.Max = 0
	enumerate.StartPage = 0
	enumerate.PageSize = grafana.MaxPageSize

	err := p.client.SearchDashboards(ctx, enumerate, func(hit grafana.DashboardHit) error {
		p.enumerated++
		if _, ok := delivered[hit.UID]; ok {
			return nil
		}
		p.repaired++
		return send(job{uid: hit.UID, title: hit.Title})
	})
	if err != nil {
		return fmt.Errorf("checking the listing against what the instance holds: %w", err)
	}
	if p.repaired > 0 {
		p.log.warnf("Grafana left %s out of the pages it returned; fetching those one by one",
			count(p.repaired, "dashboard", "dashboards"))
	}
	return nil
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
