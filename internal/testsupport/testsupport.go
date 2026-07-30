// Package testsupport holds the dashboard fixtures and datasource definitions
// shared by the unit tests and the dockerized Grafana integration tests, so
// that both tiers assert against the same expectations.
package testsupport

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/elastic/grafana-promql-extractor/internal/grafana"
)

//go:embed testdata/dashboards
var fixtureFS embed.FS

const fixtureDir = "testdata/dashboards"

// Datasource UIDs referenced by the fixtures. The integration tests provision
// datasources with exactly these UIDs and names.
const (
	PrometheusUID          = "prom-main"
	PrometheusName         = "Prometheus Main"
	PrometheusSecondaryUID = "prom-secondary"
	PrometheusSecondary    = "Prometheus Secondary"
	LokiUID                = "loki-main"
	LokiName               = "Loki Main"
	CloudWatchUID          = "cw-main"
	CloudWatchName         = "CloudWatch Main"
)

// Datasources returns the datasource set the fixtures expect.
func Datasources() []grafana.Datasource {
	return []grafana.Datasource{
		{UID: PrometheusUID, Name: PrometheusName, Type: "prometheus", IsDefault: true},
		{UID: PrometheusSecondaryUID, Name: PrometheusSecondary, Type: "prometheus"},
		{UID: LokiUID, Name: LokiName, Type: "loki"},
		{UID: CloudWatchUID, Name: CloudWatchName, Type: "cloudwatch"},
	}
}

// Registry returns a datasource registry matching Datasources.
func Registry() *grafana.Registry {
	return grafana.NewRegistry(Datasources(), "", "testsupport")
}

// ProvisioningYAML renders the datasources as a Grafana provisioning file.
func ProvisioningYAML() string {
	var b strings.Builder
	b.WriteString("apiVersion: 1\ndatasources:\n")
	for _, ds := range Datasources() {
		fmt.Fprintf(&b, "  - name: %q\n", ds.Name)
		fmt.Fprintf(&b, "    uid: %s\n", ds.UID)
		fmt.Fprintf(&b, "    type: %s\n", ds.Type)
		fmt.Fprintf(&b, "    access: proxy\n")
		fmt.Fprintf(&b, "    isDefault: %t\n", ds.IsDefault)
		fmt.Fprintf(&b, "    editable: false\n")
		switch ds.Type {
		case "prometheus":
			fmt.Fprintf(&b, "    url: http://prometheus.invalid:9090\n")
		case "loki":
			fmt.Fprintf(&b, "    url: http://loki.invalid:3100\n")
		case "cloudwatch":
			fmt.Fprintf(&b, "    jsonData:\n      authType: keys\n      defaultRegion: eu-central-1\n")
		}
	}
	return b.String()
}

// Fixture is a dashboard fixture and the output it must produce under the
// default extraction settings.
type Fixture struct {
	// Name is the fixture file name without its extension.
	Name string
	// UID is the dashboard uid inside the document.
	UID string
	// Title is the dashboard title.
	Title string
	// JSON is the raw dashboard document.
	JSON []byte
	// Expected holds the "uid;query" lines the fixture must yield, in order.
	Expected []string
}

// Fixtures returns every dashboard fixture, sorted by name.
func Fixtures() ([]Fixture, error) {
	entries, err := fixtureFS.ReadDir(fixtureDir)
	if err != nil {
		return nil, err
	}

	var fixtures []Fixture
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")

		raw, err := fixtureFS.ReadFile(path.Join(fixtureDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		expectedRaw, err := fixtureFS.ReadFile(path.Join(fixtureDir, name+".expected"))
		if err != nil {
			return nil, fmt.Errorf("fixture %s has no .expected file: %w", name, err)
		}

		var meta struct {
			UID   string `json:"uid"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("fixture %s: %w", name, err)
		}

		fixtures = append(fixtures, Fixture{
			Name:     name,
			UID:      meta.UID,
			Title:    meta.Title,
			JSON:     raw,
			Expected: splitLines(string(expectedRaw)),
		})
	}

	sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].Name < fixtures[j].Name })
	return fixtures, nil
}

// ExpectedLines returns the expected output lines of every fixture combined.
func ExpectedLines(fixtures []Fixture) []string {
	var lines []string
	for _, f := range fixtures {
		lines = append(lines, f.Expected...)
	}
	return lines
}

// splitLines splits on newlines and drops blank lines.
func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
