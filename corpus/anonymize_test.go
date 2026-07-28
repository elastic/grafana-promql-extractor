//go:build corpus

package corpus

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/anonymize"
)

// corpusSalt fixes the pseudonyms, so that the test can reproduce the mapping
// the command used and compare the two runs query by query.
const corpusSalt = "corpus-validation"

const (
	// maxSurvivingIdentifiers is the share of anonymized queries that may still
	// contain a word from the dashboard they came from. Measured: none.
	maxSurvivingIdentifiers = 0.0
	// maxParseDivergence is the share of queries whose parseability may change
	// when their identifiers are replaced, in either direction. Measured:
	// 0.06%, all of it MetricsQL that the anonymizer, which only knows PromQL,
	// pseudonymizes like anything else it cannot place: the with() templates,
	// the "default" operator and functions such as topk_last of a handful of
	// VictoriaMetrics dashboards.
	maxParseDivergence = 0.002
)

// TestAnonymizedCorpus runs --anonymize over the same community dashboards and
// checks the two things the flag promises. Nothing of the dashboards may survive
// in the output, judged by a vocabulary of public PromQL and Grafana words
// assembled independently of the anonymizer. And the queries have to keep their
// shape, judged by a PromQL parser seeing the same structure before and after,
// since output that cannot be analyzed would not be worth sharing.
func TestAnonymizedCorpus(t *testing.T) {
	opts := OptionsFromEnv(t)
	dashboards := Load(t, opts)
	if minimum := opts.Dashboards * 9 / 10; len(dashboards) < minimum {
		t.Fatalf("only %d of the %d requested dashboards could be loaded, expected at least %d",
			len(dashboards), opts.Dashboards, minimum)
	}

	plain, _ := extractAll(t, opts, dashboards, "extracted-queries.txt")
	anonymous, stderr := extractAll(t, opts, dashboards, "extracted-queries-anonymized.txt",
		"--anonymize", "--anonymize-salt", corpusSalt)
	t.Logf("anonymized run summary:\n%s", indent(stderr))

	report := analyzeAnonymization(t, plain, anonymous)
	writeAnonymizationReport(t, opts, report)
	t.Log("\n" + report.String())

	t.Run("EveryQueryCorresponds", func(t *testing.T) {
		if report.Queries == 0 {
			t.Fatal("the anonymized run produced nothing")
		}
		for _, problem := range firstN(report.Mismatched, 20) {
			t.Errorf("anonymized output does not correspond: %s", problem)
		}
	})

	t.Run("NoIdentifierSurvives", func(t *testing.T) {
		rate := ratio(report.QueriesWithSurvivors, report.Queries)
		if rate > maxSurvivingIdentifiers {
			t.Errorf("%d of %d anonymized queries still carry a word from the dashboard (%.3f%%)",
				report.QueriesWithSurvivors, report.Queries, rate*100)
			for _, word := range firstN(sortedKeys(report.Surviving), 20) {
				t.Errorf("  %q survived %d times, e.g. %s", word, report.Surviving[word],
					truncate(report.SurvivingExample[word], 140))
			}
		}
	})

	t.Run("StructureSurvives", func(t *testing.T) {
		rate := ratio(len(report.ParseDivergence), report.Queries)
		t.Logf("%d of %d queries parse before anonymizing, %d after",
			report.ParsedPlain, report.Queries, report.ParsedAnonymous)
		if rate > maxParseDivergence {
			t.Errorf("%d of %d queries parse differently once anonymized (%.3f%%)",
				len(report.ParseDivergence), report.Queries, rate*100)
			for _, divergence := range firstN(report.ParseDivergence, 10) {
				t.Errorf("  %s", divergence)
			}
		}
	})
}

// AnonymizationReport holds what the anonymized run showed.
type AnonymizationReport struct {
	Dashboards int
	Queries    int
	// Surviving counts, per word of the dashboards that came through, how often
	// it did.
	Surviving        map[string]int
	SurvivingExample map[string]string
	// QueriesWithSurvivors counts the queries carrying at least one such word.
	QueriesWithSurvivors int
	// Pseudonyms counts the distinct pseudonyms per kind, which doubles as a
	// census of the corpus: how many metrics, labels and variables it names.
	Pseudonyms map[string]map[string]bool

	ParsedPlain     int
	ParsedAnonymous int
	ParseDivergence []string
	Mismatched      []string
}

// analyzeAnonymization pairs every query of the plain run with its anonymized
// counterpart. The pairing is reproduced locally from the same salt, which is
// itself a check: the command has to have anonymized exactly what the extractor
// produced, no more and no less.
func analyzeAnonymization(t *testing.T, plain, anonymous map[string][]string) *AnonymizationReport {
	t.Helper()

	anonymizer, err := anonymize.New(corpusSalt)
	if err != nil {
		t.Fatalf("anonymize.New: %v", err)
	}

	report := &AnonymizationReport{
		Dashboards:       len(plain),
		Surviving:        map[string]int{},
		SurvivingExample: map[string]string{},
		Pseudonyms:       map[string]map[string]bool{},
	}

	// The pseudonymized uid of every dashboard the plain run saw.
	pseudonymized := make(map[string]string, len(plain))
	for uid := range plain {
		pseudonymized[anonymizer.UID(uid)] = uid
	}

	for uid, queries := range plain {
		got, ok := anonymous[anonymizer.UID(uid)]
		if !ok {
			report.Mismatched = append(report.Mismatched,
				fmt.Sprintf("dashboard %s is missing from the anonymized run", uid))
			continue
		}
		if len(got) != len(queries) {
			report.Mismatched = append(report.Mismatched,
				fmt.Sprintf("dashboard %s yielded %d queries plainly but %d anonymized",
					uid, len(queries), len(got)))
			continue
		}

		for i, query := range queries {
			report.Queries++
			if want := anonymizer.Query(query); got[i] != want {
				report.Mismatched = append(report.Mismatched,
					fmt.Sprintf("dashboard %s query %d:\n      got  %s\n      want %s",
						uid, i, truncate(got[i], 140), truncate(want, 140)))
				continue
			}
			report.check(query, got[i], uid)
		}
	}

	// Every uid of the anonymized run has to stand for one of the plain run, or
	// the two runs saw different dashboards.
	for uid := range anonymous {
		if _, ok := pseudonymized[uid]; !ok {
			report.Mismatched = append(report.Mismatched,
				fmt.Sprintf("dashboard %s appeared only in the anonymized run", uid))
		}
	}
	return report
}

// check compares one pair of queries.
func (r *AnonymizationReport) check(plain, anonymous, uid string) {
	if surviving := SurvivingIdentifiers(anonymous); len(surviving) > 0 {
		r.QueriesWithSurvivors++
		for _, word := range surviving {
			r.Surviving[word]++
			if _, seen := r.SurvivingExample[word]; !seen {
				r.SurvivingExample[word] = anonymous
			}
		}
	}

	for _, word := range Words(anonymous) {
		if kind, _, ok := strings.Cut(word, "_"); ok && pseudonym.MatchString(word) {
			if r.Pseudonyms[kind] == nil {
				r.Pseudonyms[kind] = map[string]bool{}
			}
			r.Pseudonyms[kind][word] = true
		}
	}

	plainParses := ParsesAsPromQL(plain) == nil
	anonymousParses := ParsesAsPromQL(anonymous) == nil
	if plainParses {
		r.ParsedPlain++
	}
	if anonymousParses {
		r.ParsedAnonymous++
	}
	if plainParses != anonymousParses {
		verb := "stopped parsing"
		if anonymousParses {
			verb = "started parsing"
		}
		r.ParseDivergence = append(r.ParseDivergence,
			fmt.Sprintf("dashboard %s %s once anonymized:\n      %s\n      %s",
				uid, verb, truncate(plain, 140), truncate(anonymous, 140)))
	}
}

func (r *AnonymizationReport) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "Anonymization report\n")
	fmt.Fprintf(&b, "  dashboards:            %d\n", r.Dashboards)
	fmt.Fprintf(&b, "  queries:               %d\n", r.Queries)
	fmt.Fprintf(&b, "  parse as PromQL:       %d plainly, %d anonymized\n", r.ParsedPlain, r.ParsedAnonymous)
	fmt.Fprintf(&b, "  queries with a surviving word: %d\n", r.QueriesWithSurvivors)

	fmt.Fprintf(&b, "\nDistinct pseudonyms, which is what the corpus names:\n")
	for _, kind := range sortedKeys(countByKind(r.Pseudonyms)) {
		fmt.Fprintf(&b, "  %-8s %6d\n", kind, len(r.Pseudonyms[kind]))
	}

	if len(r.Surviving) > 0 {
		fmt.Fprintf(&b, "\nWords that survived anonymization (%d distinct):\n", len(r.Surviving))
		for _, word := range firstN(sortedKeys(r.Surviving), 40) {
			fmt.Fprintf(&b, "  %-30s %5d times, e.g. %s\n", word, r.Surviving[word],
				truncate(r.SurvivingExample[word], 120))
		}
	}

	section(&b, "Queries that parse differently once anonymized", r.ParseDivergence, 20)
	section(&b, "Output that does not correspond to the plain run", r.Mismatched, 20)

	return b.String()
}

func countByKind(pseudonyms map[string]map[string]bool) map[string]int {
	counts := make(map[string]int, len(pseudonyms))
	for kind, names := range pseudonyms {
		counts[kind] = len(names)
	}
	return counts
}

func writeAnonymizationReport(t *testing.T, opts Options, report *AnonymizationReport) {
	t.Helper()
	path := filepath.Join(opts.CacheDir, "report-anonymized.txt")
	if err := os.WriteFile(path, []byte(report.String()), 0o644); err != nil {
		t.Fatalf("writing report: %v", err)
	}
	t.Logf("full report written to %s", path)
}
