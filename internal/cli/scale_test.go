package cli_test

import (
	"bufio"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

// TestScale checks that memory stays flat while extracting far more dashboards
// than fit in memory at once. It is opt-in because it takes a while:
//
//	EXTRACTOR_SCALE_DASHBOARDS=50000 go test ./internal/cli/ -run TestScale -v
//
// Both ways of reading dashboards are measured. Fetching them one by one keeps
// nothing per dashboard; reading them in pages remembers a uid for each, to
// know afterwards which ones the pages left out, and that is the only thing in
// the tool that grows with the size of the instance.
func TestScale(t *testing.T) {
	raw := os.Getenv("EXTRACTOR_SCALE_DASHBOARDS")
	if raw == "" {
		t.Skip("set EXTRACTOR_SCALE_DASHBOARDS to run the scale test")
	}
	total, err := strconv.Atoi(raw)
	if err != nil || total <= 0 {
		t.Fatalf("EXTRACTOR_SCALE_DASHBOARDS=%q is not a positive number", raw)
	}

	for _, tc := range []struct {
		name string
		bulk string
		// maxLive bounds the heap held at the end of the run. Fetching one by
		// one retains nothing; pages retain a uid per dashboard, so the bound
		// grows with the instance, at a budget of 100 bytes each.
		maxLive func(total int) float64
	}{
		{name: "one by one", bulk: "off", maxLive: func(int) float64 { return 32 }},
		{name: "in pages", bulk: "on", maxLive: func(n int) float64 { return 32 + float64(n)*100/(1<<20) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
				SyntheticDashboards: total,
				Bulk:                tc.bulk == "on",
			})
			out := filepath.Join(t.TempDir(), "queries.txt")

			stopSampling, peak := sampleHeap(t)

			start := time.Now()
			stderr, err := runCLI(t,
				"--url", fake.URL,
				"-o", out,
				"--page-size", "5000",
				"--concurrency", "16",
				"--progress", "never",
				"--bulk", tc.bulk,
			)
			elapsed := time.Since(start)
			stopSampling()

			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, stderr)
			}

			// Measure before verifying the output, since verification itself
			// keeps a set of every uid and would dominate the reading.
			liveMiB := liveHeapMiB()

			lines, uniqueUIDs := countGzipLines(t, out+".gz")
			wantLines := total * 2
			if lines != wantLines {
				t.Errorf("wrote %d lines, want %d", lines, wantLines)
			}
			if uniqueUIDs != total {
				t.Errorf("wrote %d distinct dashboards, want %d", uniqueUIDs, total)
			}

			peakMiB := float64(peak.Load()) / (1 << 20)
			t.Logf("%d dashboards in %s (%.0f/s), %d lines, peak heap %.1f MiB, live heap after GC %.1f MiB",
				total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), lines, peakMiB, liveMiB)

			// The peak is the looser bound because it includes uncollected
			// garbage and the fake Grafana shares this process.
			maxLiveMiB := tc.maxLive(total)
			maxPeakMiB := 128 + (maxLiveMiB - 32)
			if liveMiB > maxLiveMiB {
				t.Errorf("live heap %.1f MiB exceeds %.1f MiB: more is retained per dashboard than a uid",
					liveMiB, maxLiveMiB)
			}
			if peakMiB > maxPeakMiB {
				t.Errorf("peak heap %.1f MiB exceeds %.1f MiB: memory should not scale with the dashboard count",
					peakMiB, maxPeakMiB)
			}
		})
	}
}

// liveHeapMiB returns the retained heap after collecting garbage.
func liveHeapMiB() float64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return float64(stats.HeapAlloc) / (1 << 20)
}

// sampleHeap records the maximum heap size until the returned function is called.
func sampleHeap(t *testing.T) (stop func(), peak *atomic.Uint64) {
	t.Helper()

	peak = &atomic.Uint64{}
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var stats runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&stats)
				for {
					current := peak.Load()
					if stats.HeapAlloc <= current || peak.CompareAndSwap(current, stats.HeapAlloc) {
						break
					}
				}
			}
		}
	}()

	return func() {
		close(done)
		<-finished
	}, peak
}

// countGzipLines counts lines and distinct dashboard uids without holding the
// whole file in memory.
func countGzipLines(t *testing.T, path string) (lines, uniqueUIDs int) {
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

	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		uid, query, found := strings.Cut(line, ";")
		if !found || uid == "" || query == "" {
			t.Fatalf("malformed line %q", line)
		}
		if _, ok := seen[uid]; !ok {
			seen[uid] = struct{}{}
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning %s: %v", path, err)
	}

	if info, err := os.Stat(path); err == nil {
		t.Logf("%s is %.1f MiB compressed", filepath.Base(path), float64(info.Size())/(1<<20))
	}
	return lines, len(seen)
}
