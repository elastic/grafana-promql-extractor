package grafana_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/grafana"
	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

func TestGrafanaRules(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Rules: true})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	rules, err := client.GrafanaRules(context.Background())
	if err != nil {
		t.Fatalf("GrafanaRules: %v", err)
	}
	fixtures := testsupport.GrafanaRuleFixtures()
	if len(rules) != len(fixtures) {
		t.Fatalf("got %d rules, want %d", len(rules), len(fixtures))
	}
	for i, rule := range rules {
		fixture := fixtures[i]
		if rule.UID != fixture.UID {
			t.Errorf("rule %d = %s, want %s", i, rule.UID, fixture.UID)
		}
		if rule.Recording != (fixture.Record != "") {
			t.Errorf("rule %s: Recording = %t", rule.UID, rule.Recording)
		}
		if len(rule.Queries) != len(fixture.Queries) {
			t.Fatalf("rule %s has %d queries, want %d", rule.UID, len(rule.Queries), len(fixture.Queries))
		}
		for j, q := range rule.Queries {
			want := fixture.Queries[j]
			expr, _ := want.Model["expr"].(string)
			if q.DatasourceUID != want.DatasourceUID || q.Expr != expr {
				t.Errorf("rule %s query %d = %+v, want %q on %s", rule.UID, j, q, expr, want.DatasourceUID)
			}
		}
	}
}

func TestDatasourceRules(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Rules: true})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	rules, err := client.DatasourceRules(context.Background(), testsupport.PrometheusUID)
	if err != nil {
		t.Fatalf("DatasourceRules: %v", err)
	}
	fixtures := testsupport.DatasourceRuleFixtures()
	if len(rules) != len(fixtures) {
		t.Fatalf("got %d rules, want %d", len(rules), len(fixtures))
	}
	for i, rule := range rules {
		want := fixtures[i]
		if rule.Name != want.Name || rule.Query != want.Query || rule.Recording != want.Record ||
			rule.File != testsupport.RuleFile || rule.Group != testsupport.RuleGroup {
			t.Errorf("rule %d = %+v, want %+v", i, rule, want)
		}
	}

	_, err = client.DatasourceRules(context.Background(), testsupport.LokiUID)
	if !grafana.IsStatus(err, http.StatusNotFound) {
		t.Errorf("a datasource without a rules API should answer 404, got %v", err)
	}
}

// Prometheus reports a failure in the body, and Grafana passes it on.
func TestDatasourceRulesReportsAnErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"error","errorType":"unavailable","error":"rule manager not ready"}`))
	}))
	t.Cleanup(server.Close)
	client := mustClient(t, grafana.Config{BaseURL: server.URL})

	if _, err := client.DatasourceRules(context.Background(), "prom"); err == nil {
		t.Error("an error status should be reported")
	}
}

func TestUIDsOfType(t *testing.T) {
	registry := testsupport.Registry()
	got := registry.UIDsOfType(func(t string) bool { return t == "prometheus" })
	want := []string{testsupport.PrometheusUID, testsupport.PrometheusSecondaryUID}
	if !slices.Equal(got, want) {
		t.Errorf("UIDsOfType(prometheus) = %v, want %v", got, want)
	}
}
