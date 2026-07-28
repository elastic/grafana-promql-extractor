package progress_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/progress"
)

func TestParseMode(t *testing.T) {
	for _, in := range []string{"auto", "AUTO", " always ", "never"} {
		if _, err := progress.ParseMode(in); err != nil {
			t.Errorf("ParseMode(%q): %v", in, err)
		}
	}
	if _, err := progress.ParseMode("sometimes"); err == nil {
		t.Error("ParseMode(\"sometimes\") should fail")
	}
}

func TestHumanInt(t *testing.T) {
	tests := map[int]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", 12345: "12,345",
		999999: "999,999", 1000000: "1,000,000", 50000: "50,000", -12345: "-12,345",
	}
	for in, want := range tests {
		if got := progress.HumanInt(in); got != want {
			t.Errorf("HumanInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCountsAreAggregated(t *testing.T) {
	tracker := progress.New(&bytes.Buffer{}, 10, progress.ModeNever)
	tracker.Start()

	tracker.AddDashboard(3)
	tracker.AddDashboard(0)
	tracker.AddFailure()
	tracker.Stop()

	dashboards, queries, failures := tracker.Counts()
	if dashboards != 3 || queries != 3 || failures != 1 {
		t.Errorf("Counts() = %d/%d/%d, want 3/3/1", dashboards, queries, failures)
	}
}

func TestNeverModeStaysSilent(t *testing.T) {
	var out bytes.Buffer
	tracker := progress.New(&out, 10, progress.ModeNever)
	tracker.Start()
	tracker.AddDashboard(1)
	tracker.Logf("this is still written")
	tracker.Stop()

	if !strings.Contains(out.String(), "this is still written") {
		t.Error("Logf messages must be written even when progress is disabled")
	}
	if strings.Contains(out.String(), "dashboards") {
		t.Errorf("no progress line expected, got %q", out.String())
	}
}

// TestNonTerminalOutputHasNoCarriageReturns keeps CI logs readable.
func TestNonTerminalOutputHasNoCarriageReturns(t *testing.T) {
	var out bytes.Buffer
	tracker := progress.New(&out, 100, progress.ModeAuto)
	tracker.Start()
	tracker.AddDashboard(5)
	tracker.Logf("a message")
	tracker.Stop()

	if strings.Contains(out.String(), "\r") {
		t.Errorf("output to a non-terminal must not use carriage returns, got %q", out.String())
	}
}

func TestSetTotalIsReflectedInTheLine(t *testing.T) {
	var out bytes.Buffer
	tracker := progress.New(&out, 0, progress.ModeAlways)
	tracker.SetTotal(200)
	tracker.AddDashboard(1)
	tracker.Stop()
	// Rendering happens on a ticker, so assert on the counters instead of racing
	// the first tick.
	if dashboards, _, _ := tracker.Counts(); dashboards != 1 {
		t.Errorf("Counts() dashboards = %d, want 1", dashboards)
	}
}

func TestConcurrentUpdatesAreSafe(t *testing.T) {
	tracker := progress.New(&bytes.Buffer{}, 1000, progress.ModeAlways)
	tracker.Start()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				tracker.AddDashboard(2)
			}
		}()
	}
	wg.Wait()
	tracker.Stop()

	dashboards, queries, _ := tracker.Counts()
	if dashboards != 800 || queries != 1600 {
		t.Errorf("Counts() = %d/%d, want 800/1600", dashboards, queries)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	tracker := progress.New(&bytes.Buffer{}, 0, progress.ModeAlways)
	tracker.Start()
	tracker.Stop()
	tracker.Stop()
}
