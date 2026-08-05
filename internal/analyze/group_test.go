package analyze_test

import (
	"strings"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/analyze"
)

func TestExtractErrorMessage(t *testing.T) {
	raw := `{"error":{"root_cause":[{"reason":"unknown PromQL function [label_join]"}],"type":"illegal_argument_exception","reason":"unknown PromQL function [label_join]"},"status":400}`
	got := analyze.ExtractErrorMessage(raw)
	if got != "unknown PromQL function [label_join]" {
		t.Fatalf("got %q", got)
	}
}

func TestGroupErrorsCanonical(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			`VectorBinaryArithmetic queries with group modifiers are not supported at this time [foo] ) * on(instance) group_left bar`,
			"VectorBinaryArithmetic queries with group modifiers are not supported at this time [...]",
		},
		{
			`unknown PromQL function [label_join]`,
			"Function [...] is not yet implemented",
		},
		{
			"Found 2 problems\nline 0:1: [sum(time()-foo)] requires the [@timestamp] field",
			"requires the [@timestamp] field [...]",
		},
	}
	for _, tc := range tests {
		groups := analyze.GroupErrors([]string{tc.in})
		if len(groups) != 1 || groups[0] != tc.want {
			t.Fatalf("GroupErrors(%q) = %#v, want [%q]", tc.in, groups, tc.want)
		}
	}
}

func TestGroupErrorsJavaStyle(t *testing.T) {
	groups := analyze.GroupErrors([]string{
		"line 1:27: Subquery queries are not supported at this time [foo[5m:1m]]",
	})
	if len(groups) != 1 {
		t.Fatalf("groups = %#v", groups)
	}
	if !strings.Contains(groups[0], "Subquery queries are not supported") {
		t.Fatalf("group = %q", groups[0])
	}
	if strings.Contains(groups[0], "5m") {
		t.Fatalf("expected numeric normalization, got %q", groups[0])
	}
}

func TestGroupErrorsHTTPSetOperators(t *testing.T) {
	raw := `set operators are not supported at this time [round(sum (rate(istio_requests_total{reporter=~"source|waypoint",response_code=~"4.."}[5m])), 0.01) or vector(0)]`
	groups := analyze.GroupErrors([]string{raw})
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want 1", groups)
	}
	want := "set operators are not supported at this time [...]"
	if groups[0] != want {
		t.Fatalf("group = %q, want %q", groups[0], want)
	}
}

func TestGroupErrorsHTTPSetOperatorsCollapsesVariants(t *testing.T) {
	short := "set operators are not supported at this time [...]"
	long := `set operators are not supported at this time [...]) * on (container_id) group_left (container, pod, namespace) max by ( container, container_id, pod, namespace) (kube_pod_container_info{container_id!="", cluster="cluster"}) OR kube_namespace_created{cluster="cluster"} * 0]`
	g1 := analyze.GroupErrors([]string{short})
	g2 := analyze.GroupErrors([]string{long})
	if len(g1) != 1 || len(g2) != 1 {
		t.Fatalf("short=%#v long=%#v", g1, g2)
	}
	if g1[0] != g2[0] {
		t.Fatalf("groups differ: %q vs %q", g1[0], g2[0])
	}
}

func TestGroupErrorsHTTPFunctionDoesNotExist(t *testing.T) {
	groups := analyze.GroupErrors([]string{"Function histogram_quantile does not exist"})
	if len(groups) != 1 || groups[0] != "Function [...] does not exist" {
		t.Fatalf("groups = %#v", groups)
	}
}

func TestGroupErrorsHTTPCounterMetric(t *testing.T) {
	raw := `function [rate] requires a counter metric, but [rate] has type [counter] [rate(net_bytes_recv[5m])]`
	groups := analyze.GroupErrors([]string{raw})
	want := "function [...] requires a counter metric, but [...] has type [...] [...]"
	if len(groups) != 1 || groups[0] != want {
		t.Fatalf("groups = %#v, want [%q]", groups, want)
	}
}

func TestGroupErrorsHTTPPlanOptimizer(t *testing.T) {
	raw := `Plan [Project[[value{r}#1, job{r}#1]] optimized incorrectly due to duplicate output attribute value{r}#1`
	groups := analyze.GroupErrors([]string{raw})
	want := "Plan [...] optimized incorrectly due to duplicate output attribute [...]"
	if len(groups) != 1 || groups[0] != want {
		t.Fatalf("groups = %#v, want [%q]", groups, want)
	}
}

func TestGroupErrorsCollapsesCounterMetricVariants(t *testing.T) {
	a := `function [rate] requires a counter metric, but [rate] has type [counter] [rate(x[5m])]`
	b := `function [sum_over_time] requires a counter metric, but [sum_over_time] has type [counter] [sum_over_time(x[$__interval])]`
	g1 := analyze.GroupErrors([]string{a})
	g2 := analyze.GroupErrors([]string{b})
	if len(g1) != 1 || len(g2) != 1 || g1[0] != g2[0] {
		t.Fatalf("g1=%#v g2=%#v", g1, g2)
	}
}
