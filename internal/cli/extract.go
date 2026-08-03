package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/elastic/grafana-promql-extractor/internal/extract"
	"github.com/elastic/grafana-promql-extractor/internal/grafana"
	"github.com/elastic/grafana-promql-extractor/internal/progress"
)

// options holds every extract command line setting.
type options struct {
	url      string
	token    string
	user     string
	password string
	orgID    int

	output            string
	compress          bool
	appendOutput      bool
	maxDashboards     int
	dashboardsPerFile int

	concurrency   int
	pageSize      int
	startPage     int
	folderUIDs    []string
	tags          []string
	bulk          string
	continueToken string

	datasourceTypes   []string
	includeUnresolved bool
	dedupe            bool

	anonymize     bool
	anonymizeSalt string

	timeout            time.Duration
	retries            int
	insecureSkipVerify bool

	progressMode string
	precount     bool
	verbose      bool
	failFast     bool
}

// envForFlag maps extract flags to the environment variables that can supply them.
var envForFlag = map[string]string{
	"url":            "GRAFANA_URL",
	"token":          "GRAFANA_TOKEN",
	"user":           "GRAFANA_USER",
	"password":       "GRAFANA_PASSWORD",
	"org-id":         "GRAFANA_ORG_ID",
	"anonymize-salt": "GRAFANA_ANONYMIZE_SALT",
}

func newExtractCmd() *cobra.Command {
	opts := &options{}

	cmd := &cobra.Command{
		Use:   "extract",
		Short: "Extract PromQL queries from Grafana dashboards",
		Long: strings.TrimSpace(`
Extract PromQL queries from the dashboards of a Grafana instance.

Dashboards are enumerated page by page and fetched concurrently, so memory use
stays flat regardless of how many dashboards the instance holds. Output is one
query per line, prefixed by the dashboard UID and a semicolon:

  cdf6f5b7;sum(rate(http_requests_total[5m]))

Only targets backed by a Prometheus-family datasource are extracted. Panel and
target datasource references are resolved against the instance's datasources and
the dashboard's own datasource variables.`),
		Example: strings.Trim(`
  # Everything, gzipped, into promql-queries.txt.gz
  export GRAFANA_URL=https://grafana.example.com
  export GRAFANA_TOKEN=glsa_xxx
  grafana-promql-extractor extract

  # A sample of 500 dashboards, uncompressed
  grafana-promql-extractor extract --max-dashboards 500 --compress=false -o sample.txt

  # 50k dashboards split into files of 10k dashboards each
  grafana-promql-extractor extract --dashboards-per-file 10000

  # Pseudonymized, so the queries can be shared outside the organization
  grafana-promql-extractor extract --anonymize -o shareable.txt

  # One request per dashboard, for an instance that answers a listing oddly
  grafana-promql-extractor extract --bulk off`, "\n"),
		Args:              cobra.NoArgs,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return applyExtractEnv(cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.url, "url", "", "Grafana base URL, e.g. https://grafana.example.com [GRAFANA_URL]")
	f.StringVar(&opts.token, "token", "", "Grafana service account token [GRAFANA_TOKEN]")
	f.StringVarP(&opts.user, "user", "u", "", "username for basic auth, used when no token is given [GRAFANA_USER]")
	f.StringVar(&opts.password, "password", "", "password for basic auth [GRAFANA_PASSWORD]")
	f.IntVar(&opts.orgID, "org-id", 0, "Grafana organization id to query [GRAFANA_ORG_ID]")

	f.StringVarP(&opts.output, "output", "o", "promql-queries.txt", "output file path")
	f.BoolVar(&opts.compress, "compress", true, "gzip the output, appending .gz to the path")
	f.BoolVar(&opts.appendOutput, "append", false, "add to existing output files instead of replacing them")
	f.IntVarP(&opts.maxDashboards, "max-dashboards", "n", 0, "maximum number of dashboards to export, 0 for all")
	f.IntVar(&opts.dashboardsPerFile, "dashboards-per-file", 0, "split the output after this many dashboards, 0 for a single file")

	f.IntVarP(&opts.concurrency, "concurrency", "c", 8, "number of dashboards to fetch in parallel")
	f.IntVar(&opts.pageSize, "page-size", grafana.DefaultPageSize, fmt.Sprintf("dashboards per search request, max %d", grafana.MaxPageSize))
	f.IntVar(&opts.startPage, "start-page", 1, "first search page to fetch, to resume an interrupted run; combine with --append")
	f.StringSliceVar(&opts.folderUIDs, "folder-uid", nil, "only export dashboards in these folders, repeatable")
	f.StringSliceVar(&opts.tags, "tag", nil, "only export dashboards carrying these tags, repeatable")
	f.StringVar(&opts.bulk, "bulk", bulkAuto,
		"read dashboards in pages of whole documents where Grafana serves them, instead of one request each: auto, on or off")
	f.StringVar(&opts.continueToken, "continue-token", "",
		"resume an interrupted bulk run at the page this token points at; combine with --append")

	f.StringSliceVar(&opts.datasourceTypes, "datasource-types", extract.DefaultDatasourceTypes, "datasource plugin types to treat as PromQL sources")
	f.BoolVar(&opts.includeUnresolved, "include-unresolved", true, "keep queries whose datasource type cannot be determined")
	f.BoolVar(&opts.dedupe, "dedupe", true, "drop repeated identical queries within a dashboard")

	f.BoolVar(&opts.anonymize, "anonymize", false,
		"replace metric names, label names, label values, variable names and dashboard UIDs with pseudonyms, so the output can be shared")
	f.StringVar(&opts.anonymizeSalt, "anonymize-salt", "",
		"secret that determines the pseudonyms, for comparing anonymized runs; random per run by default [GRAFANA_ANONYMIZE_SALT]")

	f.DurationVar(&opts.timeout, "timeout", 30*time.Second, "per-request timeout")
	f.IntVar(&opts.retries, "retries", 4, "retries per request for rate limits and server errors")
	f.BoolVar(&opts.insecureSkipVerify, "insecure-skip-verify", false, "skip TLS certificate verification")

	f.StringVar(&opts.progressMode, "progress", string(progress.ModeAuto), "progress output: auto, always or never")
	f.BoolVar(&opts.precount, "precount", true, "count dashboards first so progress can show a percentage and an ETA")
	f.BoolVarP(&opts.verbose, "verbose", "v", false, "log per-dashboard failures and retries")
	f.BoolVar(&opts.failFast, "fail-fast", false, "abort on the first dashboard that cannot be fetched")

	return cmd
}

func applyExtractEnv(cmd *cobra.Command) error {
	for name, env := range envForFlag {
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
