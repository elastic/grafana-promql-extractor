package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/cli"
)

func TestAnalyzeRequiresESTarget(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "queries.txt")
	if err := os.WriteFile(input, []byte("d1;up\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := cli.NewRootCmd()
	cmd.SetArgs([]string{"analyze", "-i", input, "--progress", "never"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error without --es-version or --es-image")
	}
	if !strings.Contains(err.Error(), "--es-version or --es-image") {
		t.Fatalf("error = %v", err)
	}
}

func TestAnalyzeRejectsBothESVersionAndImage(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "queries.txt")
	if err := os.WriteFile(input, []byte("d1;up\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := cli.NewRootCmd()
	cmd.SetArgs([]string{
		"analyze", "-i", input,
		"--es-version", "9.5.0",
		"--es-image", "docker.elastic.co/elasticsearch/elasticsearch:9.5.0",
		"--progress", "never",
	})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected mutual exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v", err)
	}
}
