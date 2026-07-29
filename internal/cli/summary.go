package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/extract"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/output"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/progress"
)

// summary reports what the run produced. Every count but the first is left out
// when it is zero, so the block stays as short as the run was uneventful.
func summary(out io.Writer, tracker *progress.Tracker, stats extract.Stats, files []output.FileInfo) {
	dashboards, queries, failures := tracker.Counts()
	fmt.Fprintf(out, "\nProcessed %s in %s\n",
		count(dashboards, "dashboard", "dashboards"), tracker.Elapsed().Round(time.Millisecond))

	var counts rows
	counts.add("queries written", "%s from %s", humanInt(queries),
		count(countDashboards(files), "dashboard", "dashboards"))
	counts.addIf(stats.Panels > 0, "panels visited", "%s", humanInt(stats.Panels))
	counts.addIf(stats.Targets > 0, "targets seen", "%s", humanInt(stats.Targets))
	counts.addIf(stats.Annotations > 0, "annotation queries", "%s", humanInt(stats.Annotations))
	counts.addIf(stats.Duplicates > 0, "duplicates dropped", "%s", humanInt(stats.Duplicates))
	counts.addIf(stats.SkippedEmpty > 0, "empty expressions", "%s skipped",
		count(stats.SkippedEmpty, "target", "targets"))
	counts.addIf(stats.SkippedLogs > 0, "logs panels", "%s skipped",
		count(stats.SkippedLogs, "target", "targets"))
	counts.addIf(stats.SkippedSpecial > 0, "built-in datasources", "%s skipped",
		count(stats.SkippedSpecial, "target", "targets"))
	counts.addIf(stats.UnresolvedIncluded+stats.UnresolvedSkipped > 0, "unresolved datasource",
		"%s kept, %s dropped", count(stats.UnresolvedIncluded, "query", "queries"),
		humanInt(stats.UnresolvedSkipped))
	if skipped := stats.TopSkippedTypes(5); len(skipped) > 0 {
		parts := make([]string, 0, len(skipped))
		for _, s := range skipped {
			parts = append(parts, fmt.Sprintf("%s %s", s.Type, humanInt(s.Count)))
		}
		counts.add("skipped by datasource", "%s", strings.Join(parts, ", "))
	}
	counts.addIf(stats.LibraryPanels > 0, "library panels",
		"%s (their queries are stored outside the dashboard)", humanInt(stats.LibraryPanels))
	counts.addIf(stats.PartialDecodes > 0, "partially decoded", "%s",
		count(stats.PartialDecodes, "dashboard", "dashboards"))
	counts.addIf(failures > 0, "failed dashboards", "%s (re-run with --verbose to see why)", humanInt(failures))
	counts.write(out)

	if len(files) == 0 {
		fmt.Fprintf(out, "  no output files written\n")
		return
	}
	fmt.Fprintf(out, "  files:\n")
	for _, f := range files {
		fmt.Fprintf(out, "    %s (%s from %s)\n", f.Path,
			count(f.Queries, "query", "queries"), count(f.Dashboards, "dashboard", "dashboards"))
	}
}

// resumeHint tells the user where to pick a run up again, whether it was
// interrupted or stopped by an error.
func resumeHint(out io.Writer, page int, interrupted bool, opts *options) {
	what := "Interrupted"
	if !interrupted {
		what = "Stopped by an error"
	}
	fmt.Fprintf(out, "\n%s on search page %d. Resume with --start-page %d --append; "+
		"the dashboards of that page may repeat.\n", what, page, page)
	if opts.anonymize && opts.anonymizeSalt == "" {
		fmt.Fprintf(out, "A resumed run would pseudonymize differently, since this one used a random salt. "+
			"Start over with --anonymize-salt to get one consistent file.\n")
	}
}

// rows is a list of label and value pairs, printed with the labels padded to a
// common width so that adding one does not mean re-aligning the others.
type rows struct {
	list  []row
	width int
}

type row struct{ label, value string }

func (r *rows) add(label, format string, args ...any) {
	r.width = max(r.width, len(label))
	r.list = append(r.list, row{label, fmt.Sprintf(format, args...)})
}

func (r *rows) addIf(cond bool, label, format string, args ...any) {
	if cond {
		r.add(label, format, args...)
	}
}

func (r *rows) write(out io.Writer) {
	for _, line := range r.list {
		fmt.Fprintf(out, "  %-*s %s\n", r.width+1, line.label+":", line.value)
	}
}

func countDashboards(files []output.FileInfo) int {
	n := 0
	for _, f := range files {
		n += f.Dashboards
	}
	return n
}

func humanInt(n int) string { return progress.HumanInt(n) }

// count pairs a number with its unit, keeping the unit singular for one.
func count(n int, singular, plural string) string {
	if n == 1 {
		return humanInt(n) + " " + singular
	}
	return humanInt(n) + " " + plural
}
