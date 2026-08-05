package analyze

import (
	"slices"
	"strings"

	"github.com/VictoriaMetrics/metricsql"
)

const (
	bootstrapLabel = "bootstrap"
	metricLabel    = "__name__"
)

// SeriesSpec is one time series to seed through remote write.
type SeriesSpec struct {
	Metric string
	Labels map[string]string
}

type seriesKey struct {
	metric string
	labels string
}

// SeriesCollector gathers referenced metrics incrementally while scanning an
// export file, without holding every query string.
type SeriesCollector struct {
	byKey        map[seriesKey]SeriesSpec
	globalLabels map[string]struct{}
	parseSkipped int
}

// NewSeriesCollector returns an empty collector.
func NewSeriesCollector() *SeriesCollector {
	return &SeriesCollector{
		byKey:        make(map[seriesKey]SeriesSpec),
		globalLabels: make(map[string]struct{}),
	}
}

// AddQuery extracts metric and label references from one query. Queries that
// fail to parse after scrubbing are skipped; ParseSkipped counts them.
func (c *SeriesCollector) AddQuery(query string) {
	scrubbed := ScrubQuery(query)
	expr, err := metricsql.Parse(scrubbed)
	if err != nil {
		c.parseSkipped++
		return
	}
	rc := &refCollector{series: c.byKey, globalLabels: c.globalLabels}
	rc.walk(expr)
}

// ParseSkipped returns how many AddQuery calls could not be parsed.
func (c *SeriesCollector) ParseSkipped() int {
	return c.parseSkipped
}

// Series returns the deduplicated series list, with global grouping labels
// filled in as bootstrap placeholders. Entries that differ before
// materialization can collapse to the same remote-write identity once missing
// labels are filled with bootstrap placeholders; those are merged here so a
// seed pass does not send duplicate samples in one batch. It does not mutate
// collected state.
func (c *SeriesCollector) Series() []SeriesSpec {
	seen := make(map[string]struct{}, len(c.byKey))
	out := make([]SeriesSpec, 0, len(c.byKey))
	for _, spec := range c.byKey {
		mat := materializeSeries(spec, c.globalLabels)
		key := remoteWriteSeriesKey(mat)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, mat)
	}
	return out
}

type refCollector struct {
	series       map[seriesKey]SeriesSpec
	globalLabels map[string]struct{}
}

func (c *refCollector) walk(e metricsql.Expr) {
	switch expr := e.(type) {
	case *metricsql.FuncExpr:
		for _, arg := range expr.Args {
			c.walk(arg)
		}
	case *metricsql.AggrFuncExpr:
		if expr.Modifier.Op != "" {
			for _, label := range expr.Modifier.Args {
				if label == "*" {
					continue
				}
				c.noteLabel(label)
			}
		}
		for _, arg := range expr.Args {
			c.walk(arg)
		}
	case *metricsql.RollupExpr:
		c.walk(expr.Expr)
	case *metricsql.MetricExpr:
		c.collectMetric(expr)
	case *metricsql.BinaryOpExpr:
		if expr.GroupModifier.Op != "" {
			for _, label := range expr.GroupModifier.Args {
				if label == "*" {
					continue
				}
				c.noteLabel(label)
			}
		}
		if expr.JoinModifier.Op != "" {
			for _, label := range expr.JoinModifier.Args {
				if label == "*" {
					continue
				}
				c.noteLabel(label)
			}
		}
		c.walk(expr.Left)
		c.walk(expr.Right)
	}
}

func (c *refCollector) noteLabel(label string) {
	if label == "" || label == metricLabel {
		return
	}
	c.globalLabels[label] = struct{}{}
}

func (c *refCollector) collectMetric(me *metricsql.MetricExpr) {
	name := metricName(me)
	if name == "" {
		return
	}
	labels := map[string]string{}
	for _, group := range me.LabelFilterss {
		for _, lf := range group {
			if lf.Label == "" || lf.Label == metricLabel {
				continue
			}
			if lf.IsRegexp || lf.IsNegative {
				setBootstrapIfAbsent(labels, lf.Label)
				continue
			}
			labels[lf.Label] = lf.Value
		}
	}
	c.add(name, labels)
}

func setBootstrapIfAbsent(labels map[string]string, name string) {
	if _, ok := labels[name]; !ok {
		labels[name] = bootstrapLabel
	}
}

func materializeSeries(spec SeriesSpec, globalLabels map[string]struct{}) SeriesSpec {
	labels := copyLabels(spec.Labels)
	for label := range globalLabels {
		setBootstrapIfAbsent(labels, label)
	}
	return SeriesSpec{Metric: spec.Metric, Labels: labels}
}

func (c *refCollector) add(metric string, labels map[string]string) {
	key := seriesKey{metric: metric, labels: labelKey(labels)}
	spec, ok := c.series[key]
	if !ok {
		c.series[key] = SeriesSpec{Metric: metric, Labels: copyLabels(labels)}
		return
	}
	for k, v := range labels {
		if _, exists := spec.Labels[k]; !exists {
			spec.Labels[k] = v
		}
	}
	c.series[key] = spec
}

func metricName(me *metricsql.MetricExpr) string {
	b := me.AppendString(nil)
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if name := strings.TrimSpace(s[:i]); name != "" {
			return name
		}
	} else if s != "" {
		return s
	}
	for _, group := range me.LabelFilterss {
		for _, lf := range group {
			if lf.Label == metricLabel && !lf.IsRegexp && !lf.IsNegative {
				return lf.Value
			}
		}
	}
	return ""
}

func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
