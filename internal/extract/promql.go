package extract

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// DefaultDatasourceTypes are the plugin types treated as PromQL sources.
var DefaultDatasourceTypes = []string{
	"prometheus",
	"grafana-amazonprometheus-datasource",
	"grafana-mimir-datasource",
}

// Datasource references with a special meaning rather than a real datasource.
const (
	refMixed       = "-- mixed --"
	refDashboard   = "-- dashboard --"
	refGrafana     = "-- grafana --"
	refExpressions = "-- expressions --"
	refExpr        = "__expr__"
)

// DatasourceLookup resolves datasource references to plugin types.
type DatasourceLookup interface {
	// Lookup resolves a datasource uid or name to its plugin type.
	Lookup(uidOrName string) (string, bool)
	// DefaultType returns the plugin type of the default datasource, or "".
	DefaultType() string
}

// TypeSet is a case-insensitive set of datasource plugin types.
type TypeSet map[string]struct{}

// NewTypeSet builds a TypeSet from a list of plugin types.
func NewTypeSet(types []string) TypeSet {
	set := make(TypeSet, len(types))
	for _, t := range types {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			set[t] = struct{}{}
		}
	}
	return set
}

// Has reports whether the set contains a plugin type.
func (s TypeSet) Has(t string) bool {
	_, ok := s[strings.ToLower(strings.TrimSpace(t))]
	return ok
}

// List returns the sorted plugin types in the set.
func (s TypeSet) List() []string {
	return slices.Sorted(maps.Keys(s))
}

// Extractor pulls PromQL expressions out of dashboards.
type Extractor struct {
	// Lookup resolves datasource references. It may be nil, in which case only
	// references that carry their own plugin type can be resolved.
	Lookup DatasourceLookup
	// Allowed is the set of datasource plugin types to extract from.
	Allowed TypeSet
	// IncludeUnresolved keeps expressions whose datasource type cannot be
	// determined, on the assumption that an "expr" field is most likely PromQL.
	IncludeUnresolved bool
	// Dedupe drops repeated identical expressions within a dashboard.
	Dedupe bool
}

// Stats records what happened during extraction, for the run summary.
type Stats struct {
	Dashboards         int
	Panels             int
	Targets            int
	Annotations        int
	Queries            int
	Duplicates         int
	SkippedEmpty       int
	SkippedLogs        int
	SkippedSpecial     int
	SkippedByType      map[string]int
	UnresolvedIncluded int
	UnresolvedSkipped  int
	LibraryPanels      int
	PartialDecodes     int
}

// Merge accumulates other into s.
func (s *Stats) Merge(other Stats) {
	s.Dashboards += other.Dashboards
	s.Panels += other.Panels
	s.Targets += other.Targets
	s.Annotations += other.Annotations
	s.Queries += other.Queries
	s.Duplicates += other.Duplicates
	s.SkippedEmpty += other.SkippedEmpty
	s.SkippedLogs += other.SkippedLogs
	s.SkippedSpecial += other.SkippedSpecial
	s.UnresolvedIncluded += other.UnresolvedIncluded
	s.UnresolvedSkipped += other.UnresolvedSkipped
	s.LibraryPanels += other.LibraryPanels
	s.PartialDecodes += other.PartialDecodes
	for t, n := range other.SkippedByType {
		if s.SkippedByType == nil {
			s.SkippedByType = make(map[string]int, len(other.SkippedByType))
		}
		s.SkippedByType[t] += n
	}
}

// TopSkippedTypes returns the skipped datasource types ordered by count.
func (s *Stats) TopSkippedTypes(limit int) []TypeCount {
	counts := make([]TypeCount, 0, len(s.SkippedByType))
	for t, n := range s.SkippedByType {
		counts = append(counts, TypeCount{Type: t, Count: n})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count != counts[j].Count {
			return counts[i].Count > counts[j].Count
		}
		return counts[i].Type < counts[j].Type
	})
	if limit > 0 && len(counts) > limit {
		counts = counts[:limit]
	}
	return counts
}

// TypeCount is a datasource plugin type and how often it was seen.
type TypeCount struct {
	Type  string
	Count int
}

// Result is the outcome of extracting one dashboard.
type Result struct {
	UID     string
	Title   string
	Queries []string
	Stats   Stats
}

// Extract collects the PromQL expressions of a dashboard.
func (e *Extractor) Extract(env *Envelope) Result {
	dash := &env.Dashboard
	res := Result{UID: dash.UID, Title: dash.Title}
	res.Stats.Dashboards = 1

	var seen map[string]struct{}
	if e.Dedupe {
		seen = make(map[string]struct{})
	}

	// appendQuery reports whether the expression ended up in the output.
	appendQuery := func(expr string) bool {
		query := normalizeQuery(expr)
		if query == "" {
			res.Stats.SkippedEmpty++
			return false
		}
		if seen != nil {
			if _, dup := seen[query]; dup {
				res.Stats.Duplicates++
				return false
			}
			seen[query] = struct{}{}
		}
		res.Queries = append(res.Queries, query)
		res.Stats.Queries++
		return true
	}

	// consider keeps an expression when the datasource backing it speaks PromQL.
	consider := func(inherited, ref DatasourceRef, expr string) bool {
		pluginType, special := e.resolve(dash, inherited, ref)
		switch {
		case special:
			res.Stats.SkippedSpecial++
		case pluginType == "":
			if e.IncludeUnresolved && normalizeQuery(expr) != "" {
				res.Stats.UnresolvedIncluded++
				return appendQuery(expr)
			}
			res.Stats.UnresolvedSkipped++
		case e.Allowed.Has(pluginType):
			return appendQuery(expr)
		default:
			if res.Stats.SkippedByType == nil {
				res.Stats.SkippedByType = make(map[string]int)
			}
			res.Stats.SkippedByType[strings.ToLower(pluginType)]++
		}
		return false
	}

	var visit func(panel Panel, inherited DatasourceRef)
	visit = func(panel Panel, inherited DatasourceRef) {
		res.Stats.Panels++

		panelRef := panel.Datasource
		if !panelRef.Set {
			panelRef = inherited
		}

		if panel.LibraryPanel != nil {
			// Library panel targets live outside the dashboard document.
			res.Stats.LibraryPanels++
		}

		if strings.EqualFold(panel.Type, "logs") {
			res.Stats.SkippedLogs += len(panel.Targets)
		} else {
			for _, target := range panel.Targets {
				res.Stats.Targets++
				consider(panelRef, target.Datasource, target.Expr)
			}
		}

		for _, nested := range panel.Panels {
			visit(nested, panelRef)
		}
	}

	for _, panel := range dash.Panels {
		visit(panel, DatasourceRef{})
	}
	for _, row := range dash.Rows {
		for _, panel := range row.Panels {
			visit(panel, DatasourceRef{})
		}
	}

	// Annotation queries run against the datasource just like panel queries do.
	for _, annotation := range dash.Annotations.List {
		query := annotation.Query()
		if normalizeQuery(query) == "" {
			continue
		}
		if consider(DatasourceRef{}, annotation.Datasource, query) {
			res.Stats.Annotations++
		}
	}

	return res
}

// resolve determines the datasource plugin type backing a target. It reports
// special=true for references that never carry PromQL, and an empty type when
// the reference cannot be resolved.
func (e *Extractor) resolve(dash *Dashboard, panelRef, targetRef DatasourceRef) (pluginType string, special bool) {
	ref := targetRef
	if !ref.Set {
		ref = panelRef
	}
	if !ref.Set {
		return e.defaultType(), false
	}

	if isSpecial(ref) {
		// A mixed datasource delegates to the per-target references, which have
		// already been preferred above; anything else carries no PromQL.
		return "", !isMixed(ref)
	}

	if ref.Type != "" && !isGenericType(ref.Type) {
		return ref.Type, false
	}

	if name, ok := variableName(ref.Ref); ok {
		return e.resolveVariable(dash, name), false
	}
	if ref.Ref != "" && e.Lookup != nil {
		if t, ok := e.Lookup.Lookup(ref.Ref); ok {
			return t, false
		}
	}
	return "", false
}

// resolveVariable resolves a "$datasource" style reference through the
// dashboard's own variables. A datasource variable names the plugin type it
// selects from, and exported dashboards record the same in __inputs.
func (e *Extractor) resolveVariable(dash *Dashboard, name string) string {
	for _, v := range dash.Templating.List {
		if !strings.EqualFold(v.Name, name) {
			continue
		}
		if t := v.DatasourceType(); t != "" {
			return t
		}
	}
	for _, in := range dash.Inputs {
		if strings.EqualFold(in.Name, name) && in.PluginID != "" {
			return in.PluginID
		}
	}
	return ""
}

func (e *Extractor) defaultType() string {
	if e.Lookup == nil {
		return ""
	}
	return e.Lookup.DefaultType()
}

func isSpecial(ref DatasourceRef) bool {
	switch strings.ToLower(ref.Ref) {
	case refMixed, refDashboard, refGrafana, refExpressions, refExpr:
		return true
	}
	switch strings.ToLower(ref.Type) {
	case "dashboard", "grafana", refExpr, "mixed":
		return true
	}
	return false
}

func isMixed(ref DatasourceRef) bool {
	return strings.EqualFold(ref.Ref, refMixed) || strings.EqualFold(ref.Type, "mixed")
}

// isGenericType reports whether a type field is a placeholder rather than a
// concrete plugin type.
func isGenericType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "datasource", "mixed":
		return true
	}
	return false
}

// variableName extracts the variable name from "$name", "${name}",
// "${name:format}" or the legacy "[[name]]" syntax.
func variableName(ref string) (string, bool) {
	switch {
	case strings.HasPrefix(ref, "${") && strings.HasSuffix(ref, "}"):
		name := ref[2 : len(ref)-1]
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		return name, name != ""
	case strings.HasPrefix(ref, "$"):
		name := ref[1:]
		return name, name != ""
	case strings.HasPrefix(ref, "[[") && strings.HasSuffix(ref, "]]"):
		name := ref[2 : len(ref)-2]
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		return name, name != ""
	}
	return "", false
}

// normalizeQuery collapses the whitespace of an expression onto a single line,
// since the output format is one query per line and multi-line expressions are
// common in dashboards.
//
// A PromQL comment runs to the end of its line, so it has to be dropped rather
// than folded into the line below it, which would comment out the rest of the
// expression.
func normalizeQuery(expr string) string {
	if strings.TrimSpace(expr) == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(expr))
	var quote rune
	escaped, comment, space := false, false, false

	for _, r := range expr {
		switch {
		case comment:
			comment = r != '\n'
			continue

		case quote != 0:
			// Inside a string literal, where # starts no comment. A raw line
			// break is not valid there, but keep the one-line guarantee of the
			// output format regardless.
			if r == '\n' || r == '\r' {
				r = ' '
			}
			b.WriteRune(r)
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == quote:
				quote = 0
			}
			continue

		case r == '#':
			// Drop the comment, but keep the tokens around it apart.
			comment, space = true, true
			continue

		case unicode.IsSpace(r):
			space = true
			continue
		}

		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		if r == '"' || r == '\'' || r == '`' {
			quote = r
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
