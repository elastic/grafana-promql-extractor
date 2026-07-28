//go:build corpus

package corpus

import (
	"regexp"
	"strings"

	"github.com/VictoriaMetrics/metricsql"
)

// grafanaVariable matches the interpolation syntaxes Grafana supports:
// $name, ${name}, ${name:format} and the legacy [[name]]. A legacy variable
// name may not contain a bracket itself, so that the inner pair is matched in
// a range selector such as x[[[interval]]].
var grafanaVariable = regexp.MustCompile(`\$\{[^}]*\}|\$[a-zA-Z_][a-zA-Z0-9_]*|\[\[[^\[\]]*\]\]`)

// numericVariables interpolate to a number rather than to an identifier.
var numericVariables = map[string]bool{
	"__interval_ms":      true,
	"__range_ms":         true,
	"__rate_interval_ms": true,
	"__from":             true,
	"__to":               true,
	"__dashboard_ms":     true,
}

// Interpolate substitutes Grafana variables so that a dashboard expression can
// be handed to a PromQL parser. Variables are replaced by a value that is valid
// in the position they appear in: a duration inside a range selector, a number
// for the millisecond variables, and an identifier everywhere else.
func Interpolate(query string) string {
	matches := grafanaVariable.FindAllStringIndex(query, -1)
	if len(matches) == 0 {
		return query
	}
	inBracket := bracketDepths(query)

	var b strings.Builder
	b.Grow(len(query))
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		b.WriteString(query[last:start])
		b.WriteString(replacement(variableName(query[start:end]), inBracket[start]))
		last = end
	}
	b.WriteString(query[last:])
	return b.String()
}

func replacement(name string, inBracket bool) string {
	switch {
	case inBracket:
		return "5m"
	case numericVariables[name]:
		return "1000"
	case strings.HasSuffix(name, "_interval") || name == "interval" || name == "__interval":
		return "5m"
	default:
		return "grafanavar"
	}
}

// variableName strips the interpolation syntax and any format specifier.
func variableName(token string) string {
	name := token
	switch {
	case strings.HasPrefix(name, "${"):
		name = strings.TrimSuffix(strings.TrimPrefix(name, "${"), "}")
	case strings.HasPrefix(name, "[["):
		name = strings.TrimSuffix(strings.TrimPrefix(name, "[["), "]]")
	case strings.HasPrefix(name, "$"):
		name = name[1:]
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// bracketDepths reports, for every byte offset, whether it sits inside a square
// bracket outside of a quoted string, which is where a duration is expected.
func bracketDepths(query string) []bool {
	inside := make([]bool, len(query)+1)
	depth := 0
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			if depth > 0 {
				depth--
			}
		}
		inside[i] = depth > 0
	}
	return inside
}

// ParsesAsPromQL reports whether an expression parses once its Grafana
// variables are interpolated. MetricsQL is a superset of PromQL, so this is a
// check that an expression is PromQL-shaped rather than a strict validation:
// it rejects LogQL, SQL and Flux, but accepts Graphite-style dotted names,
// which PromQL has no syntax for.
func ParsesAsPromQL(query string) error {
	_, err := metricsql.Parse(Interpolate(query))
	return err
}

// LooksLikeLogQL finds a pipe outside of a quoted string. PromQL has no pipe
// operator, while every LogQL pipeline stage starts with one, so a bare pipe is
// a reliable sign that a log query slipped through.
func LooksLikeLogQL(query string) bool {
	found := false
	scan(query, func(_ int, c byte) bool {
		if c == '|' {
			found = true
		}
		return !found
	})
	return found
}

// HasBareStreamSelector reports whether the expression selects series without
// naming a metric, the shape of every LogQL stream selector. PromQL can do the
// same through __name__, which is excluded here, and is rare otherwise.
func HasBareStreamSelector(query string) bool {
	if strings.Contains(query, "__name__") {
		return false
	}
	found := false
	scan(query, func(i int, c byte) bool {
		if c != '{' {
			return true
		}
		j := i - 1
		for j >= 0 && (query[j] == ' ' || query[j] == '\t') {
			j--
		}
		if j < 0 || !isNameByte(query[j]) {
			found = true
		}
		return !found
	})
	return found
}

func isNameByte(c byte) bool {
	return c == '_' || c == ':' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// scan calls visit for every byte outside a string literal, until visit
// returns false.
func scan(query string, visit func(i int, c byte) bool) {
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		default:
			if !visit(i, c) {
				return
			}
		}
	}
}
