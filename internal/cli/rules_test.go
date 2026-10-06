package cli_test

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

func TestExtractsRules(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures, Rules: true})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}

	want := append(testsupport.ExpectedLines(fixtures), testsupport.ExpectedRuleLines()...)
	assertSameLines(t, readLines(t, out), want)

	// Loki rules hold LogQL, so the Loki datasource is never asked.
	if got, want := fake.Requests(testsupport.DatasourceRulesRoute), len(testsupport.PrometheusUIDs()); got != want {
		t.Errorf("asked %d datasources for rules, want only the %d Prometheus ones", got, want)
	}
	for _, row := range []string{"alert rules:", "recording rules:", "and 7 rules"} {
		if !strings.Contains(stderr, row) {
			t.Errorf("summary is missing %q:\n%s", row, stderr)
		}
	}
}

// A run limited to some dashboards leaves the rules out unless asked for them.
func TestRulesFollowTheScopeOfTheRun(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(5)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards, Rules: true})

	tests := []struct {
		name      string
		args      []string
		wantRules bool
	}{
		{"max dashboards", []string{"--max-dashboards", "2"}, false},
		{"folder", []string{"--folder-uid", "somewhere"}, false},
		{"tag", []string{"--tag", "team"}, false},
		{"asked for", []string{"--max-dashboards", "2", "--rules", "on"}, true},
		{"turned off", []string{"--rules", "off"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "queries.txt")
			args := append([]string{"--url", fake.URL, "-o", out, "--compress=false", "--progress", "never"}, tt.args...)
			if stderr, err := runCLI(t, args...); err != nil {
				t.Fatalf("run failed: %v\n%s", err, stderr)
			}
			got := len(ruleLines(readLines(t, out))) > 0
			if got != tt.wantRules {
				t.Errorf("rules in the output: %t, want %t", got, tt.wantRules)
			}
		})
	}
}

func TestInstanceWithoutRules(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(3)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	assertSameLines(t, readLines(t, out), testsupport.ExpectedLines(dashboards))
	if strings.Contains(stderr, "rules") {
		t.Errorf("an instance without rules is not worth a word:\n%s", stderr)
	}

	if _, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never",
		"--rules", "on"); err == nil {
		t.Error("--rules on should fail against an instance that serves no rules")
	}
}

func TestRulesTheCredentialsCannotRead(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(3)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:  dashboards,
		Rules:       true,
		RulerStatus: http.StatusForbidden,
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	for _, want := range []string{"cannot read Grafana-managed rules", "unreadable rules:"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr is missing %q:\n%s", want, stderr)
		}
	}
	// The datasources still serve theirs.
	if got := ruleLines(readLines(t, out)); len(got) != 4 {
		t.Errorf("got %d rule lines, want the 4 of the Prometheus datasources: %v", len(got), got)
	}

	if _, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never",
		"--rules", "on"); err == nil {
		t.Error("--rules on should fail when the rules cannot be read")
	}
}

func TestSkipsADatasourceWhoseRulesFail(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:            testsupport.GeneratedFixtures(3),
		Rules:                 true,
		DatasourceRulesStatus: map[string]int{testsupport.PrometheusSecondaryUID: http.StatusBadGateway},
	})
	out := filepath.Join(t.TempDir(), "queries.txt")

	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never",
		"--retries", "0")
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "skipping the rules of datasource "+testsupport.PrometheusSecondaryUID) {
		t.Errorf("the failing datasource was not reported:\n%s", stderr)
	}
	for _, line := range ruleLines(readLines(t, out)) {
		if strings.HasPrefix(line, "rule:"+testsupport.PrometheusSecondaryUID+"/") {
			t.Errorf("unexpected line from the failing datasource: %s", line)
		}
	}

	// Asking for rules explicitly insists that the instance serves them, not
	// that every datasource does.
	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never",
		"--retries", "0", "--rules", "on"); err != nil {
		t.Errorf("--rules on should skip the failing datasource: %v\n%s", err, stderr)
	}

	if _, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never",
		"--retries", "0", "--fail-fast"); err == nil {
		t.Error("--fail-fast should stop at the failing datasource")
	}
}

// Rules are written before the dashboards, and each file is described by what
// it holds.
func TestSummaryNamesWhatEachFileHolds(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(3)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards, Rules: true})
	out := filepath.Join(t.TempDir(), "queries.txt")

	rules := len(testsupport.ExpectedRuleLines()) - 1 // one Grafana-managed rule has two queries
	stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never",
		"--dashboards-per-file", strconv.Itoa(rules+1))
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	for _, want := range []string{
		fmt.Sprintf("from 1 dashboard and %d rules)", rules),
		"from 2 dashboards)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("summary is missing %q:\n%s", want, stderr)
		}
	}
}

func TestAnonymizesRules(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Rules: true})
	out := filepath.Join(t.TempDir(), "queries.txt")

	if stderr, err := runCLI(t, "--url", fake.URL, "-o", out, "--compress=false", "--progress", "never",
		"--anonymize"); err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr)
	}
	lines := readLines(t, out)
	if len(lines) != len(testsupport.ExpectedRuleLines()) {
		t.Errorf("got %d lines, want %d", len(lines), len(testsupport.ExpectedRuleLines()))
	}
	joined := strings.Join(lines, "\n")
	for _, secret := range []string{"rule:", "fx-rule", "prom-main", testsupport.RuleGroup, "http_requests_total", "HighErrorRate"} {
		if strings.Contains(joined, secret) {
			t.Errorf("%q survived anonymization", secret)
		}
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "rule_") {
			t.Errorf("rule line without a rule pseudonym: %s", line)
		}
	}
}

func TestRejectsUnknownRulesMode(t *testing.T) {
	if _, err := runCLI(t, "--url", "http://localhost", "--rules", "sometimes"); err == nil {
		t.Error("--rules sometimes should have been rejected")
	}
}

// ruleLines keeps the lines that came from rules rather than dashboards.
func ruleLines(lines []string) []string {
	var rules []string
	for _, line := range lines {
		if strings.HasPrefix(line, "rule:") {
			rules = append(rules, line)
		}
	}
	return rules
}
