package cli

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/elastic/grafana-promql-extractor/internal/anonymize"
	"github.com/elastic/grafana-promql-extractor/internal/extract"
	"github.com/elastic/grafana-promql-extractor/internal/grafana"
	"github.com/elastic/grafana-promql-extractor/internal/output"
)

// rules modes.
const (
	rulesAuto = "auto"
	rulesOn   = "on"
	rulesOff  = "off"
)

// chooseRules decides whether a run reads alert and recording rules, and
// whether an instance that will not serve them fails the run. Rules belong to
// no dashboard, so auto leaves them out of a run limited to some dashboards and
// says why.
func chooseRules(opts *options, log *logger) (read, strict bool) {
	switch opts.rules {
	case rulesOff:
		return false, false
	case rulesOn:
		return true, true
	}
	if reason := rulesSkippedFor(opts); reason != "" {
		log.debugf("leaving out alert and recording rules, since the run is %s", reason)
		return false, false
	}
	return true, false
}

// rulesSkippedFor names the option that makes auto leave the rules out, if any.
func rulesSkippedFor(opts *options) string {
	switch {
	case len(opts.folderUIDs) > 0:
		return "limited by --folder-uid"
	case len(opts.tags) > 0:
		return "limited by --tag"
	case opts.maxDashboards > 0:
		return "limited by --max-dashboards"
	default:
		return ""
	}
}

// ruleReader extracts the rules of an instance. An
// instance holds a few thousand rules at most, which a handful of requests
// return whole, so they need none of the pipeline's machinery.
type ruleReader struct {
	client     *grafana.Client
	registry   *grafana.Registry
	extractor  *extract.Extractor
	anonymizer *anonymize.Anonymizer
	writer     *output.Writer
	log        *logger
	// strict fails the run when the instance will not serve rules, instead of
	// carrying on without them.
	strict   bool
	failFast bool
	// concurrency is how many datasources are asked for their rules at once.
	concurrency int
}

// ruleOutcome is what reading the rules produced, for the summary.
type ruleOutcome struct {
	stats extract.Stats
	// written counts the rules with at least one query in the output.
	written int
	queries int
	// failed counts the sources of rules that could not be read.
	failed int
}

func (r *ruleReader) run(ctx context.Context) (ruleOutcome, error) {
	var outcome ruleOutcome

	// Whether the instance serves rules at all shows here, which is what
	// --rules on insists on.
	rules, err := r.client.GrafanaRules(ctx)
	if err != nil && r.strict && ctx.Err() == nil {
		return outcome, fmt.Errorf("reading Grafana-managed rules: %w", err)
	}
	if err := r.check(ctx, err, "Grafana-managed rules", &outcome); err != nil {
		return outcome, err
	}
	for _, rule := range rules {
		queries := make([]extract.RuleQuery, 0, len(rule.Queries))
		for _, q := range rule.Queries {
			queries = append(queries, extract.RuleQuery{DatasourceUID: q.DatasourceUID, Expr: q.Expr})
		}
		err := r.write(&outcome, extract.Rule{
			ID:        extract.RulePrefix + rule.UID,
			Recording: rule.Recording,
			Queries:   queries,
		})
		if err != nil {
			return outcome, err
		}
	}

	// Only datasources that speak PromQL are asked; a Loki ruler holds LogQL.
	// Each is asked on its own, and one that is down takes every retry before
	// it gives up, so they are asked side by side and written in order.
	uids := r.registry.UIDsOfType(r.extractor.Allowed.Has)
	fetched := make([]struct {
		rules []grafana.DatasourceRule
		err   error
	}, len(uids))
	var group errgroup.Group
	group.SetLimit(max(r.concurrency, 1))
	for i, uid := range uids {
		group.Go(func() error {
			fetched[i].rules, fetched[i].err = r.client.DatasourceRules(ctx, uid)
			return nil
		})
	}
	_ = group.Wait()

	for i, uid := range uids {
		if err := r.check(ctx, fetched[i].err, "the rules of datasource "+uid, &outcome); err != nil {
			return outcome, err
		}
		ids := datasourceRuleIDs(uid, fetched[i].rules)
		for j, rule := range fetched[i].rules {
			err := r.write(&outcome, extract.Rule{
				ID:        ids[j],
				Recording: rule.Recording,
				// The datasource evaluates the rule, so it is what the query runs
				// against.
				Queries: []extract.RuleQuery{{DatasourceUID: uid, Expr: rule.Query}},
			})
			if err != nil {
				return outcome, err
			}
		}
	}
	return outcome, nil
}

// check decides what an error reading one source of rules means for the run.
// An instance or datasource that does not serve rules answers 404, which says
// nothing worth a warning unless rules were asked for explicitly; credentials
// that may not read them are worth one, since the output then lacks rules the
// user may expect. Anything else is counted and skipped like a dashboard that
// cannot be fetched.
func (r *ruleReader) check(ctx context.Context, err error, what string, outcome *ruleOutcome) error {
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case grafana.IsStatus(err, http.StatusNotFound) && !r.strict:
		r.log.debugf("not reading %s, which this instance does not serve: %v", what, err)
		return nil
	case grafana.IsStatus(err, http.StatusUnauthorized, http.StatusForbidden):
		r.log.warnf("cannot read %s with these credentials, so they are missing from the output: %v", what, err)
		outcome.failed++
		return nil
	case r.failFast:
		return fmt.Errorf("reading %s: %w", what, err)
	default:
		r.log.warnf("skipping %s: %v", what, err)
		outcome.failed++
		return nil
	}
}

func (r *ruleReader) write(outcome *ruleOutcome, rule extract.Rule) error {
	result := r.extractor.ExtractRule(rule)
	outcome.stats.Merge(result.Stats)
	if len(result.Queries) == 0 {
		return nil
	}
	if r.anonymizer != nil {
		result.UID = r.anonymizer.Rule(result.UID)
		for i, query := range result.Queries {
			result.Queries[i] = r.anonymizer.Query(query)
		}
	}
	if err := r.writer.WriteRule(result.UID, result.Queries); err != nil {
		return err
	}
	outcome.written++
	outcome.queries += len(result.Queries)
	return nil
}

// datasourceRuleIDs identifies the rules of a datasource, which have no uid,
// by where they are defined: the datasource, the file or Mimir namespace, the
// group and the name. Prometheus reports the file as an absolute path. Only
// the file keeps its slashes, so that the identifier splits back into its
// parts. A group may hold several rules of one name, an alert at two
// severities say, and the second and later get their place among them
// appended.
func datasourceRuleIDs(uid string, rules []grafana.DatasourceRule) []string {
	ids := make([]string, len(rules))
	seen := make(map[string]int, len(rules))
	for i, rule := range rules {
		id := extract.RulePrefix + escapePart.Replace(uid) + "/" +
			escapeFile.Replace(strings.TrimPrefix(rule.File, "/")) + "/" +
			escapePart.Replace(rule.Group) + "/" + escapePart.Replace(rule.Name)
		seen[id]++
		if n := seen[id]; n > 1 {
			id += "#" + strconv.Itoa(n)
		}
		ids[i] = id
	}
	return ids
}

// The characters that would make a rule identifier ambiguous or break the
// line it starts, percent-encoded.
var (
	escapePart = strings.NewReplacer("%", "%25", "/", "%2F", "#", "%23", ";", "%3B", "\n", "%0A", "\r", "%0D")
	escapeFile = strings.NewReplacer("%", "%25", "#", "%23", ";", "%3B", "\n", "%0A", "\r", "%0D")
)
