package analyze

import (
	"slices"
	"strings"

	"github.com/VictoriaMetrics/metricsql"
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
}

// NewSeriesCollector returns an empty collector.
func NewSeriesCollector() *SeriesCollector {
	return &SeriesCollector{
		byKey:        make(map[seriesKey]SeriesSpec),
		globalLabels: make(map[string]struct{}),
	}
}

// AddQuery extracts metric and label references from one query.
func (c *SeriesCollector) AddQuery(query string) {
	scrubbed := ScrubQuery(query)
	expr, err := metricsql.Parse(scrubbed)
	if err != nil {
		return
	}
	rc := &refCollector{series: c.byKey, globalLabels: c.globalLabels}
	rc.walk(expr)
}

// Series returns the deduplicated series list, with global grouping labels
// filled in as bootstrap placeholders.
func (c *SeriesCollector) Series() []SeriesSpec {
	for key, spec := range c.byKey {
		for label := range c.globalLabels {
			if _, ok := spec.Labels[label]; !ok {
				spec.Labels[label] = "bootstrap"
			}
		}
		c.byKey[key] = spec
	}

	out := make([]SeriesSpec, 0, len(c.byKey))
	for _, spec := range c.byKey {
		out = append(out, spec)
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
	if label == "" || label == "__name__" {
		return
	}
	c.globalLabels[label] = struct{}{}
}

func (c *refCollector) collectMetric(me *metricsql.MetricExpr) {
	name := metricName(me)
	labels := map[string]string{}
	for _, group := range me.LabelFilterss {
		for _, lf := range group {
			if lf.Label == "" {
				continue
			}
			if lf.Label == "__name__" {
				if name == "" && !lf.IsRegexp && !lf.IsNegative {
					name = lf.Value
				}
				continue
			}
			// Equality keeps the concrete value; regexp and negative matchers
			// still need the label name in the mapping.
			if lf.IsRegexp || lf.IsNegative {
				if _, ok := labels[lf.Label]; !ok {
					labels[lf.Label] = "bootstrap"
				}
				continue
			}
			labels[lf.Label] = lf.Value
		}
	}
	if name == "" {
		return
	}
	c.add(name, labels)
}

func (c *refCollector) add(metric string, labels map[string]string) {
	if labels == nil {
		labels = map[string]string{}
	}
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
		return strings.TrimSpace(s[:i])
	}
	return s
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
