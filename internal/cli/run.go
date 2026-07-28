package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/anonymize"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/extract"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/grafana"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/output"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/progress"
)

// ErrInterrupted is returned when the run was cancelled by a signal. Output
// written before the interruption is flushed and reported.
var ErrInterrupted = errors.New("interrupted")

func run(cmd *cobra.Command, opts *options) error {
	if err := validate(opts); err != nil {
		return err
	}

	mode, err := progress.ParseMode(opts.progressMode)
	if err != nil {
		return err
	}

	log := &logger{out: cmd.ErrOrStderr(), verbose: opts.verbose}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := grafana.New(grafana.Config{
		BaseURL:            opts.url,
		Token:              opts.token,
		User:               opts.user,
		Password:           opts.password,
		OrgID:              opts.orgID,
		Timeout:            opts.timeout,
		Retries:            opts.retries,
		InsecureSkipVerify: opts.insecureSkipVerify,
		UserAgent:          grafana.DefaultUserAgent + "/" + version,
		Logf:               log.debugf,
	})
	if err != nil {
		return err
	}

	grafanaVersion, err := client.Health(ctx)
	if err != nil {
		return err
	}
	log.debugf("connected to %s (Grafana %s)", client.BaseURL(), grafanaVersion)

	registry, err := grafana.LoadRegistry(ctx, client)
	if err != nil {
		return fmt.Errorf("%w\n\nDatasource types are needed to tell Prometheus queries apart from other "+
			"query languages. Check that the credentials are valid and can read datasources", err)
	}
	log.debugf("resolved %d datasources from %s, default type %q",
		registry.Count, registry.Source, registry.DefaultType())
	if registry.Count == 0 {
		log.warnf("no datasources found; queries will only be classified by the type recorded in each dashboard")
	}

	extractor := &extract.Extractor{
		Lookup:            registry,
		Allowed:           extract.NewTypeSet(opts.datasourceTypes),
		IncludeUnresolved: opts.includeUnresolved,
		Dedupe:            opts.dedupe,
	}

	var anonymizer *anonymize.Anonymizer
	if opts.anonymize {
		if anonymizer, err = anonymize.New(opts.anonymizeSalt); err != nil {
			return err
		}
		log.debugf("anonymizing with %s", saltSource(opts.anonymizeSalt))
	}

	searchOpts := grafana.SearchOptions{
		PageSize:   opts.pageSize,
		Max:        opts.maxDashboards,
		StartPage:  opts.startPage,
		FolderUIDs: opts.folderUIDs,
		Tags:       opts.tags,
	}

	total := opts.maxDashboards
	if total == 0 && opts.precount && mode != progress.ModeNever {
		log.debugf("counting dashboards")
		if total, err = client.CountDashboards(ctx, searchOpts); err != nil {
			if ctx.Err() != nil {
				return ErrInterrupted
			}
			log.warnf("could not count dashboards up front, continuing without a total: %v", err)
			total = 0
		} else {
			log.debugf("found %d dashboards", total)
		}
	}

	tracker := progress.New(cmd.ErrOrStderr(), total, mode)
	log.tracker = tracker
	writer := output.New(output.Options{
		Path:              opts.output,
		Compress:          opts.compress,
		DashboardsPerFile: opts.dashboardsPerFile,
		Append:            opts.appendOutput,
	})

	// Tracked so that an interrupted run can tell the user where to resume.
	var currentPage atomic.Int64
	currentPage.Store(int64(searchOpts.FirstPage()))
	searchOpts.OnPage = func(page int) { currentPage.Store(int64(page)) }

	tracker.Start()
	stats, runErr := pipeline(ctx, client, extractor, anonymizer, writer, tracker, log, searchOpts, opts)
	closeErr := writer.Close()
	tracker.Stop()

	summary(cmd.ErrOrStderr(), tracker, stats, writer.Files())
	interrupted := errors.Is(runErr, context.Canceled) || ctx.Err() != nil
	if runErr != nil && interrupted {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"\nInterrupted on search page %d. Resume with --start-page %d --append; "+
				"the dashboards of that page may repeat.\n",
			currentPage.Load(), currentPage.Load())
		if opts.anonymize && opts.anonymizeSalt == "" {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"A resumed run would pseudonymize differently, since this one used a random salt. "+
					"Start over with --anonymize-salt to get one consistent file.\n")
		}
	}

	if closeErr != nil {
		return closeErr
	}
	if runErr != nil {
		if interrupted {
			return ErrInterrupted
		}
		return runErr
	}
	return nil
}

// pipeline streams dashboards from search into a worker pool and funnels the
// extracted queries through a single writer, so that only a bounded number of
// dashboard documents is ever in memory.
func pipeline(
	ctx context.Context,
	client *grafana.Client,
	extractor *extract.Extractor,
	anonymizer *anonymize.Anonymizer,
	writer *output.Writer,
	tracker *progress.Tracker,
	log *logger,
	searchOpts grafana.SearchOptions,
	opts *options,
) (extract.Stats, error) {
	var stats extract.Stats

	hits := make(chan grafana.DashboardHit, opts.concurrency*4)
	results := make(chan extract.Result, opts.concurrency*4)

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		defer close(hits)
		return client.SearchDashboards(ctx, searchOpts, func(hit grafana.DashboardHit) error {
			select {
			case hits <- hit:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})

	var workers sync.WaitGroup
	for range opts.concurrency {
		workers.Add(1)
		group.Go(func() error {
			defer workers.Done()
			for hit := range hits {
				result, err := fetch(ctx, client, extractor, anonymizer, hit)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if opts.failFast {
						return fmt.Errorf("dashboard %s: %w", hit.UID, err)
					}
					log.debugf("skipping dashboard %s (%s): %v", hit.UID, hit.Title, err)
					tracker.AddFailure()
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
			if err := writer.WriteDashboard(result.UID, result.Queries); err != nil {
				return err
			}
			tracker.AddDashboard(len(result.Queries))
		}
		return nil
	})

	return stats, group.Wait()
}

// fetch downloads one dashboard and extracts its queries. The document is
// decoded straight from the response body and dropped as soon as extraction is
// done.
func fetch(
	ctx context.Context,
	client *grafana.Client,
	extractor *extract.Extractor,
	anonymizer *anonymize.Anonymizer,
	hit grafana.DashboardHit,
) (extract.Result, error) {
	body, err := client.DashboardJSON(ctx, hit.UID)
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

	result := extractor.Extract(env)
	if result.UID == "" {
		result.UID = hit.UID
	}
	if result.Title == "" {
		result.Title = hit.Title
	}
	if partial {
		result.Stats.PartialDecodes = 1
	}
	if anonymizer != nil {
		// Pseudonymize in the worker, so the hashing spreads over the pool.
		result.UID = anonymizer.UID(result.UID)
		for i, query := range result.Queries {
			result.Queries[i] = anonymizer.Query(query)
		}
	}
	return result, nil
}

// saltSource describes where the pseudonyms come from, without echoing the
// salt itself, which is the secret that keeps them irreversible.
func saltSource(salt string) string {
	if salt == "" {
		return "a random salt, so pseudonyms differ from any other run"
	}
	return "the given salt, so pseudonyms match other runs using it"
}

func validate(opts *options) error {
	if strings.TrimSpace(opts.url) == "" {
		return errors.New("a Grafana URL is required: pass --url or set GRAFANA_URL")
	}
	if opts.concurrency < 1 {
		return errors.New("--concurrency must be at least 1")
	}
	if opts.pageSize < 1 {
		return errors.New("--page-size must be at least 1")
	}
	if opts.pageSize > grafana.MaxPageSize {
		return fmt.Errorf("--page-size must not exceed %d, the maximum Grafana accepts", grafana.MaxPageSize)
	}
	if opts.maxDashboards < 0 {
		return errors.New("--max-dashboards must not be negative")
	}
	if opts.dashboardsPerFile < 0 {
		return errors.New("--dashboards-per-file must not be negative")
	}
	if opts.retries < 0 {
		return errors.New("--retries must not be negative")
	}
	if strings.TrimSpace(opts.output) == "" {
		return errors.New("--output must not be empty")
	}
	if len(extract.NewTypeSet(opts.datasourceTypes)) == 0 {
		return errors.New("--datasource-types must list at least one plugin type")
	}
	if opts.anonymizeSalt != "" && !opts.anonymize {
		return errors.New("--anonymize-salt has no effect without --anonymize")
	}
	// A random salt per run would give the appended queries different
	// pseudonyms than the ones already in the file.
	if opts.anonymize && opts.appendOutput && opts.anonymizeSalt == "" {
		return errors.New("--anonymize with --append needs --anonymize-salt, " +
			"so that the queries added now get the same pseudonyms as the ones already written")
	}
	return nil
}

// summary reports what the run produced.
func summary(out io.Writer, tracker *progress.Tracker, stats extract.Stats, files []output.FileInfo) {
	dashboards, queries, failures := tracker.Counts()

	fmt.Fprintf(out, "\nProcessed %s dashboards in %s\n", humanInt(dashboards), tracker.Elapsed().Round(time.Millisecond))
	fmt.Fprintf(out, "  queries written:        %s from %s dashboards\n",
		humanInt(queries), humanInt(countDashboards(files)))
	if stats.Annotations > 0 {
		fmt.Fprintf(out, "  annotation queries:     %s\n", humanInt(stats.Annotations))
	}
	if stats.Duplicates > 0 {
		fmt.Fprintf(out, "  duplicates dropped:     %s\n", humanInt(stats.Duplicates))
	}
	if stats.UnresolvedIncluded > 0 {
		fmt.Fprintf(out, "  unresolved datasource:  %s queries kept, %s dropped\n",
			humanInt(stats.UnresolvedIncluded), humanInt(stats.UnresolvedSkipped))
	}
	if skipped := stats.TopSkippedTypes(5); len(skipped) > 0 {
		parts := make([]string, 0, len(skipped))
		for _, s := range skipped {
			parts = append(parts, fmt.Sprintf("%s %s", s.Type, humanInt(s.Count)))
		}
		fmt.Fprintf(out, "  skipped by datasource:  %s\n", strings.Join(parts, ", "))
	}
	if stats.LibraryPanels > 0 {
		fmt.Fprintf(out, "  library panels:         %s (their queries are stored outside the dashboard)\n", humanInt(stats.LibraryPanels))
	}
	if stats.PartialDecodes > 0 {
		fmt.Fprintf(out, "  partially decoded:      %s dashboards\n", humanInt(stats.PartialDecodes))
	}
	if failures > 0 {
		fmt.Fprintf(out, "  failed dashboards:      %s (re-run with --verbose to see why)\n", humanInt(failures))
	}

	if len(files) == 0 {
		fmt.Fprintf(out, "  no output files written\n")
		return
	}
	fmt.Fprintf(out, "  files:\n")
	for _, f := range files {
		fmt.Fprintf(out, "    %s (%s queries from %s dashboards)\n", f.Path, humanInt(f.Queries), humanInt(f.Dashboards))
	}
}

func countDashboards(files []output.FileInfo) int {
	n := 0
	for _, f := range files {
		n += f.Dashboards
	}
	return n
}

func humanInt(n int) string { return progress.HumanInt(n) }

// logger routes warnings around the progress line so they are not overwritten.
type logger struct {
	out     io.Writer
	verbose bool
	tracker *progress.Tracker
}

func (l *logger) warnf(format string, args ...any) {
	if l.tracker != nil {
		l.tracker.Logf(format, args...)
		return
	}
	fmt.Fprintf(l.out, format+"\n", args...)
}

func (l *logger) debugf(format string, args ...any) {
	if l.verbose {
		l.warnf(format, args...)
	}
}
