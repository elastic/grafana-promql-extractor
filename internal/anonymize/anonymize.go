// Package anonymize rewrites the identifiers of a PromQL query into stable
// pseudonyms, so that extracted queries can be shared without revealing metric
// names, label names, label values, dashboard variable names or dashboard uids.
//
// What belongs to PromQL or to Grafana is kept: functions, aggregations,
// operators, modifiers, durations, numbers, the reserved labels le and
// quantile, any label whose name starts with __, and Grafana's built-in $__
// variables. The shape of a query therefore survives, which is what keeps the
// output worth analyzing.
package anonymize

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// pseudonymBytes is how much of the digest a pseudonym carries. Five bytes keep
// a collision between two names unlikely well past a million distinct names,
// while keeping the output readable.
const pseudonymBytes = 5

// The kind of an identifier, which both prefixes its pseudonym and separates
// the namespaces, so that a metric and a label of the same name do not end up
// with the same pseudonym.
const (
	kindMetric    = "metric_"
	kindLabel     = "label_"
	kindValue     = "value_"
	kindVariable  = "var_"
	kindFunction  = "func_"
	kindDashboard = "dash_"
)

// Anonymizer maps identifiers to pseudonyms. It is safe for concurrent use.
type Anonymizer struct {
	salt []byte
}

// New returns an Anonymizer. An empty salt means a random one, which makes the
// pseudonyms irreversible but also different on every run; pass a secret of
// your own to keep them comparable across runs.
func New(salt string) (*Anonymizer, error) {
	if salt != "" {
		return &Anonymizer{salt: []byte(salt)}, nil
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generating an anonymization salt: %w", err)
	}
	return &Anonymizer{salt: random}, nil
}

// UID rewrites a dashboard uid, which is often named after what it monitors.
func (a *Anonymizer) UID(uid string) string {
	if uid == "" {
		return uid
	}
	return a.pseudonym(kindDashboard, uid)
}

// Query rewrites every identifier of a PromQL expression.
func (a *Anonymizer) Query(query string) string {
	s := &scanner{a: a, src: query}
	s.run()
	return s.out.String()
}

// pseudonym is the stable stand-in for one identifier. The salt makes it
// irreversible: without it, a name cannot be recovered from the digest, not
// even by trying a dictionary of likely names.
func (a *Anonymizer) pseudonym(kind, name string) string {
	h := sha256.New()
	h.Write(a.salt)
	h.Write([]byte(kind))
	h.Write([]byte(name))

	digest := make([]byte, 0, sha256.Size)
	return kind + hex.EncodeToString(h.Sum(digest)[:pseudonymBytes])
}

// scanner walks a query and copies it through, rewriting the identifiers it
// recognizes. It never fails: an expression it cannot make sense of, and
// dashboards hold plenty of those, still comes out with its identifiers
// replaced.
type scanner struct {
	a   *Anonymizer
	src string
	i   int
	out strings.Builder

	// parens records for every open parenthesis whether it holds a list of
	// label names, as the one after "by" does.
	parens []bool
	// braces counts the open label matchers.
	braces int
	// brackets counts the open range selectors, which hold durations.
	brackets int
	// labelList marks that the next parenthesis opens a list of label names.
	labelList bool
	// matcherLabel is the label the current matcher compares, so that the value
	// of a __name__ matcher can be renamed as the metric it is.
	matcherLabel string
}

func (s *scanner) run() {
	s.out.Grow(len(s.src) + len(s.src)/2)

	for s.i < len(s.src) {
		switch c := s.src[s.i]; {
		case c == '"' || c == '\'' || c == '`':
			s.stringLiteral()
		case c == '$', c == '[' && strings.HasPrefix(s.src[s.i:], "[["):
			s.variable()
		case s.startsIdentifier():
			s.identifier()
		case isDigit(c):
			s.number()
		default:
			s.punctuation(c)
		}
	}
}

// startsIdentifier reports whether a name begins at the current position. A
// colon can be part of a recording rule name, but it also separates the two
// durations of a subquery, so it only starts a name when a letter follows.
func (s *scanner) startsIdentifier() bool {
	c := s.src[s.i]
	if c != ':' {
		return isIdentStart(c)
	}
	return s.i+1 < len(s.src) && (isLetter(s.src[s.i+1]) || s.src[s.i+1] == '_')
}

// identifier rewrites a bare word, whose meaning follows from where it sits.
func (s *scanner) identifier() {
	name := s.take(isIdentByte)
	lower := strings.ToLower(name)

	switch {
	case s.brackets > 0 && durationUnits[lower]:
		// A range selector holds a duration, whose unit can end up on its own
		// when a variable supplies the number, as in [${__range_s}s].
		s.out.WriteString(name)

	case s.inLabelList():
		// Inside a matcher or a grouping list every word names a label.
		s.matcherLabel = name
		if isReservedLabel(name) {
			s.out.WriteString(name)
		} else {
			s.out.WriteString(s.a.pseudonym(kindLabel, name))
		}

	case keywords[lower]:
		s.labelList = groupingKeywords[lower]
		s.out.WriteString(name)

	case functions[lower] && s.callFollows():
		s.out.WriteString(name)

	case s.callFollows():
		// Something is being called that PromQL does not define. Keeping the
		// name would risk leaking one, so it is replaced, but stays
		// recognizable as a call.
		s.out.WriteString(s.a.pseudonym(kindFunction, name))

	default:
		s.out.WriteString(s.a.pseudonym(kindMetric, name))
	}
}

// variable rewrites a Grafana variable in any of its syntaxes, keeping the
// syntax itself and any format specifier.
func (s *scanner) variable() {
	token, rest := s.a.variableToken(s.src[s.i:])
	s.out.WriteString(token)
	s.i = len(s.src) - len(rest)
}

// variableName keeps Grafana's own variables, which are the same everywhere,
// and replaces the dashboard's.
func (a *Anonymizer) variableName(name string) string {
	if strings.HasPrefix(name, "__") {
		return name
	}
	return a.pseudonym(kindVariable, name)
}

// stringLiteral rewrites the words of a string, which is a label value in a
// matcher and an argument elsewhere. Everything else in it is left alone, so
// that regular expression syntax, digits and separators survive.
func (s *scanner) stringLiteral() {
	quote := s.src[s.i]
	start := s.i
	s.i++

	end := -1
	for s.i < len(s.src) {
		c := s.src[s.i]
		if c == '\\' && quote != '`' && s.i+1 < len(s.src) {
			s.i += 2
			continue
		}
		s.i++
		if c == quote {
			end = s.i - 1
			break
		}
	}

	// The value of a __name__ matcher is a metric name, so name it as one.
	kind := kindValue
	if s.braces > 0 && s.matcherLabel == "__name__" {
		kind = kindMetric
	}

	if end < 0 {
		// Unterminated, which Prometheus would reject; rewrite what is there.
		s.out.WriteByte(quote)
		s.out.WriteString(s.a.value(s.src[start+1:], kind))
		return
	}
	s.out.WriteByte(quote)
	s.out.WriteString(s.a.value(s.src[start+1:end], kind))
	s.out.WriteByte(quote)
}

// value rewrites the words of a string, leaving its punctuation in place.
func (a *Anonymizer) value(content, kind string) string {
	var b strings.Builder
	b.Grow(len(content))

	for i := 0; i < len(content); {
		switch c := content[i]; {
		case c == '\\':
			// An escape is syntax, so "\d" stays a digit class rather than
			// losing its d to a pseudonym. Doubled backslashes are how a
			// dashboard writes one, and the letter after them belongs to the
			// regular expression just the same.
			n := 1
			for i+n < len(content) && content[i+n] == '\\' {
				n++
			}
			if i+n < len(content) {
				n++
			}
			b.WriteString(content[i : i+n])
			i += n
		case c == '(':
			// The flags of a group, the i of "(?i)", are syntax too.
			n := max(regexGroupPrefix(content[i:]), 1)
			b.WriteString(content[i : i+n])
			i += n
		case c == '$', c == '[' && strings.HasPrefix(content[i:], "[["):
			// A variable reference means the same inside a string as outside.
			token, rest := a.variableToken(content[i:])
			b.WriteString(token)
			i = len(content) - len(rest)
		case c == '[' && classEnd(content[i:]) > 0:
			n := classEnd(content[i:])
			b.WriteString(a.characterClass(content[i:i+n], kind))
			i += n
		case isIdentStart(c) && c != ':':
			start := i
			for i < len(content) && isWordByte(content[i]) {
				i++
			}
			b.WriteString(a.pseudonym(kind, content[start:i]))
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// characterClass rewrites the contents of a regular expression class, which are
// characters rather than names. Ranges and single characters stay as they are,
// since a-z has to keep meaning a-z and one letter gives nothing away; a longer
// run is still replaced, so that "[internal]" does not name anything.
func (a *Anonymizer) characterClass(class, kind string) string {
	var b strings.Builder
	b.Grow(len(class))
	b.WriteByte('[')

	inner := class[1 : len(class)-1]
	for i := 0; i < len(inner); {
		switch {
		case inner[i] == '\\' && i+1 < len(inner):
			b.WriteString(inner[i : i+2])
			i += 2

		case i+2 < len(inner) && inner[i+1] == '-' && isWordByte(inner[i]) && isWordByte(inner[i+2]):
			b.WriteString(inner[i : i+3])
			i += 3

		case isWordByte(inner[i]):
			start := i
			for i < len(inner) && isWordByte(inner[i]) {
				i++
				if i+1 < len(inner) && inner[i+1] == '-' {
					// The next character opens a range, which is syntax.
					break
				}
			}
			if run := inner[start:i]; len(run) == 1 {
				b.WriteString(run)
			} else {
				b.WriteString(a.pseudonym(kind, run))
			}

		default:
			b.WriteByte(inner[i])
			i++
		}
	}

	b.WriteByte(']')
	return b.String()
}

// classEnd returns the length of the character class starting at s[0], the
// closing bracket included, or zero if it is not closed. A bracket right after
// the opening one is a literal, as in "[]]".
func classEnd(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case ']':
			if i > 1 {
				return i + 1
			}
		}
	}
	return 0
}

// variableToken rewrites a leading variable reference and returns what follows.
func (a *Anonymizer) variableToken(s string) (rewritten, rest string) {
	if strings.HasPrefix(s, "[[") {
		if name, format, rest, ok := cut(s[2:], "]]"); ok {
			return "[[" + a.variableName(name) + format + "]]", rest
		}
	}
	if strings.HasPrefix(s, "${") {
		if name, format, rest, ok := cut(s[2:], "}"); ok {
			return "${" + a.variableName(name) + format + "}", rest
		}
	}
	if strings.HasPrefix(s, "$") && len(s) > 1 && isIdentStart(s[1]) {
		end := 1
		for end < len(s) && isWordByte(s[end]) {
			end++
		}
		return "$" + a.variableName(s[1:end]), s[end:]
	}
	return s[:1], s[1:]
}

// number copies a number or duration through. Letters that belong to it, as in
// 5m or 1e6, are part of the token rather than an identifier.
func (s *scanner) number() {
	s.out.WriteString(s.take(func(c byte) bool {
		return isDigit(c) || isLetter(c) || c == '.'
	}))
}

func (s *scanner) punctuation(c byte) {
	switch c {
	case '{':
		s.braces++
		s.matcherLabel = ""
	case '}':
		if s.braces > 0 {
			s.braces--
		}
		s.matcherLabel = ""
	case '(':
		s.parens = append(s.parens, s.labelList)
		s.labelList = false
	case ')':
		if n := len(s.parens); n > 0 {
			s.parens = s.parens[:n-1]
		}
	case '[':
		s.brackets++
	case ']':
		if s.brackets > 0 {
			s.brackets--
		}
	case ',':
		s.matcherLabel = ""
	}
	s.out.WriteByte(c)
	s.i++
}

// inLabelList reports whether words at this position name labels.
func (s *scanner) inLabelList() bool {
	if s.braces > 0 {
		return true
	}
	return len(s.parens) > 0 && s.parens[len(s.parens)-1]
}

// callFollows reports whether the next non-blank character opens an argument
// list, since PromQL allows a space between a function and its arguments.
func (s *scanner) callFollows() bool {
	for j := s.i; j < len(s.src); j++ {
		switch s.src[j] {
		case ' ', '\t':
		case '(':
			return true
		default:
			return false
		}
	}
	return false
}

// take consumes the run of bytes matching accept and returns it.
func (s *scanner) take(accept func(byte) bool) string {
	start := s.i
	for s.i < len(s.src) && accept(s.src[s.i]) {
		s.i++
	}
	return s.src[start:s.i]
}

// cut splits a variable reference at its closing delimiter, separating the name
// from a format specifier such as the ":csv" of ${host:csv}.
func cut(s, closing string) (name, format, rest string, ok bool) {
	inside, rest, ok := strings.Cut(s, closing)
	if !ok {
		return "", "", s, false
	}
	name = inside
	if i := strings.IndexByte(inside, ':'); i >= 0 {
		name, format = inside[:i], inside[i:]
	}
	if strings.TrimSpace(name) == "" {
		return "", "", s, false
	}
	return name, format, rest, true
}

// regexGroupPrefix returns the length of a group opener whose letters are
// regular expression flags rather than words, as in "(?i)" or "(?s:". It
// returns zero for anything else, including a plain "(".
func regexGroupPrefix(s string) int {
	if !strings.HasPrefix(s, "(?") {
		return 0
	}
	n := 2
	for n < len(s) && strings.IndexByte("imsU", s[n]) >= 0 {
		n++
	}
	if n < len(s) && (s[n] == ')' || s[n] == ':') {
		return n
	}
	return 0
}

// isReservedLabel reports whether a label name is Prometheus's rather than the
// user's. Labels starting with __ are reserved, le and quantile carry the
// meaning of histograms and summaries.
func isReservedLabel(name string) bool {
	return strings.HasPrefix(name, "__") || name == "le" || name == "quantile"
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// isIdentStart accepts what may begin a PromQL identifier, including the colon
// of a recording rule name.
func isIdentStart(c byte) bool { return isLetter(c) || c == '_' || c == ':' }
func isIdentByte(c byte) bool  { return isIdentStart(c) || isDigit(c) }

// isWordByte accepts what may continue a variable or a word inside a value.
func isWordByte(c byte) bool { return isLetter(c) || isDigit(c) || c == '_' }

// keywords are the PromQL words that are never user data: aggregations, binary
// operators, modifiers and the numeric literals spelled as words. They are kept
// whether or not they are called, since "sum by (job) (x)" writes an
// aggregation without a following parenthesis.
var keywords = words(`
	sum min max avg group stddev stdvar count count_values topk bottomk quantile
	limitk limit_ratio
	by without on ignoring group_left group_right offset bool and or unless atan2
	inf nan
`)

// groupingKeywords are followed by a parenthesized list of label names.
var groupingKeywords = words(`by without on ignoring group_left group_right`)

// durationUnits are the units of a Prometheus duration.
var durationUnits = words(`ms s m h d w y`)

// functions are the PromQL functions, kept where they are called.
var functions = words(`
	abs absent absent_over_time acos acosh asin asinh atan atanh avg_over_time
	ceil changes clamp clamp_max clamp_min cos cosh count_over_time
	days_in_month day_of_month day_of_week day_of_year deg delta deriv
	double_exponential_smoothing exp floor histogram_avg histogram_count
	histogram_fraction histogram_quantile histogram_stddev histogram_stdvar
	histogram_sum holt_winters hour idelta increase info irate label_join
	label_replace last_over_time ln log10 log2 mad_over_time max_over_time
	min_over_time minute month pi predict_linear present_over_time
	quantile_over_time rad rate resets round scalar sgn sin sinh sort
	sort_by_label sort_by_label_desc sort_desc sqrt stddev_over_time
	stdvar_over_time sum_over_time tan tanh time timestamp vector year
	start end
`)

func words(list string) map[string]bool {
	set := make(map[string]bool)
	for _, word := range strings.Fields(list) {
		set[word] = true
	}
	return set
}
