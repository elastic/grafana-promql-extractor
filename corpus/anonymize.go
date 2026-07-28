//go:build corpus

package corpus

import (
	"regexp"
	"strings"

	"github.com/VictoriaMetrics/metricsql"
)

// pseudonym matches a replacement the anonymizer produces.
var pseudonym = regexp.MustCompile(`^(metric|label|value|var|func|dash)_[0-9a-f]{10}$`)

// SurvivingIdentifiers returns the words of an anonymized query that are
// neither a pseudonym nor part of the vocabulary that PromQL and Grafana share,
// and which therefore came from the dashboard. The vocabulary is deliberately
// assembled from a different source than the anonymizer's own: the function
// names come from a PromQL parser, so that a word the anonymizer decided to keep
// is only accepted here if something else also calls it public.
func SurvivingIdentifiers(query string) []string {
	var surviving []string
	for _, word := range Words(dropSyntax(query)) {
		if !isPublicWord(word) {
			surviving = append(surviving, word)
		}
	}
	return surviving
}

var (
	// variableFormat matches the format specifier of a variable reference, the
	// ":date:seconds" of ${__to:date:seconds}.
	variableFormat = regexp.MustCompile(`(\$\{[^}:]*):[^}]*(\})|(\[\[[^\]:]*):[^\]]*(\]\])`)
	// regexGroupFlags matches the flags of a regular expression group, the "i"
	// of "(?i)".
	regexGroupFlags = regexp.MustCompile(`\(\?[imsU]*([):])`)
	// characterClass matches a regular expression class, whose contents are
	// characters rather than names, and characterRange one range inside it.
	characterClass = regexp.MustCompile(`\[[^\]]*\]`)
	characterRange = regexp.MustCompile(`[A-Za-z0-9]-[A-Za-z0-9]`)
)

// dropSyntax removes the spelling that belongs to Grafana or to regular
// expressions rather than to the dashboard: variable format specifiers, whose
// date formats can spell out anything, and group flags. Neither can give
// anything away, and neither can be listed word by word.
func dropSyntax(query string) string {
	query = variableFormat.ReplaceAllString(query, "$1$2$3$4")
	query = regexGroupFlags.ReplaceAllString(query, "($1")
	// A range spans two characters, so splitting "[a-z0-9]" into words would
	// report the fragment "z0" as if a dashboard had named something.
	return characterClass.ReplaceAllStringFunc(query, func(class string) string {
		return characterRange.ReplaceAllString(class, "")
	})
}

// Words splits out the identifier-like words of an expression. A word that
// continues a number, the "m" of 5m or the "e" of 1e6, belongs to that number
// rather than being an identifier of its own.
func Words(s string) []string {
	var words []string
	for i := 0; i < len(s); {
		if !isWordStart(s[i]) || i > 0 && isWordPart(s[i-1]) {
			i++
			continue
		}
		start := i
		for i < len(s) && isWordPart(s[i]) {
			i++
		}
		words = append(words, s[start:i])
	}
	return words
}

func isWordStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isWordPart(c byte) bool {
	return isWordStart(c) || c >= '0' && c <= '9'
}

// isPublicWord reports whether a word means the same in every Grafana instance
// and therefore gives nothing away.
func isPublicWord(word string) bool {
	lower := strings.ToLower(word)
	switch {
	case pseudonym.MatchString(word):
		return true
	case len(word) == 1:
		// One letter names nothing. They turn up where a regular expression
		// class lists characters, as the a and c of "[a|c]".
		return true
	case strings.HasPrefix(word, "__"):
		// Prometheus reserves these label names, Grafana these variables.
		return true
	case word == "le" || word == "quantile":
		return true
	case metricsql.IsSupportedFunction(lower):
		return true
	case promQLWords[lower], grafanaFormats[lower], durationUnits[lower]:
		return true
	}
	return false
}

// promQLWords are the words of the language that the parser above does not
// report as functions: operators, modifiers, the numeric literals spelled as
// words, and the functions Prometheus has added since.
var promQLWords = words(`
	by without on ignoring group_left group_right offset bool and or unless
	atan2 inf nan info double_exponential_smoothing limit_ratio
	histogram_avg histogram_count histogram_stddev histogram_stdvar histogram_sum
	ts_of_max_over_time ts_of_min_over_time ts_of_last_over_time
`)

// durationUnits stand on their own where a variable supplies the number of a
// duration, as in [${__range_s}s].
var durationUnits = words(`ms s m h d w y`)

// grafanaFormats are the format specifiers a variable reference may carry, as
// in ${host:csv}. They are Grafana's own words, kept so that the reference
// keeps working.
var grafanaFormats = words(`
	csv date distributed doublequote glob html json key lucene percentencode pipe
	queryparam raw regex singlequote sqlstring text value
`)

func words(list string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(list) {
		set[word] = true
	}
	return set
}
