//go:build corpus

package corpus

import (
	"encoding/json"
	"sort"
	"strings"
)

// FoundExpr is an expr field discovered by walking a dashboard document
// without any knowledge of the dashboard schema.
type FoundExpr struct {
	// Location is the chain of object keys leading to the expression, with
	// array indices omitted, for example "panels/targets" or "annotations/list".
	Location string
	Expr     string
}

// FindExprs walks a dashboard document and returns every non-empty expr string
// in it. It is deliberately schema-agnostic: comparing its results with the
// extractor's output shows what the extractor leaves behind and why.
func FindExprs(raw []byte) []FoundExpr {
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil
	}
	var found []FoundExpr
	walk(document, nil, &found)
	sort.SliceStable(found, func(i, j int) bool { return found[i].Location < found[j].Location })
	return found
}

func walk(node any, path []string, found *[]FoundExpr) {
	switch value := node.(type) {
	case map[string]any:
		// Sorted so that repeated runs report the same order.
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			if key == "expr" {
				if expr, ok := value[key].(string); ok && strings.TrimSpace(expr) != "" {
					*found = append(*found, FoundExpr{
						Location: strings.Join(path, "/"),
						Expr:     Normalize(expr),
					})
					continue
				}
			}
			walk(value[key], append(path, key), found)
		}
	case []any:
		for _, item := range value {
			walk(item, path, found)
		}
	}
}

// Normalize applies the two rules the output format documents, collapsing an
// expression onto one line and dropping its comments, so that expressions
// found here can be compared with extracted ones. It is a second, independent
// implementation of those rules on purpose.
func Normalize(expr string) string {
	var b strings.Builder
	b.Grow(len(expr))

	var quote byte
	space, comment := false, false

	for i := 0; i < len(expr); i++ {
		c := expr[i]

		if comment {
			comment = c != '\n'
			continue
		}
		if quote != 0 {
			if c == '\n' || c == '\r' {
				c = ' '
			}
			b.WriteByte(c)
			switch {
			case c == '\\' && i+1 < len(expr):
				i++
				b.WriteByte(expr[i])
			case c == quote:
				quote = 0
			}
			continue
		}

		switch {
		case c == '#':
			comment, space = true, true
			continue
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			space = true
			continue
		}

		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		if c == '"' || c == '\'' || c == '`' {
			quote = c
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}
