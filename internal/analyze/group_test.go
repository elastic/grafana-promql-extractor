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
			"Function label_join is not yet implemented",
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
