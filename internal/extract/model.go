// Package extract parses Grafana dashboard JSON and pulls out the PromQL
// expressions of panel targets backed by a Prometheus-family datasource.
package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Envelope is the response shape of GET /api/dashboards/uid/:uid.
type Envelope struct {
	Dashboard Dashboard `json:"dashboard"`
	Meta      Meta      `json:"meta"`
}

// Meta carries the dashboard metadata Grafana stores outside the document.
type Meta struct {
	FolderUID   string `json:"folderUid"`
	FolderTitle string `json:"folderTitle"`
	URL         string `json:"url"`
}

// Dashboard is the subset of the dashboard document needed for extraction.
type Dashboard struct {
	UID         string      `json:"uid"`
	Title       string      `json:"title"`
	Panels      []Panel     `json:"panels"`
	Rows        []Row       `json:"rows"`
	Templating  Templating  `json:"templating"`
	Annotations Annotations `json:"annotations"`
	// Inputs is present in exported dashboards and maps ${DS_FOO} references to
	// a plugin id.
	Inputs []Input `json:"__inputs"`
}

// Annotations holds the dashboard's annotation queries.
type Annotations struct {
	List []Annotation `json:"list"`
}

// Annotation marks events on every panel of a dashboard. A Prometheus
// annotation carries its PromQL in expr, just as a panel target does; newer
// Grafana releases nest the same query in a target instead.
type Annotation struct {
	Name       string        `json:"name"`
	Expr       string        `json:"expr"`
	Target     *Target       `json:"target"`
	Datasource DatasourceRef `json:"datasource"`
}

// Query returns the annotation's expression, whichever shape it is stored in.
func (a Annotation) Query() string {
	if a.Expr != "" {
		return a.Expr
	}
	if a.Target != nil {
		return a.Target.Expr
	}
	return ""
}

// Row is a pre-Grafana-5 row containing panels.
type Row struct {
	Panels []Panel `json:"panels"`
}

// Templating holds the dashboard's variables.
type Templating struct {
	List []TemplateVar `json:"list"`
}

// Panel is a dashboard panel, possibly containing nested panels when it is a row.
type Panel struct {
	Type         string        `json:"type"`
	Title        string        `json:"title"`
	Datasource   DatasourceRef `json:"datasource"`
	Targets      []Target      `json:"targets"`
	Panels       []Panel       `json:"panels"`
	LibraryPanel *struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	} `json:"libraryPanel"`
}

// Target is a single query within a panel.
type Target struct {
	Expr       string        `json:"expr"`
	Datasource DatasourceRef `json:"datasource"`
	RefID      string        `json:"refId"`
}

// Input is an entry of the __inputs array of an exported dashboard.
type Input struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	PluginID string `json:"pluginId"`
}

// TemplateVar is a dashboard variable. For variables of type "datasource" the
// query field holds the plugin type of the datasources to choose from.
type TemplateVar struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Query    json.RawMessage `json:"query"`
	PluginID string          `json:"pluginId"`
}

// DatasourceType returns the plugin type a datasource variable selects from.
func (v TemplateVar) DatasourceType() string {
	if v.Type != "datasource" {
		return ""
	}
	if v.PluginID != "" {
		return v.PluginID
	}
	if len(v.Query) == 0 {
		return ""
	}
	// Usually a plain string such as "prometheus", but some schema versions
	// nest it in an object.
	var s string
	if err := json.Unmarshal(v.Query, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Type     string `json:"type"`
		PluginID string `json:"pluginId"`
	}
	if err := json.Unmarshal(v.Query, &obj); err == nil {
		if obj.PluginID != "" {
			return obj.PluginID
		}
		return obj.Type
	}
	return ""
}

// DatasourceRef is a reference to a datasource. Grafana has used several
// shapes over the years: absent or null (inherit), a bare string holding a
// datasource name or a variable reference such as "$datasource", or an object
// with a type and a uid, where the uid may itself be a variable reference.
type DatasourceRef struct {
	// Set is false when the field is absent or null, meaning "inherit".
	Set bool
	// Type is the plugin type, when the reference carries one.
	Type string
	// Ref is the uid, name or variable reference.
	Ref string
}

// UnmarshalJSON accepts every datasource reference shape Grafana emits.
// Unrecognized shapes are treated as absent rather than failing the dashboard.
func (d *DatasourceRef) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		d.Set = true
		d.Ref = s
		return nil
	}

	var obj struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil
	}
	if obj.Type == "" && obj.UID == "" {
		return nil
	}
	d.Set = true
	d.Type = strings.TrimSpace(obj.Type)
	d.Ref = strings.TrimSpace(obj.UID)
	return nil
}

// document accepts both shapes a dashboard arrives in: the envelope returned by
// /api/dashboards/uid/:uid, and a bare dashboard document as stored in a file or
// served by grafana.com.
type document struct {
	Dashboard *Dashboard `json:"dashboard"`
	Meta      Meta       `json:"meta"`

	UID         string      `json:"uid"`
	Title       string      `json:"title"`
	Panels      []Panel     `json:"panels"`
	Rows        []Row       `json:"rows"`
	Templating  Templating  `json:"templating"`
	Annotations Annotations `json:"annotations"`
	Inputs      []Input     `json:"__inputs"`
}

func (d *document) envelope() *Envelope {
	if d.Dashboard != nil {
		return &Envelope{Dashboard: *d.Dashboard, Meta: d.Meta}
	}
	return &Envelope{
		Meta: d.Meta,
		Dashboard: Dashboard{
			UID:         d.UID,
			Title:       d.Title,
			Panels:      d.Panels,
			Rows:        d.Rows,
			Templating:  d.Templating,
			Annotations: d.Annotations,
			Inputs:      d.Inputs,
		},
	}
}

// ParseEnvelope decodes a dashboard, accepting either the API envelope or a
// bare dashboard document.
//
// Dashboards in the wild occasionally hold an unexpected JSON type in a field
// we care about. encoding/json reports those as *json.UnmarshalTypeError after
// having decoded everything else, so the partial dashboard is returned along
// with a non-nil ErrPartialDecode to let callers keep going.
func ParseEnvelope(r io.Reader) (*Envelope, error) {
	var doc document
	err := json.NewDecoder(r).Decode(&doc)
	if err == nil {
		return doc.envelope(), nil
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return doc.envelope(), fmt.Errorf("%w: %v", ErrPartialDecode, err)
	}
	return nil, err
}

// ErrPartialDecode signals that a dashboard decoded with recoverable type errors.
var ErrPartialDecode = errors.New("dashboard decoded with type errors")
