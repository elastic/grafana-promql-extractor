package analyze

import (
	"regexp"
	"strings"
)

// grafanaVariable matches the interpolation syntaxes Grafana supports:
// $name, ${name}, ${name:format} and the legacy [[name]]. $1 in label_replace
// is a PromQL capture, not a Grafana variable, and is left alone.
var grafanaVariable = regexp.MustCompile(`\$\{[^}]+\}|\$[a-zA-Z_][a-zA-Z0-9_]*|\[\[[^\[\]]+\]\]`)

// ScrubQuery replaces Grafana template variables so a query can be sent to
// Elasticsearch without knowing the dashboard's variable values. Range
// selectors that contain a variable become [1m]; a variable in function
// position becomes rate or avg, depending on its argument; remaining $var /
// ${var} / ${var:format} / [[var]] placeholders become the variable name.
func ScrubQuery(query string) string {
	query = scrubRangeVariables(query)
	return scrubIdentVariables(query)
}

func scrubRangeVariables(query string) string {
	var b strings.Builder
	b.Grow(len(query))
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case quote != 0:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(query) {
				b.WriteByte(query[i+1])
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
			b.WriteByte(c)
		case c == '[':
			end := matchingBracket(query, i)
			if end > i && rangeHasGrafanaVar(query[i+1:end]) {
				b.WriteString("[1m]")
				i = end
				continue
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func scrubIdentVariables(query string) string {
	matches := grafanaVariable.FindAllStringIndex(query, -1)
	if len(matches) == 0 {
		return query
	}
	var b strings.Builder
	b.Grow(len(query))
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		name, ok := grafanaVarName(query[start:end])
		if !ok {
			continue
		}
		b.WriteString(query[last:start])
		if open, ok := callOpenParen(query, end); ok {
			b.WriteString(functionStandIn(query, open))
		} else {
			b.WriteString(name)
		}
		last = end
	}
	b.WriteString(query[last:])
	return b.String()
}

// callOpenParen reports whether the variable ending at end is called, as in
// ${metric:value}(x[5m]), and returns the index of the opening parenthesis.
func callOpenParen(query string, end int) (int, bool) {
	i := end
	for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n') {
		i++
	}
	return i, i < len(query) && query[i] == '('
}

// functionStandIn picks a function that accepts the argument a variable in
// function position is called with, since the dropdown's options are not in
// the export: rate for a range vector (rate / increase dropdowns), avg for an
// instant vector (aggregation dropdowns).
func functionStandIn(query string, open int) string {
	if callTakesRangeVector(query, open) {
		return "rate"
	}
	return "avg"
}

// callTakesRangeVector reports whether the argument list opened at open holds a
// range or subquery selector at its own nesting level. Brackets inside label
// matchers, strings or nested calls do not count.
func callTakesRangeVector(query string, open int) bool {
	parens, braces := 0, 0
	var quote byte
	for i := open; i < len(query); i++ {
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
		case c == '{':
			braces++
		case c == '}':
			braces--
		case c == '(':
			parens++
		case c == ')':
			parens--
			if parens == 0 {
				return false
			}
		case c == '[' && parens == 1 && braces == 0:
			return true
		}
	}
	return false
}

func rangeHasGrafanaVar(inner string) bool {
	for _, match := range grafanaVariable.FindAllString(inner, -1) {
		if _, ok := grafanaVarName(match); ok {
			return true
		}
	}
	return false
}

func grafanaVarName(token string) (string, bool) {
	var name string
	switch {
	case strings.HasPrefix(token, "${") && strings.HasSuffix(token, "}"):
		name = token[2 : len(token)-1]
	case strings.HasPrefix(token, "[[") && strings.HasSuffix(token, "]]"):
		name = token[2 : len(token)-2]
	case strings.HasPrefix(token, "$"):
		name = token[1:]
	default:
		return "", false
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if name[0] >= '0' && name[0] <= '9' {
		return "", false
	}
	return name, true
}

func matchingBracket(s string, open int) int {
	depth := 0
	var quote byte
	for i := open; i < len(s); i++ {
		c := s[i]
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
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
