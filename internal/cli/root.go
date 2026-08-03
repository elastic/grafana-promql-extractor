package cli

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags.
var version = "dev"

// NewRootCmd builds the command. It is exported so tests can drive the real
// command in process.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grafana-promql-extractor",
		Short: "Extract and analyze PromQL queries from Grafana dashboards",
		Long: strings.TrimSpace(`
Extract PromQL queries from Grafana dashboards, and optionally check them against
an Elasticsearch PromQL endpoint.

Use the extract subcommand to pull queries from a Grafana instance into a flat
file. Use analyze to run those queries against Elasticsearch and report which
expressions the release accepts.`),
		Version:           version,
		Args:              cobra.NoArgs,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = cmd.Help()
			return errors.New("specify a subcommand: extract or analyze")
		},
	}

	cmd.AddCommand(newExtractCmd())
	cmd.AddCommand(newAnalyzeCmd())

	return cmd
}

// Execute runs the command.
func Execute() error {
	return NewRootCmd().Execute()
}
