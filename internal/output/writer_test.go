package output_test

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/output"
)

func TestWritesPlainFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")

	writer := output.New(output.Options{Path: path})
	mustWrite(t, writer, "dash1", []string{"sum(rate(http_requests_total[5m]))"})
	mustWrite(t, writer, "dash2", []string{`http_requests_total{job="api-server"}`})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := "dash1;sum(rate(http_requests_total[5m]))\ndash2;http_requests_total{job=\"api-server\"}\n"
	if got := readFile(t, path); got != want {
		t.Errorf("file contents:\n%q\nwant:\n%q", got, want)
	}

	files := writer.Files()
	if len(files) != 1 {
		t.Fatalf("wrote %d files, want 1", len(files))
	}
	if files[0].Dashboards != 2 || files[0].Queries != 2 {
		t.Errorf("file info = %+v, want 2 dashboards and 2 queries", files[0])
	}
}

func TestCompressesAndAppendsExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")

	writer := output.New(output.Options{Path: path, Compress: true})
	mustWrite(t, writer, "dash1", []string{"up"})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("the uncompressed path should not exist")
	}
	if got := readGzip(t, path+".gz"); got != "dash1;up\n" {
		t.Errorf("decompressed contents = %q", got)
	}
	if files := writer.Files(); len(files) != 1 || !strings.HasSuffix(files[0].Path, ".txt.gz") {
		t.Errorf("Files() = %+v, want a single .txt.gz file", files)
	}
}

// TestDoesNotDoubleSuffixGz covers an output path the user already suffixed.
func TestDoesNotDoubleSuffixGz(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt.gz")

	writer := output.New(output.Options{Path: path, Compress: true})
	mustWrite(t, writer, "dash1", []string{"up"})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := readGzip(t, path); got != "dash1;up\n" {
		t.Errorf("decompressed contents = %q", got)
	}
}

func TestSplitsAfterDashboardsPerFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")

	writer := output.New(output.Options{Path: path, DashboardsPerFile: 2})
	for i := 1; i <= 5; i++ {
		uid := fmt.Sprintf("dash%d", i)
		mustWrite(t, writer, uid, []string{uid + "_metric_a", uid + "_metric_b"})
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	files := writer.Files()
	if len(files) != 3 {
		t.Fatalf("wrote %d files, want 3", len(files))
	}
	wantCounts := []int{2, 2, 1}
	for i, f := range files {
		wantPath := filepath.Join(dir, fmt.Sprintf("queries-%05d.txt", i+1))
		if f.Path != wantPath {
			t.Errorf("file %d path = %q, want %q", i, f.Path, wantPath)
		}
		if f.Dashboards != wantCounts[i] {
			t.Errorf("file %d holds %d dashboards, want %d", i, f.Dashboards, wantCounts[i])
		}
	}

	// Every dashboard's queries must stay together in one file.
	for _, f := range files {
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(readFile(t, f.Path)), "\n") {
			uid, _, _ := strings.Cut(line, ";")
			seen[uid] = true
		}
		for uid := range seen {
			count := strings.Count(readFile(t, f.Path), uid+";")
			if count != 2 {
				t.Errorf("dashboard %s has %d of its 2 queries in %s: a dashboard must not span files",
					uid, count, f.Path)
			}
		}
	}
}

func TestSplitCombinedWithCompression(t *testing.T) {
	dir := t.TempDir()
	writer := output.New(output.Options{
		Path:              filepath.Join(dir, "queries.txt"),
		Compress:          true,
		DashboardsPerFile: 1,
	})
	mustWrite(t, writer, "dash1", []string{"a"})
	mustWrite(t, writer, "dash2", []string{"b"})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i, want := range []string{"dash1;a\n", "dash2;b\n"} {
		path := filepath.Join(dir, fmt.Sprintf("queries-%05d.txt.gz", i+1))
		if got := readGzip(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestSkipsDashboardsWithoutQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")

	writer := output.New(output.Options{Path: path, DashboardsPerFile: 2})
	mustWrite(t, writer, "empty1", nil)
	mustWrite(t, writer, "empty2", []string{})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(writer.Files()) != 0 {
		t.Errorf("Files() = %+v, want none", writer.Files())
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("no file should be created when there is nothing to write")
	}
}

func TestReplacesExistingFileByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")
	if err := os.WriteFile(path, []byte("stale;content\n"), 0o644); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	writer := output.New(output.Options{Path: path})
	mustWrite(t, writer, "dash1", []string{"up"})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := readFile(t, path); got != "dash1;up\n" {
		t.Errorf("contents = %q, want the stale content replaced", got)
	}
}

// TestAppendsToExistingFile covers resuming a run with --start-page.
func TestAppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")

	first := output.New(output.Options{Path: path, Append: true})
	mustWrite(t, first, "dash1", []string{"up"})
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := output.New(output.Options{Path: path, Append: true})
	mustWrite(t, second, "dash2", []string{"down"})
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := readFile(t, path); got != "dash1;up\ndash2;down\n" {
		t.Errorf("contents = %q, want both runs", got)
	}
}

// TestAppendedGzipStaysReadable relies on concatenated gzip streams being valid.
func TestAppendedGzipStaysReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")

	for _, uid := range []string{"dash1", "dash2"} {
		writer := output.New(output.Options{Path: path, Compress: true, Append: true})
		mustWrite(t, writer, uid, []string{uid + "_metric"})
		if err := writer.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	if got := readGzip(t, path+".gz"); got != "dash1;dash1_metric\ndash2;dash2_metric\n" {
		t.Errorf("decompressed contents = %q, want both runs", got)
	}
}

// TestAppendContinuesSplitNumbering covers resuming a split run: the second run
// must not write over the files of the first one, nor mix its dashboards into a
// file that a consumer may already have processed.
func TestAppendContinuesSplitNumbering(t *testing.T) {
	for _, compress := range []bool{false, true} {
		name := "plain"
		suffix := ".txt"
		if compress {
			name = "compressed"
			suffix = ".txt.gz"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			options := output.Options{
				Path:              filepath.Join(dir, "queries.txt"),
				Compress:          compress,
				DashboardsPerFile: 2,
				Append:            true,
			}

			first := output.New(options)
			for _, uid := range []string{"dash1", "dash2", "dash3"} {
				mustWrite(t, first, uid, []string{uid + "_metric"})
			}
			if err := first.Close(); err != nil {
				t.Fatalf("closing first run: %v", err)
			}

			second := output.New(options)
			for _, uid := range []string{"dash4", "dash5"} {
				mustWrite(t, second, uid, []string{uid + "_metric"})
			}
			if err := second.Close(); err != nil {
				t.Fatalf("closing second run: %v", err)
			}

			read := readFile
			if compress {
				read = readGzip
			}
			wantFiles := []string{
				"dash1;dash1_metric\ndash2;dash2_metric\n",
				"dash3;dash3_metric\n",
				"dash4;dash4_metric\ndash5;dash5_metric\n",
			}
			for i, want := range wantFiles {
				path := filepath.Join(dir, fmt.Sprintf("queries-%05d%s", i+1, suffix))
				if got := read(t, path); got != want {
					t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "queries-00004"+suffix)); err == nil {
				t.Error("the second run wrote more files than it had dashboards for")
			}

			files := second.Files()
			if len(files) != 1 || filepath.Base(files[0].Path) != "queries-00003"+suffix {
				t.Errorf("second run reported %+v, want only queries-00003%s", files, suffix)
			}
		})
	}
}

func TestCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "queries.txt")

	writer := output.New(output.Options{Path: path})
	mustWrite(t, writer, "dash1", []string{"up"})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := readFile(t, path); got != "dash1;up\n" {
		t.Errorf("contents = %q", got)
	}
}

func TestSanitizesUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")

	writer := output.New(output.Options{Path: path})
	mustWrite(t, writer, "we;ird\nuid", []string{"up"})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := strings.TrimSpace(readFile(t, path))
	if got != "we_ird_uid;up" {
		t.Errorf("line = %q, want we_ird_uid;up", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Error("a uid must never introduce extra lines")
	}
}

// TestFilesIncludesTheOpenFile guards against reporting only rotated files.
func TestFilesIncludesTheOpenFile(t *testing.T) {
	dir := t.TempDir()
	writer := output.New(output.Options{Path: filepath.Join(dir, "queries.txt")})
	mustWrite(t, writer, "dash1", []string{"up"})

	if files := writer.Files(); len(files) != 1 || files[0].Queries != 1 {
		t.Errorf("Files() before Close = %+v, want the in-progress file", files)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if files := writer.Files(); len(files) != 1 {
		t.Errorf("Files() after Close = %+v, want one file", files)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writer := output.New(output.Options{Path: filepath.Join(dir, "queries.txt")})
	mustWrite(t, writer, "dash1", []string{"up"})

	if err := writer.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func mustWrite(t *testing.T, writer *output.Writer, uid string, queries []string) {
	t.Helper()
	if err := writer.WriteDashboard(uid, queries); err != nil {
		t.Fatalf("WriteDashboard(%q): %v", uid, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func readGzip(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("reading %s as gzip: %v", path, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompressing %s: %v", path, err)
	}
	return string(data)
}
