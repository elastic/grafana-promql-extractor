//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
	"github.com/elastic/grafana-promql-extractor/internal/cli"
)

func TestAnalyzeCLI(t *testing.T) {
	requireDocker(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "queries.txt")
	output := filepath.Join(dir, "report.md")
	if err := os.WriteFile(input, []byte("d1;up\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := cli.NewRootCmd()
	var stderr bytes.Buffer
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"analyze",
		"-i", input,
		"-o", output,
		"--es-image", esImage(),
		"--progress", "never",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("analyze failed: %v\n%s", err, stderr.String())
	}

	report, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "Successful Queries") {
		t.Fatalf("missing report summary:\n%s", report)
	}
	if !strings.Contains(stderr.String(), "queries supported by Elasticsearch") {
		t.Fatalf("missing stderr summary:\n%s", stderr.String())
	}
}

func TestAnalyzeWithDockerElasticsearch(t *testing.T) {
	requireDocker(t)

	ctx := context.Background()
	collector := analyze.NewSeriesCollector()
	collector.AddQuery("up")
	collector.AddQuery("3.14")

	cluster, err := analyze.StartElasticsearch(ctx, esImage(), collector.Series())
	if err != nil {
		t.Fatalf("starting Elasticsearch: %v", err)
	}
	t.Cleanup(func() {
		if err := cluster.Close(context.Background()); err != nil {
			t.Logf("stopping Elasticsearch: %v", err)
		}
	})

	dir := t.TempDir()
	input := filepath.Join(dir, "queries.txt")
	if err := os.WriteFile(input, []byte("d1;up\nd2;3.14\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := analyze.NewClient(analyze.ClientConfig{
		BaseURL: cluster.URL,
		Start:   cluster.QueryStart.Format(time.RFC3339),
		End:     cluster.QueryEnd.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}

	report := analyze.NewReport()
	if err := analyze.StreamAnalyze(ctx, input, analyze.StreamOptions{
		Client:      client,
		Concurrency: 2,
		Report:      report,
	}); err != nil {
		t.Fatal(err)
	}
	if report.TotalQueries() != 2 {
		t.Fatalf("got %d results", report.TotalQueries())
	}
	if report.SuccessfulQueries() != 2 {
		t.Fatalf("only %d/%d queries succeeded", report.SuccessfulQueries(), report.TotalQueries())
	}
}

func esImage() string {
	if img := strings.TrimSpace(os.Getenv("ES_IMAGE")); img != "" {
		return img
	}
	version := strings.TrimSpace(os.Getenv("ES_VERSION"))
	if version == "" {
		version = "9.5.0"
	}
	return analyze.ResolveImage(version)
}
