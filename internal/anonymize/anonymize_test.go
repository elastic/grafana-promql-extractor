package anonymize_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-promql-extractor/internal/anonymize"
)

// newAnonymizer returns an Anonymizer with a fixed salt, so that a test can
// assert on the pseudonyms themselves.
func newAnonymizer(t *testing.T) *anonymize.Anonymizer {
	t.Helper()
	a, err := anonymize.New("test-salt")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// pseudonym matches any replacement this package produces.
var pseudonym = regexp.MustCompile(`(metric|label|value|var|func|dash)_[0-9a-f]{10}`)

// shape replaces every pseudonym with its kind, so that a test can assert what
// was replaced with what without hard coding digests.
func shape(query string) string {
	return pseudonym.ReplaceAllString(query, "<$1>")
}

func TestReplacesWhatBelongsToTheUser(t *testing.T) {
	a := newAnonymizer(t)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "metric name and label matchers",
			query: `http_requests_total{job="api-server", code=~"5.."}`,
			want:  `<metric>{<label>="<value>-<value>", <label>=~"5.."}`,
		},
		{
			name:  "functions, aggregations and durations survive",
			query: `sum by (job) (rate(http_requests_total[5m]))`,
			want:  `sum by (<label>) (rate(<metric>[5m]))`,
		},
		{
			name:  "grouping list after the aggregation",
			query: `sum(rate(errors_total[$__rate_interval])) without (instance, pod)`,
			want:  `sum(rate(<metric>[$__rate_interval])) without (<label>, <label>)`,
		},
		{
			name:  "dashboard variables in every syntax",
			query: `up{ns="$namespace", pod=~"${pod:regex}", host="[[host]]"}`,
			want:  `<metric>{<label>="$<var>", <label>=~"${<var>:regex}", <label>="[[<var>]]"}`,
		},
		{
			name:  "grafana's own variables are not the user's",
			query: `rate(x[$__rate_interval]) / $__interval_ms * 1000`,
			want:  `rate(<metric>[$__rate_interval]) / $__interval_ms * 1000`,
		},
		{
			name:  "reserved labels carry meaning",
			query: `histogram_quantile(0.99, sum by (le, job) (rate(latency_bucket[5m])))`,
			want:  `histogram_quantile(0.99, sum by (le, <label>) (rate(<metric>[5m])))`,
		},
		{
			name:  "a name matcher names a metric",
			query: `{__name__=~"node_cpu.*", mode!="idle"}`,
			want:  `{__name__=~"<metric>.*", <label>!="<value>"}`,
		},
		{
			name:  "operators, numbers and comparisons survive",
			query: `100 - (avg(node_memory_free / node_memory_total) * 100) > 0.9`,
			want:  `100 - (avg(<metric> / <metric>) * 100) > 0.9`,
		},
		{
			name:  "binary operator modifiers",
			query: `a_total * on (cluster) group_left (owner) b_info unless c_total`,
			want:  `<metric> * on (<label>) group_left (<label>) <metric> unless <metric>`,
		},
		{
			name:  "recording rule names",
			query: `cluster:cpu:rate5m`,
			want:  `<metric>`,
		},
		{
			name:  "regular expression syntax survives",
			query: `up{pod=~"^prefect-(agent|worker)-.*$"}`,
			want:  `<metric>{<label>=~"^<value>-(<value>|<value>)-.*$"}`,
		},
		{
			name:  "subqueries and offsets",
			query: `max_over_time(rate(x[5m])[1h:5m] offset 1d)`,
			want:  `max_over_time(rate(<metric>[5m])[1h:5m] offset 1d)`,
		},
		{
			name:  "a call PromQL does not define stays a call",
			query: `label_values(some_internal_helper(x), job)`,
			want:  `<func>(<func>(<metric>), <metric>)`,
		},
		{
			name:  "a variable in a function position",
			query: `sum(${aggregation:value}(x[5m]))`,
			want:  `sum(${<var>:value}(<metric>[5m]))`,
		},
		{
			// The unit of a duration ends up on its own when a variable
			// supplies the number.
			name:  "a variable inside a range selector",
			query: `sum(increase(requests_total[${__range_s}s])) - x[$interval_m m]`,
			want:  `sum(increase(<metric>[${__range_s}s])) - <metric>[$<var> m]`,
		},
		{
			name:  "escapes and flags keep the regular expression working",
			query: `up{pod=~"(?i)web-\d+", host=~"(?:a|b)\.example\.com", ip=~"\\d+\\.\\d+"}`,
			want:  `<metric>{<label>=~"(?i)<value>-\d+", <label>=~"(?:<value>|<value>)\.<value>\.<value>", <label>=~"\\d+\\.\\d+"}`,
		},
		{
			// A range has to keep meaning a range, or the regular expression
			// stops compiling.
			name:  "character classes keep their ranges",
			query: `up{pod=~"[a-z]+-[0-9]{2}", name=~"[A-Za-z0-9_.-]+", env=~"[internal]"}`,
			want:  `<metric>{<label>=~"[a-z]+-[0-9]{2}", <label>=~"[A-Za-z0-9_.-]+", <label>=~"[<value>]"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shape(a.Query(tt.query)); got != tt.want {
				t.Errorf("Query(%q)\n got %s\nwant %s", tt.query, got, tt.want)
			}
		})
	}
}

func TestLeavesNoOriginalIdentifierBehind(t *testing.T) {
	a := newAnonymizer(t)

	// Every one of these words would give something away.
	secrets := []string{
		"payments_api_latency_seconds", "tenant", "acme-corp", "prod-eu-west",
		"internal_helper", "customer_id", "billing",
	}
	query := `payments_api_latency_seconds{tenant="acme-corp", cluster="prod-eu-west", ` +
		`customer_id=~"$customer_id"} and on (billing) internal_helper(x)`

	got := a.Query(query)
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Errorf("the output still contains %q:\n%s", secret, got)
		}
	}
}

func TestPseudonymsAreStableAndDistinct(t *testing.T) {
	a := newAnonymizer(t)

	// The same name has to map to the same pseudonym everywhere, or the output
	// cannot be analyzed.
	first := a.Query(`sum(requests_total{job="api"})`)
	second := a.Query(`rate(requests_total{job="api"}[5m])`)

	metric := pseudonym.FindString(first)
	if metric == "" || !strings.Contains(second, metric) {
		t.Errorf("the same metric got different pseudonyms:\n%s\n%s", first, second)
	}

	// Different names have to map to different pseudonyms, and a name used as a
	// metric must not collide with the same name used as a label.
	distinct := a.Query(`a_total{a_total="a_total"}`)
	names := pseudonym.FindAllString(distinct, -1)
	if len(names) != 3 {
		t.Fatalf("expected three pseudonyms in %q", distinct)
	}
	if names[0] == names[1] || names[1] == names[2] || names[0] == names[2] {
		t.Errorf("a metric, a label and a value of the same name share a pseudonym: %s", distinct)
	}
}

func TestSaltSeparatesRuns(t *testing.T) {
	query := `requests_total{job="api"}`

	one, _ := anonymize.New("salt-one")
	two, _ := anonymize.New("salt-two")
	same, _ := anonymize.New("salt-one")

	if one.Query(query) == two.Query(query) {
		t.Error("different salts produced the same pseudonyms")
	}
	if one.Query(query) != same.Query(query) {
		t.Error("the same salt produced different pseudonyms")
	}

	// A random salt is the default, so two runs cannot be correlated.
	random1, err := anonymize.New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	random2, err := anonymize.New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if random1.Query(query) == random2.Query(query) {
		t.Error("two random salts produced the same pseudonyms")
	}
}

func TestUID(t *testing.T) {
	a := newAnonymizer(t)

	if got := shape(a.UID("prod-payments-overview")); got != "<dash>" {
		t.Errorf("UID = %q, want a dashboard pseudonym", got)
	}
	if a.UID("one") == a.UID("two") {
		t.Error("two uids share a pseudonym")
	}
	if a.UID("one") != a.UID("one") {
		t.Error("a uid is not stable")
	}
	if a.UID("") != "" {
		t.Error("an empty uid should stay empty")
	}
}

// Dashboards hold expressions that Prometheus would reject. Those still have to
// come out anonymized rather than untouched.
func TestHandlesBrokenExpressions(t *testing.T) {
	a := newAnonymizer(t)

	tests := map[string]string{
		`sum by (job) rate(secret_metric[5m]))`: `sum by (<label>) rate(<metric>[5m]))`,
		`secret_metric{job="unterminated`:       `<metric>{<label>="<value>`,
		`secret_metric{`:                        `<metric>{`,
		`}{`:                                    `}{`,
		``:                                      ``,
		`   `:                                   `   `,
		`{app="x"} |= "secret_word"`:            `{<label>="<value>"} |= "<value>"`,
	}

	for query, want := range tests {
		if got := shape(a.Query(query)); got != want {
			t.Errorf("Query(%q)\n got %s\nwant %s", query, got, want)
		}
	}
}
