package cli

import (
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/grafana"
)

func TestDatasourceRuleIDsAreUnique(t *testing.T) {
	rules := []grafana.DatasourceRule{
		// An alert at two severities shares its name.
		{File: "/etc/prometheus/rules.yml", Group: "api", Name: "HighLatency"},
		{File: "/etc/prometheus/rules.yml", Group: "api", Name: "HighLatency"},
		// Mimir namespaces and groups may both contain slashes.
		{File: "team/a", Group: "x", Name: "Down"},
		{File: "team", Group: "a/x", Name: "Down"},
		// Nothing a name contains may break the line or fake a suffix.
		{File: "f", Group: "g", Name: "a;b"},
		{File: "f", Group: "g", Name: "a_b"},
		{File: "f", Group: "g", Name: "HighLatency#2"},
		{File: "f", Group: "g", Name: "HighLatency"},
		{File: "f", Group: "g", Name: "HighLatency"},
	}
	want := []string{
		"rule:prom/etc/prometheus/rules.yml/api/HighLatency",
		"rule:prom/etc/prometheus/rules.yml/api/HighLatency#2",
		"rule:prom/team/a/x/Down",
		"rule:prom/team/a%2Fx/Down",
		"rule:prom/f/g/a%3Bb",
		"rule:prom/f/g/a_b",
		"rule:prom/f/g/HighLatency%232",
		"rule:prom/f/g/HighLatency",
		"rule:prom/f/g/HighLatency#2",
	}
	got := datasourceRuleIDs("prom", rules)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
