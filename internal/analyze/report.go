package analyze

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Report accumulates coverage statistics while queries are checked. It is safe
// for concurrent use from worker goroutines.
type Report struct {
	mu sync.Mutex

	totalQueries      int
	successfulQueries int

	dashboards map[string]*dashStats

	groupCount      map[string]int
	groupDashboards map[string]map[string]struct{}
	groupExample    map[string]string
	dashboardGroups map[string]map[string]struct{}
}

type dashStats struct {
	queries  int
	failures int
	groups   map[string]struct{}
}

// NewReport returns an empty report.
func NewReport() *Report {
	return &Report{
		dashboards:      make(map[string]*dashStats),
		groupCount:      make(map[string]int),
		groupDashboards: make(map[string]map[string]struct{}),
		groupExample:    make(map[string]string),
		dashboardGroups: make(map[string]map[string]struct{}),
	}
}

// Record adds one query outcome to the report.
func (r *Report) Record(dashboardUID, query string, success bool, errorGroups []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.totalQueries++
	if success {
		r.successfulQueries++
	}

	ds := r.dashboards[dashboardUID]
	if ds == nil {
		ds = &dashStats{groups: make(map[string]struct{})}
		r.dashboards[dashboardUID] = ds
	}
	ds.queries++
	if success {
		return
	}

	ds.failures++
	for _, g := range errorGroups {
		if g == "" {
			continue
		}
		r.groupCount[g]++
		if r.groupDashboards[g] == nil {
			r.groupDashboards[g] = make(map[string]struct{})
		}
		r.groupDashboards[g][dashboardUID] = struct{}{}
		if ex, ok := r.groupExample[g]; !ok || len(query) < len(ex) {
			r.groupExample[g] = query
		}
		ds.groups[g] = struct{}{}
		if r.dashboardGroups[dashboardUID] == nil {
			r.dashboardGroups[dashboardUID] = make(map[string]struct{})
		}
		r.dashboardGroups[dashboardUID][g] = struct{}{}
	}
}

// TotalQueries returns how many queries were checked.
func (r *Report) TotalQueries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totalQueries
}

// SuccessfulQueries returns how many queries Elasticsearch accepted.
func (r *Report) SuccessfulQueries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.successfulQueries
}

// WriteMarkdown renders the aggregated report to out.
func (r *Report) WriteMarkdown(out io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	successfulQueries := r.successfulQueries
	totalQueries := r.totalQueries
	totalDashboards := len(r.dashboards)

	successfulDashboards := 0
	for _, ds := range r.dashboards {
		if ds.queries > 0 && ds.failures == 0 {
			successfulDashboards++
		}
	}

	fmt.Fprintf(out, "| Successful Queries | Successful Dashboards |\n")
	fmt.Fprintf(out, "|-------------------:|----------------------:|\n")
	if totalQueries == 0 {
		fmt.Fprintf(out, "| 0%% (0/0) | 0%% (0/0) |\n\n")
	} else {
		fmt.Fprintf(out, "| %.2f%% (%d/%d) | %.2f%% (%d/%d) |\n\n",
			float64(successfulQueries)*100/float64(totalQueries), successfulQueries, totalQueries,
			float64(successfulDashboards)*100/float64(totalDashboards), successfulDashboards, totalDashboards,
		)
	}

	onlyErrorByDashboard := make(map[string]map[string]struct{})
	for dash, groups := range r.dashboardGroups {
		if len(groups) != 1 {
			continue
		}
		for g := range groups {
			if onlyErrorByDashboard[g] == nil {
				onlyErrorByDashboard[g] = make(map[string]struct{})
			}
			onlyErrorByDashboard[g][dash] = struct{}{}
		}
	}

	groups := make([]string, 0, len(r.groupCount))
	for g := range r.groupCount {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return r.groupCount[groups[i]] > r.groupCount[groups[j]]
	})

	fmt.Fprintf(out, "| Error Group | Total | Dashboards | Only error | Example Query |\n")
	fmt.Fprintf(out, "|-------------|------:|-----------:|-----------:|---------------|\n")
	for _, g := range groups {
		total := r.groupCount[g]
		dashboardCount := len(r.groupDashboards[g])
		onlyCount := 0
		if m := onlyErrorByDashboard[g]; m != nil {
			onlyCount = len(m)
		}
		example := strings.ReplaceAll(r.groupExample[g], "|", `\|`)
		fmt.Fprintf(out, "| %s | %.2f%% (%d) | %.2f%% (%d) | %.2f%% (%d) | `%s` |\n",
			g,
			float64(total)*100/float64(totalQueries), total,
			float64(dashboardCount)*100/float64(totalDashboards), dashboardCount,
			float64(onlyCount)*100/float64(totalDashboards), onlyCount,
			example,
		)
	}
	fmt.Fprintln(out)
	return nil
}
