package analyze

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	linePrefix     = regexp.MustCompile(`(?m)^line -?\d+:-?\d+: `)
	errorLineSep   = regexp.MustCompile(`\nline -?\d+:-?\d+: `)
	bracketContent = regexp.MustCompile(`\[.*\]`)
	digits         = regexp.MustCompile(`\d+`)
	atThisTime     = " at this time"
)

// ExtractErrorMessage pulls a human-readable message out of a Prometheus or
// Elasticsearch HTTP error body.
func ExtractErrorMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var prom promResponse
	if json.Unmarshal([]byte(raw), &prom) == nil {
		if prom.Error != "" {
			return prom.Error
		}
		if prom.ErrorType != "" {
			return prom.ErrorType
		}
	}

	var es struct {
		Error struct {
			Reason    string `json:"reason"`
			RootCause []struct {
				Reason string `json:"reason"`
			} `json:"root_cause"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &es) == nil {
		if es.Error.Reason != "" {
			return es.Error.Reason
		}
		if len(es.Error.RootCause) > 0 && es.Error.RootCause[0].Reason != "" {
			return es.Error.RootCause[0].Reason
		}
	}

	return raw
}

// GroupErrors normalizes error messages for aggregation, following the Java
// PromqlCoverageAnalyzer grouping rules.
func GroupErrors(errors []string) []string {
	if len(errors) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(errors))
	groups := make([]string, 0, len(errors))
	for _, raw := range errors {
		raw = ExtractErrorMessage(raw)
		for _, part := range splitErrorLines(raw) {
			g := normalizeErrorGroup(part)
			if g == "" {
				continue
			}
			if _, ok := seen[g]; ok {
				continue
			}
			seen[g] = struct{}{}
			groups = append(groups, g)
		}
	}
	return groups
}

func splitErrorLines(msg string) []string {
	parts := errorLineSep.Split(msg, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = linePrefix.ReplaceAllString(p, "")
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "Found") && strings.Contains(p, "problem") {
			continue
		}
		out = append(out, p)
	}
	return out
}

func normalizeErrorGroup(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}

	if g := canonicalErrorGroup(msg); g != "" {
		return g
	}

	if idx := strings.Index(msg, atThisTime); idx != -1 {
		return strings.TrimSpace(msg[:idx+len(atThisTime)]) + " [...]"
	}

	msg = truncateAfter(msg, "expecting {")
	msg = truncateAfter(msg, "no viable alternative at input")
	msg = truncateAfter(msg, "Invalid call to dataType on an unresolved object")
	msg = truncateAfter(msg, "Cannot parse regex")
	msg = truncateAfter(msg, "mismatched input")
	if strings.Contains(msg, "optimized incorrectly due to missing references") {
		return "optimized incorrectly due to missing references"
	}
	if strings.Contains(msg, "Function [") && strings.Contains(msg, "] does not exist") {
		return "Function [...] does not exist"
	}
	if strings.Contains(msg, "Function ") && strings.Contains(msg, " does not exist") {
		return "Function [...] does not exist"
	}
	msg = linePrefix.ReplaceAllString(msg, "")
	msg = bracketContent.ReplaceAllString(msg, "[...]")
	msg = digits.ReplaceAllString(msg, "N")
	return msg
}

func canonicalErrorGroup(msg string) string {
	switch {
	case strings.Contains(msg, "VectorBinarySet queries with group modifiers are not supported"):
		return "VectorBinarySet queries with group modifiers are not supported at this time [...]"
	case strings.Contains(msg, "binary expressions with different grouping keys"):
		return "binary expressions with different grouping keys not supported yet"
	case strings.Contains(msg, "function [") && strings.Contains(msg, "requires a counter metric"):
		return "function [...] requires a counter metric, but [...] has type [...] [...]"
	case strings.Contains(msg, "function [") && strings.Contains(msg, "does not support counter metric"):
		return "function [...] does not support counter metric [...] of type [...]; use rate() or increase() to convert counters first [...]"
	case strings.Contains(msg, "Plan [") && strings.Contains(msg, "duplicate output attribute"):
		return "Plan [...] optimized incorrectly due to duplicate output attribute [...]"
	case strings.Contains(msg, "Plan [") && strings.Contains(msg, "optimized incorrectly"):
		return "Plan [...] optimized incorrectly [...]"
	case strings.Contains(msg, "Cannot compare incompatible types"):
		return "Cannot compare incompatible types [...] and [...] with operator [...]"
	case strings.Contains(msg, "An HTTP line is larger than"):
		return "An HTTP line is larger than N bytes."
	case strings.Contains(msg, "Expected duration or numeric value, got"):
		return "Expected duration or numeric value, got [...]"
	case strings.Contains(msg, "Function [") && strings.Contains(msg, "] does not exist"):
		return "Function [...] does not exist"
	case strings.Contains(msg, "Function ") && strings.Contains(msg, " does not exist"):
		return "Function [...] does not exist"
	case strings.Contains(msg, "VectorBinaryArithmetic queries with group modifiers are not supported"):
		return "VectorBinaryArithmetic queries with group modifiers are not supported at this time [...]"
	case strings.Contains(msg, "comparison operators are only supported at the top-level"):
		return "comparison operators are only supported at the top-level at this time [...]"
	case strings.Contains(msg, "comparison operators with non-literal right-hand side"):
		return "comparison operators with non-literal right-hand side are not supported at this time [...]"
	case strings.Contains(msg, "set operators are not supported at this time"):
		return "set operators are not supported at this time [...]"
	case strings.Contains(msg, "set operator [") && strings.Contains(msg, "is not supported at this time"):
		return "set operator [...]"
	case strings.Contains(msg, "binary expressions with nested aggregations are not supported"):
		return "binary expressions with nested aggregations are not supported at this time [...]"
	case strings.Contains(msg, "binary expressions with WITHOUT are not supported"):
		return "binary expressions with WITHOUT are not supported at this time [...]"
	case strings.Contains(msg, "regex label selectors on __name__ are not supported"):
		return "regex label selectors on __name__ are not supported at this time [...]"
	case strings.Contains(msg, "Subquery queries are not supported"):
		return "Subquery queries are not supported at this time [...]"
	case strings.Contains(msg, "@ modifiers are not supported"):
		return "@ modifiers are not supported at this time [...]"
	case strings.Contains(msg, "requires the [@timestamp] field"):
		return "requires the [@timestamp] field [...]"
	case strings.Contains(msg, "Function [") && strings.Contains(msg, "is not yet implemented"):
		return "Function [...] is not yet implemented"
	case strings.Contains(msg, "unknown PromQL function ["):
		return "Function [...] is not yet implemented"
	case strings.Contains(msg, "Found ambiguous reference to"):
		if idx := strings.Index(msg, "expecting"); idx != -1 {
			return strings.TrimSpace(msg[:idx])
		}
		return "Found ambiguous reference to [...]"
	case strings.Contains(msg, "Unknown column [") && strings.Contains(msg, "did you mean"):
		return "Unknown column [...], did you mean any of [...]?"
	case strings.Contains(msg, "Unknown column ["):
		return "Unknown column [...]"
	}
	return ""
}

func truncateAfter(s, marker string) string {
	if idx := strings.Index(s, marker); idx != -1 {
		return strings.TrimSpace(s[:idx+len(marker)])
	}
	return s
}
