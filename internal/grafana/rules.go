package grafana

import (
	"context"
	"fmt"
	"net/url"
	"sort"
)

// GrafanaRulesPath serves the alert and recording rules Grafana evaluates
// itself, grouped by folder. The ruler API is used rather than the
// provisioning API because it only takes alert.rules:read, which the Viewer
// role carries, while provisioning needs an administrator.
const GrafanaRulesPath = "/api/ruler/grafana/api/v1/rules"

// DatasourceRulesPath is where Grafana passes on the rules API of a
// Prometheus-compatible datasource. Unlike the ruler API of a datasource, which
// only Mimir and Cortex have, this one also serves a plain Prometheus that loads
// its rules from files.
func DatasourceRulesPath(uid string) string {
	return "/api/prometheus/" + url.PathEscape(uid) + "/api/v1/rules"
}

// GrafanaRule is a rule Grafana evaluates itself. Its queries are a small
// pipeline: datasource queries, and expressions that reduce and compare their
// results, which run in Grafana and carry no PromQL.
type GrafanaRule struct {
	UID string
	// Recording is set for a recording rule, which writes its result back as a
	// metric rather than alerting on it.
	Recording bool
	Queries   []GrafanaRuleQuery
}

// GrafanaRuleQuery is one step of a Grafana-managed rule.
type GrafanaRuleQuery struct {
	DatasourceUID string
	// Expr is the expression of a Prometheus-style query, and empty for
	// anything that keeps its query elsewhere in the model.
	Expr string
}

// GrafanaRules lists every rule Grafana evaluates itself, ordered by folder,
// group and position in the group.
func (c *Client) GrafanaRules(ctx context.Context) ([]GrafanaRule, error) {
	var folders map[string][]struct {
		Name  string `json:"name"`
		Rules []struct {
			GrafanaAlert struct {
				UID    string `json:"uid"`
				Record *struct {
					Metric string `json:"metric"`
				} `json:"record"`
				Data []struct {
					DatasourceUID string `json:"datasourceUid"`
					Model         struct {
						Expr string `json:"expr"`
					} `json:"model"`
				} `json:"data"`
			} `json:"grafana_alert"`
		} `json:"rules"`
	}
	if err := c.GetJSON(ctx, GrafanaRulesPath, nil, &folders); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	sort.Strings(names)

	var rules []GrafanaRule
	for _, folder := range names {
		for _, group := range folders[folder] {
			for _, r := range group.Rules {
				alert := r.GrafanaAlert
				rule := GrafanaRule{
					UID:       alert.UID,
					Recording: alert.Record != nil,
					Queries:   make([]GrafanaRuleQuery, 0, len(alert.Data)),
				}
				for _, q := range alert.Data {
					rule.Queries = append(rule.Queries, GrafanaRuleQuery{
						DatasourceUID: q.DatasourceUID,
						Expr:          q.Model.Expr,
					})
				}
				rules = append(rules, rule)
			}
		}
	}
	return rules, nil
}

// DatasourceRule is a rule a Prometheus-compatible datasource evaluates. These
// rules have no uid; the file, group and name are what tell them apart.
type DatasourceRule struct {
	File  string
	Group string
	Name  string
	// Recording is set for a recording rule, as opposed to an alerting one.
	Recording bool
	Query     string
}

// DatasourceRules lists the rules of one Prometheus-compatible datasource.
func (c *Client) DatasourceRules(ctx context.Context, uid string) ([]DatasourceRule, error) {
	var response struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Groups []struct {
				Name  string `json:"name"`
				File  string `json:"file"`
				Rules []struct {
					Name  string `json:"name"`
					Type  string `json:"type"`
					Query string `json:"query"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	path := DatasourceRulesPath(uid)
	if err := c.GetJSON(ctx, path, nil, &response); err != nil {
		return nil, err
	}
	if response.Status != "" && response.Status != "success" {
		return nil, fmt.Errorf("%s answered %s: %s", path, response.Status, response.Error)
	}

	var rules []DatasourceRule
	for _, group := range response.Data.Groups {
		for _, r := range group.Rules {
			rules = append(rules, DatasourceRule{
				File:      group.File,
				Group:     group.Name,
				Name:      r.Name,
				Recording: r.Type == "recording",
				Query:     r.Query,
			})
		}
	}
	return rules, nil
}
