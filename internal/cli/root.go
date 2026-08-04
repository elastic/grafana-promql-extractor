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
		Short: "Extract PromQL queries from Grafana dashboards",
		Long: strings.TrimSpace(`
Extract PromQL queries from the dashboards of a Grafana instance.

Use the extract subcommand to pull queries into a flat file.`),
		Version:           version,
		Args:              cobra.NoArgs,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = cmd.Help()
			return errors.New("specify a subcommand: extract")
		},
	}

	cmd.AddCommand(newExtractCmd())

	return cmd
}

// Execute runs the command.
func Execute() error {
	return NewRootCmd().Execute()
}
