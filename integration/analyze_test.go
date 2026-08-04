//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
	"github.com/elastic/grafana-promql-extractor/internal/cli"
)

func TestAnalyzeCLI(t *testing.T) {
	requireDocker(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "queries.txt")
	output := filepath.Join(dir, "report.md")
	if err := os.WriteFile(input, []byte("d1;up\nd2;3.14\n"), 0o644); err != nil {
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
	if !strings.Contains(stderr.String(), "2/2 queries supported by Elasticsearch") {
		t.Fatalf("missing stderr summary:\n%s", stderr.String())
	}
}

func esImage() string {
	if img := strings.TrimSpace(os.Getenv("ES_IMAGE")); img != "" {
		return img
	}
	version := strings.TrimSpace(os.Getenv("ES_VERSION"))
	if version == "" {
		version = "9.4.4"
	}
	return analyze.ResolveImage(version)
}
