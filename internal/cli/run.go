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

	"github.com/felixbarny/grafana-promql-extractor/internal/anonymize"
	"github.com/felixbarny/grafana-promql-extractor/internal/extract"
	"github.com/felixbarny/grafana-promql-extractor/internal/grafana"
	"github.com/felixbarny/grafana-promql-extractor/internal/output"
	"github.com/felixbarny/grafana-promql-extractor/internal/progress"
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
	listOpts := grafana.ListOptions{
		PageSize:      opts.pageSize,
		Max:           opts.maxDashboards,
		ContinueToken: opts.continueToken,
	}

	bulk, err := chooseBulk(ctx, client, opts, log)
	if err != nil {
		return err
	}
	if bulk {
		log.debugf("reading whole dashboards in pages of up to %d", listOpts.PageSize)
	} else {
		log.debugf("fetching dashboards one by one")
	}

	// A listing is checked against the instance once it is done, which a run
	// that only wants a sample has no use for, and a resumed one cannot do:
	// the dashboards before the token were delivered by the earlier run, and
	// this one has no way of knowing which those were.
	verify := bulk && opts.maxDashboards == 0 && opts.continueToken == ""
	if bulk && !verify {
		log.warnf("a listing cannot be checked against the instance when it is %s, "+
			"so dashboards Grafana leaves out of a page will be missing from the output",
			listingLimitedBy(opts))
	}

	total := opts.maxDashboards
	// counted stays zero unless the dashboards were actually counted, which is
	// what makes it worth comparing the run against afterwards.
	countFirst := opts.precount && mode != progress.ModeNever
	counted := 0
	if total == 0 && countFirst {
		log.debugf("counting dashboards")
		if total, err = client.CountDashboards(ctx, searchOpts); err != nil {
			if ctx.Err() != nil {
				return ErrInterrupted
			}
			log.warnf("could not count dashboards up front, continuing without a total: %v", err)
			total = 0
		} else {
			counted = total
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
	var currentToken atomic.Pointer[string]
	currentToken.Store(&opts.continueToken)
	listOpts.OnPage = func(token string) { currentToken.Store(&token) }

	extraction := &pipeline{
		client:      client,
		extractor:   extractor,
		anonymizer:  anonymizer,
		writer:      writer,
		tracker:     tracker,
		log:         log,
		search:      searchOpts,
		list:        listOpts,
		bulk:        bulk,
		verify:      verify,
		concurrency: opts.concurrency,
		failFast:    opts.failFast,
	}

	tracker.Start()
	stats, runErr := extraction.run(ctx)
	closeErr := writer.Close()
	tracker.Stop()

	summary(cmd.ErrOrStderr(), tracker, stats, writer.Files(), expectation{
		counted:  counted,
		repaired: extraction.repaired,
	})
	interrupted := errors.Is(runErr, context.Canceled) || ctx.Err() != nil
	if runErr != nil {
		resumeHint(cmd.ErrOrStderr(), resumePosition{
			bulk:  bulk,
			page:  int(currentPage.Load()),
			token: *currentToken.Load(),
		}, interrupted, opts)
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
	// Whatever the pages left out was handed to the workers afterwards, so a
	// dashboard still missing here got past both, and the output is short in a
	// way this run cannot account for.
	if delivered, _, failed := tracker.Counts(); verify && extraction.enumerated > delivered+failed {
		return fmt.Errorf("Grafana delivered %s of the %s it holds, and fetching the rest did not "+
			"make up the difference; re-run with --bulk off to fetch them one by one",
			count(delivered+failed, "dashboard", "dashboards"),
			count(extraction.enumerated, "dashboard", "dashboards"))
	}
	return nil
}

// listingLimitedBy names why a listing has to be taken at its word.
func listingLimitedBy(opts *options) string {
	if opts.continueToken != "" {
		return "resumed with --continue-token"
	}
	return "cut short by --max-dashboards"
}

// bulk modes.
const (
	bulkAuto = "auto"
	bulkOn   = "on"
	bulkOff  = "off"
)

// chooseBulk decides how dashboards are enumerated. Reading them in pages
// returns whole dashboards, which turns one request per dashboard into one per
// page, but Grafana only serves it from version 12, it cannot filter, and its
// paging is a chain of tokens rather than numbered pages.
//
// Pages can also come back missing a batch of dashboards while still answering
// 200, so a run that uses them checks the result against /api/search afterwards
// and fetches whatever was left out. That makes the fast path safe to take
// without being asked, which is why the default is auto.
func chooseBulk(ctx context.Context, client *grafana.Client, opts *options, log *logger) (bool, error) {
	mode := opts.bulk
	// A continue token means nothing to the search API, so anything but a
	// listing would silently start the run over.
	if opts.continueToken != "" {
		mode = bulkOn
	}
	if mode == bulkOff {
		return false, nil
	}

	unsupported := bulkUnsupportedFor(opts)
	if mode == bulkOn {
		if unsupported != "" {
			return false, fmt.Errorf("--bulk=on cannot be combined with %s", unsupported)
		}
		available, err := client.BulkAvailable(ctx)
		if err != nil {
			return false, err
		}
		if !available {
			return false, fmt.Errorf("--bulk=on, but %w; it needs Grafana 12 or later",
				grafana.ErrBulkUnavailable)
		}
		return true, nil
	}

	if unsupported != "" {
		log.debugf("pages cannot honor %s", unsupported)
		return false, nil
	}
	available, err := client.BulkAvailable(ctx)
	if err != nil {
		// Enumeration works either way, so a probe that fails for its own
		// reasons must not end the run.
		log.debugf("could not tell whether this Grafana serves pages of dashboards: %v", err)
		return false, nil
	}
	return available, nil
}

// bulkUnsupportedFor names the option that rules bulk listing out, if any.
func bulkUnsupportedFor(opts *options) string {
	switch {
	case len(opts.folderUIDs) > 0:
		return "--folder-uid"
	case len(opts.tags) > 0:
		return "--tag"
	case opts.startPage > 1:
		return "--start-page, which numbers search pages"
	default:
		return ""
	}
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
	switch opts.bulk {
	case bulkAuto, bulkOn, bulkOff:
	default:
		return fmt.Errorf("--bulk must be %s, %s or %s, not %q", bulkAuto, bulkOn, bulkOff, opts.bulk)
	}
	if opts.continueToken != "" {
		if reason := bulkUnsupportedFor(opts); reason != "" {
			return fmt.Errorf("--continue-token resumes a bulk listing and cannot be combined with %s", reason)
		}
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
