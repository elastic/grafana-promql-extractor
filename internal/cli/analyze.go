package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
	"github.com/elastic/grafana-promql-extractor/internal/progress"
)

var analyzeEnvForFlag = map[string]string{
	"es-version": "ES_VERSION",
	"es-image":   "ES_IMAGE",
}

type analyzeOptions struct {
	input  string
	output string

	esVersion string
	esImage   string
	index     string

	concurrency int
	timeout     time.Duration

	progress string
}

func newAnalyzeCmd() *cobra.Command {
	opts := &analyzeOptions{}

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Check extracted PromQL queries against Elasticsearch",
		Long: strings.TrimSpace(`
Read a query export file and send each PromQL expression to an Elasticsearch
Prometheus-compatible query_range endpoint. A query counts as supported when the
endpoint returns status success, even if the result is empty.

Unsupported language constructs return an error that is grouped in the report.`),
		Example: strings.Trim(`
  grafana-promql-extractor analyze -i promql-queries.txt.gz -o report.md --es-version 9.5.0

  grafana-promql-extractor analyze -i shareable.txt.gz -o coverage.md --es-image docker.elastic.co/elasticsearch/elasticsearch:9.5.0-SNAPSHOT`, "\n"),
		Args:              cobra.NoArgs,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return applyAnalyzeEnv(cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAnalyze(cmd, opts)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&opts.input, "input", "i", "", "query export file to analyze")
	f.StringVarP(&opts.output, "output", "o", "-", "markdown report path, or - for stdout")
	f.StringVar(&opts.esVersion, "es-version", "", "start Elasticsearch in Docker at this version, e.g. 9.5.0 [ES_VERSION]")
	f.StringVar(&opts.esImage, "es-image", "", "start Elasticsearch from this container image; mutually exclusive with --es-version [ES_IMAGE]")
	f.StringVar(&opts.index, "index", "", "optional index expression for /_prometheus/{index}/api/v1/query_range")
	f.IntVarP(&opts.concurrency, "concurrency", "c", 8, "number of queries to check in parallel")
	f.DurationVar(&opts.timeout, "timeout", 30*time.Second, "per-request timeout")
	f.StringVar(&opts.progress, "progress", string(progress.ModeAuto), "progress output: auto, always or never")

	_ = cmd.MarkFlagRequired("input")

	return cmd
}

func applyAnalyzeEnv(cmd *cobra.Command) error {
	for name, env := range analyzeEnvForFlag {
		flag := cmd.Flags().Lookup(name)
		if flag == nil || flag.Changed {
			continue
		}
		value := strings.TrimSpace(os.Getenv(env))
		if value == "" {
			continue
		}
		if err := cmd.Flags().Set(name, value); err != nil {
			return fmt.Errorf("invalid value for %s: %w", env, err)
		}
	}
	return nil
}

func resolveAnalyzeImage(version, image string) (string, error) {
	version = strings.TrimSpace(version)
	image = strings.TrimSpace(image)
	switch {
	case version != "" && image != "":
		return "", fmt.Errorf("--es-version and --es-image are mutually exclusive")
	case image != "":
		return image, nil
	case version != "":
		return analyze.ResolveImage(version), nil
	default:
		return "", fmt.Errorf("--es-version or --es-image is required (or set ES_VERSION / ES_IMAGE)")
	}
}

func runAnalyze(cmd *cobra.Command, opts *analyzeOptions) error {
	if strings.TrimSpace(opts.input) == "" {
		return fmt.Errorf("--input is required")
	}
	image, err := resolveAnalyzeImage(opts.esVersion, opts.esImage)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Hand the signal back to the runtime once the first one has arrived, so
	// that a second Ctrl-C aborts a shutdown that is taking too long instead of
	// being swallowed as well.
	go func() {
		<-ctx.Done()
		stop()
	}()

	progressMode, err := progress.ParseMode(opts.progress)
	if err != nil {
		return err
	}

	collector := analyze.NewSeriesCollector()
	queryCount := 0
	if err := analyze.ScanExport(opts.input, func(e analyze.Entry) error {
		collector.AddQuery(e.Query)
		queryCount++
		return nil
	}); err != nil {
		return err
	}
	if queryCount == 0 {
		return fmt.Errorf("%s contains no queries", opts.input)
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "starting Elasticsearch %s in Docker...\n", image)
	cluster, err := analyze.StartElasticsearch(ctx, image, collector.Series())
	if err != nil {
		return err
	}
	defer func() {
		if err := cluster.Close(context.Background()); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: stopping Elasticsearch container: %v\n", err)
		}
	}()
	if cluster.SeededSeries > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "seeded %d time series via remote write\n", cluster.SeededSeries)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Elasticsearch ready at %s (data stream %s)\n", cluster.URL, analyze.DefaultPrometheusDataStream)

	client, err := analyze.NewClient(analyze.ClientConfig{
		BaseURL:   cluster.URL,
		Index:     opts.index,
		Start:     cluster.QueryStart.Format(time.RFC3339),
		End:       cluster.QueryEnd.Format(time.RFC3339),
		Timeout:   opts.timeout,
		UserAgent: "grafana-promql-extractor/" + version,
	})
	if err != nil {
		return err
	}

	report := analyze.NewReport()
	tracker := progress.New(cmd.ErrOrStderr(), queryCount, progressMode)
	tracker.SetUnit(progress.UnitQueries)
	tracker.Start()
	defer tracker.Stop()

	if err := analyze.StreamAnalyze(ctx, opts.input, analyze.StreamOptions{
		Client:      client,
		Concurrency: opts.concurrency,
		Report:      report,
		OnQuery: func() {
			tracker.AddQuery()
		},
	}); err != nil {
		return err
	}

	var out *os.File
	if opts.output == "-" {
		out = os.Stdout
	} else {
		out, err = os.Create(opts.output)
		if err != nil {
			return err
		}
		defer out.Close()
	}

	if err := report.WriteMarkdown(out); err != nil {
		return err
	}

	successful := report.SuccessfulQueries()
	total := report.TotalQueries()
	fmt.Fprintf(cmd.ErrOrStderr(), "%d/%d queries supported by Elasticsearch (%.1f%%)\n",
		successful, total, float64(successful)*100/float64(total))
	return nil
}
