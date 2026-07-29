package cli

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

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
	// Hand the signal back to the runtime once the first one has arrived, so
	// that a second Ctrl-C aborts a shutdown that is taking too long instead of
	// being swallowed as well.
	go func() {
		<-ctx.Done()
		stop()
	}()

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
		// One connection per worker, plus the one the search loop runs on.
		MaxIdleConns: opts.concurrency + 1,
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

	// Tracked so that a run which does not finish can tell the user where to
	// resume.
	var currentPage atomic.Int64
	currentPage.Store(int64(searchOpts.FirstPage()))
	searchOpts.OnPage = func(page int) { currentPage.Store(int64(page)) }

	extraction := &pipeline{
		client:      client,
		extractor:   extractor,
		anonymizer:  anonymizer,
		writer:      writer,
		tracker:     tracker,
		log:         log,
		search:      searchOpts,
		concurrency: opts.concurrency,
		failFast:    opts.failFast,
	}

	tracker.Start()
	stats, runErr := extraction.run(ctx)
	closeErr := writer.Close()
	tracker.Stop()

	summary(cmd.ErrOrStderr(), tracker, stats, writer.Files())
	interrupted := errors.Is(runErr, context.Canceled) || ctx.Err() != nil
	if runErr != nil {
		resumeHint(cmd.ErrOrStderr(), int(currentPage.Load()), interrupted, opts)
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
