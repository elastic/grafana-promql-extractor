package analyze

import "regexp"

var (
	bracketVar = regexp.MustCompile(`\[\$\w+\]`)
	dollarVar  = regexp.MustCompile(`\$(\w+)`)
	braceVar   = regexp.MustCompile(`\$\{(\w+)\}`)
)

// ScrubQuery replaces Grafana template variables so a query can be sent to
// Elasticsearch without knowing the dashboard's variable values.
func ScrubQuery(query string) string {
	query = bracketVar.ReplaceAllString(query, "[1m]")
	query = dollarVar.ReplaceAllString(query, "$1")
	query = braceVar.ReplaceAllString(query, "$1")
	return query
}
