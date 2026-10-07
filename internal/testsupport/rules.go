package testsupport

import (
	"encoding/json"
	"strings"
)

// Where the rule fixtures live. The integration tests provision them under
// these names, and the fake serves them the same way.
const (
	// RuleFolder is the folder the Grafana-managed rules are stored in.
	RuleFolder = "Fixture rules"
	// RuleGroup is the group of both the Grafana-managed rules and the rules
	// of the Prometheus datasources.
	RuleGroup = "fixtures"
	// RuleFile is where Prometheus loads its rules from. Being absolute, it
	// brings its own slash into the identifiers of the rules.
	RuleFile = "/etc/prometheus/rules.yml"
)

// GrafanaRuleFixture is a rule Grafana evaluates itself.
type GrafanaRuleFixture struct {
	UID   string
	Title string
	// Record names the metric a recording rule writes, and is empty for an
	// alert rule.
	Record string
	// Condition is the query an alert rule fires on.
	Condition string
	Queries   []RuleFixtureQuery
	// Expected holds the queries the rule must yield, in order.
	Expected []string
}

// RuleFixtureQuery is one step of a Grafana-managed rule.
type RuleFixtureQuery struct {
	RefID         string
	DatasourceUID string
	Model         map[string]any
}

// expressionUID is the datasource Grafana runs its own expressions on.
const expressionUID = "__expr__"

// GrafanaRuleFixtures returns the Grafana-managed rules the fixtures define.
func GrafanaRuleFixtures() []GrafanaRuleFixture {
	reduce := func(refID, from string) RuleFixtureQuery {
		return RuleFixtureQuery{RefID: refID, DatasourceUID: expressionUID, Model: map[string]any{
			"type": "reduce", "expression": from, "reducer": "last",
		}}
	}
	threshold := func(refID, from string, above float64) RuleFixtureQuery {
		return RuleFixtureQuery{RefID: refID, DatasourceUID: expressionUID, Model: map[string]any{
			"type": "threshold", "expression": from,
			"conditions": []any{map[string]any{
				"evaluator": map[string]any{"type": "gt", "params": []any{above}},
			}},
		}}
	}
	query := func(refID, datasourceUID, expr string) RuleFixtureQuery {
		return RuleFixtureQuery{RefID: refID, DatasourceUID: datasourceUID, Model: map[string]any{"expr": expr}}
	}

	errorRatio := `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))`
	return []GrafanaRuleFixture{
		{
			UID:       "fx-rule-error-ratio",
			Title:     "High error ratio",
			Condition: "C",
			Queries: []RuleFixtureQuery{
				query("A", PrometheusUID, errorRatio),
				reduce("B", "A"),
				threshold("C", "B", 0.05),
			},
			Expected: []string{errorRatio},
		},
		{
			// Each query is resolved on its own, and a multi-line one comes out
			// on a single line without its comment.
			UID:       "fx-rule-two-queries",
			Title:     "Load above capacity",
			Condition: "D",
			Queries: []RuleFixtureQuery{
				query("A", PrometheusSecondaryUID, "max by (instance) (\n  node_load1 # one-minute load\n)"),
				query("B", PrometheusUID, `count by (instance) (node_cpu_seconds_total{mode="idle"})`),
				{RefID: "C", DatasourceUID: expressionUID, Model: map[string]any{"type": "math", "expression": "$A / $B"}},
				threshold("D", "C", 1),
			},
			Expected: []string{
				"max by (instance) ( node_load1 )",
				`count by (instance) (node_cpu_seconds_total{mode="idle"})`,
			},
		},
		{
			UID:       "fx-rule-loki",
			Title:     "Error logs",
			Condition: "C",
			Queries: []RuleFixtureQuery{
				query("A", LokiUID, `sum(count_over_time({job="api"} |= "should_not_appear_loki_rule" [5m]))`),
				reduce("B", "A"),
				threshold("C", "B", 10),
			},
		},
		{
			UID:    "fx-rule-recording",
			Title:  "job:http_requests:rate5m",
			Record: "job:http_requests:rate5m",
			Queries: []RuleFixtureQuery{
				query("A", PrometheusUID, "sum by (job) (rate(http_requests_total[5m]))"),
			},
			Expected: []string{"sum by (job) (rate(http_requests_total[5m]))"},
		},
	}
}

// DatasourceRuleFixture is a rule a Prometheus datasource evaluates. Every
// Prometheus datasource of the fixtures serves the same ones.
type DatasourceRuleFixture struct {
	Name string
	// Record is set for a recording rule, whose name is the metric it writes.
	Record bool
	// Query is written the way Prometheus prints it back, which is also what
	// it must come out as.
	Query string
}

// DatasourceRuleFixtures returns the rules the Prometheus datasources serve.
func DatasourceRuleFixtures() []DatasourceRuleFixture {
	return []DatasourceRuleFixture{
		{Name: "job:http_errors:rate5m", Record: true,
			Query: `sum by (job) (rate(http_requests_total{code=~"5.."}[5m]))`},
		{Name: "HighErrorRate",
			Query: `job:http_errors:rate5m > 1`},
	}
}

// PrometheusUIDs are the fixture datasources that serve rules.
func PrometheusUIDs() []string {
	return []string{PrometheusUID, PrometheusSecondaryUID}
}

// ExpectedRuleLines returns the lines every rule fixture must yield, the
// Grafana-managed ones first and then those of each Prometheus datasource.
func ExpectedRuleLines() []string {
	var lines []string
	for _, rule := range GrafanaRuleFixtures() {
		for _, query := range rule.Expected {
			lines = append(lines, "rule:"+rule.UID+";"+query)
		}
	}
	for _, uid := range PrometheusUIDs() {
		for _, rule := range DatasourceRuleFixtures() {
			lines = append(lines, "rule:"+uid+RuleFile+"/"+RuleGroup+"/"+rule.Name+";"+rule.Query)
		}
	}
	return lines
}

// RulerJSON renders the Grafana-managed rules the way the ruler API returns
// them: groups by folder, each rule wrapped in grafana_alert.
func RulerJSON() []byte {
	rules := make([]any, 0)
	for _, rule := range GrafanaRuleFixtures() {
		alert := map[string]any{
			"uid":           rule.UID,
			"title":         rule.Title,
			"condition":     rule.Condition,
			"data":          rule.data(),
			"namespace_uid": "fx-rules",
			"rule_group":    RuleGroup,
		}
		if rule.Record != "" {
			alert["record"] = map[string]any{"metric": rule.Record, "from": rule.Queries[0].RefID}
		}
		rules = append(rules, map[string]any{"expr": "", "grafana_alert": alert})
	}
	return mustJSON(map[string]any{
		RuleFolder: []any{map[string]any{"name": RuleGroup, "interval": "1m", "rules": rules}},
	})
}

// AlertingProvisioningJSON renders the Grafana-managed rules as a provisioning
// file. JSON is valid YAML, which is what Grafana reads.
func AlertingProvisioningJSON() []byte {
	rules := make([]any, 0)
	for _, rule := range GrafanaRuleFixtures() {
		provisioned := map[string]any{
			"uid":   rule.UID,
			"title": rule.Title,
			"data":  rule.data(),
		}
		if rule.Record != "" {
			provisioned["record"] = map[string]any{
				"metric":              rule.Record,
				"from":                rule.Queries[0].RefID,
				"targetDatasourceUid": PrometheusUID,
			}
			// Grafana 11 refuses a provisioned rule without a condition, even
			// one that only records.
			provisioned["condition"] = rule.Queries[0].RefID
		} else {
			provisioned["condition"] = rule.Condition
			provisioned["noDataState"] = "OK"
			provisioned["execErrState"] = "OK"
			provisioned["for"] = "5m"
		}
		rules = append(rules, provisioned)
	}
	return mustJSON(map[string]any{
		"apiVersion": 1,
		"groups": []any{map[string]any{
			"orgId":    1,
			"name":     RuleGroup,
			"folder":   RuleFolder,
			"interval": "1m",
			"rules":    rules,
		}},
	})
}

func (rule GrafanaRuleFixture) data() []any {
	data := make([]any, 0, len(rule.Queries))
	for _, q := range rule.Queries {
		model := map[string]any{"refId": q.RefID}
		for k, v := range q.Model {
			model[k] = v
		}
		if q.DatasourceUID == expressionUID {
			model["datasource"] = map[string]any{"type": "__expr__", "uid": expressionUID}
		}
		data = append(data, map[string]any{
			"refId":             q.RefID,
			"datasourceUid":     q.DatasourceUID,
			"relativeTimeRange": map[string]any{"from": 600, "to": 0},
			"model":             model,
		})
	}
	return data
}

// PrometheusRulesJSON renders the datasource rules the way the rules API of
// Prometheus returns them.
func PrometheusRulesJSON() []byte {
	rules := make([]any, 0)
	for _, rule := range DatasourceRuleFixtures() {
		kind := "alerting"
		if rule.Record {
			kind = "recording"
		}
		rules = append(rules, map[string]any{"name": rule.Name, "query": rule.Query, "type": kind})
	}
	return mustJSON(map[string]any{
		"status": "success",
		"data": map[string]any{"groups": []any{map[string]any{
			"name": RuleGroup, "file": RuleFile, "interval": 60, "rules": rules,
		}}},
	})
}

// PrometheusRuleFile renders the datasource rules as a Prometheus rule file.
func PrometheusRuleFile() string {
	var b strings.Builder
	b.WriteString("groups:\n  - name: " + RuleGroup + "\n    rules:\n")
	for _, rule := range DatasourceRuleFixtures() {
		key := "alert"
		if rule.Record {
			key = "record"
		}
		b.WriteString("      - " + key + ": " + rule.Name + "\n")
		b.WriteString("        expr: " + string(mustJSON(rule.Query)) + "\n")
	}
	return b.String()
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
