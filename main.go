// Command grafana-promql-extractor extracts PromQL from Grafana dashboards
// and can analyze export files against Elasticsearch PromQL.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/elastic/grafana-promql-extractor/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		if errors.Is(err, cli.ErrInterrupted) {
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
