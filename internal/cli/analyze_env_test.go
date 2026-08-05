package cli

import (
	"testing"
)

func TestApplyAnalyzeEnvCLIVersionBeatsImageEnv(t *testing.T) {
	t.Setenv("ES_IMAGE", "docker.elastic.co/elasticsearch/elasticsearch:9.9.9")
	t.Setenv("ES_VERSION", "")

	cmd := newAnalyzeCmd()
	if err := cmd.ParseFlags([]string{"-i", "queries.txt", "--es-version", "9.5.0"}); err != nil {
		t.Fatal(err)
	}
	if err := applyAnalyzeEnv(cmd); err != nil {
		t.Fatal(err)
	}

	version := cmd.Flag("es-version").Value.String()
	image := cmd.Flag("es-image").Value.String()
	got, err := resolveAnalyzeImage(version, image)
	if err != nil {
		t.Fatalf("resolveAnalyzeImage: %v", err)
	}
	want := "docker.elastic.co/elasticsearch/elasticsearch:9.5.0"
	if got != want {
		t.Fatalf("image = %q, want %q (version=%q image=%q)", got, want, version, image)
	}
}

func TestApplyAnalyzeEnvCLIImageBeatsVersionEnv(t *testing.T) {
	t.Setenv("ES_VERSION", "9.5.0")
	t.Setenv("ES_IMAGE", "")

	want := "docker.elastic.co/elasticsearch/elasticsearch:9.9.9-SNAPSHOT"
	cmd := newAnalyzeCmd()
	if err := cmd.ParseFlags([]string{"-i", "queries.txt", "--es-image", want}); err != nil {
		t.Fatal(err)
	}
	if err := applyAnalyzeEnv(cmd); err != nil {
		t.Fatal(err)
	}

	version := cmd.Flag("es-version").Value.String()
	image := cmd.Flag("es-image").Value.String()
	got, err := resolveAnalyzeImage(version, image)
	if err != nil {
		t.Fatalf("resolveAnalyzeImage: %v", err)
	}
	if got != want {
		t.Fatalf("image = %q, want %q (version=%q image=%q)", got, want, version, image)
	}
}
