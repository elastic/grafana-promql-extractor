package extract

import "strings"

// RulePrefix starts the identifier of every rule in the output, which keeps it
// apart from dashboard uids: those cannot contain a colon.
const RulePrefix = "rule:"

// refExpressionsUID is the uid older releases gave the expression datasource.
const refExpressionsUID = "-100"

// Rule is an alert or recording rule, reduced to what extraction needs.
type Rule struct {
	// ID identifies the rule in the output.
	ID string
	// Recording is set for a recording rule, as opposed to an alerting one.
	Recording bool
	Queries   []RuleQuery
}

// RuleQuery is one query of a rule and the datasource it runs against.
type RuleQuery struct {
	DatasourceUID string
	Expr          string
}

// ExtractRule collects the PromQL expressions of a rule. A rule a Prometheus
// datasource evaluates has a single query; one Grafana evaluates has several,
// each against its own datasource, plus expressions that reduce and compare
// their results inside Grafana and are skipped like the expression datasource
// is on panels.
func (e *Extractor) ExtractRule(rule Rule) Result {
	res := Result{UID: rule.ID}
	if rule.Recording {
		res.Stats.RecordingRules = 1
	} else {
		res.Stats.AlertRules = 1
	}

	c := e.newCollector(&res)
	for _, q := range rule.Queries {
		res.Stats.RuleQueries++
		pluginType, special := e.resolveRuleDatasource(q.DatasourceUID)
		c.consider(pluginType, special, q.Expr)
	}
	return res
}

// resolveRuleDatasource determines the plugin type of a rule query. Unlike a
// panel target, a rule query always names its datasource by uid.
func (e *Extractor) resolveRuleDatasource(uid string) (pluginType string, special bool) {
	switch strings.ToLower(uid) {
	case refExpr, refExpressionsUID:
		return "", true
	case "":
		return e.defaultType(), false
	}
	if e.Lookup != nil {
		if t, ok := e.Lookup.Lookup(uid); ok {
			return t, false
		}
	}
	return "", false
}
