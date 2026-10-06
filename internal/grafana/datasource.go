package grafana

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Registry maps datasource UIDs and names to their plugin type, so that
// dashboard datasource references can be resolved to a concrete type.
type Registry struct {
	byUID  map[string]string
	byName map[string]string
	// lowered holds lowercased names as a last-resort lookup, since dashboard
	// JSON occasionally differs in case from the configured datasource name.
	lowered map[string]string

	defaultType string
	// uidsByType lists the datasource uids of each lowercased plugin type.
	uidsByType map[string][]string

	// Source records which endpoint the registry was loaded from.
	Source string
	// Count is the number of datasources discovered.
	Count int
}

// NewRegistry builds a registry from a list of datasources.
func NewRegistry(datasources []Datasource, defaultRef, source string) *Registry {
	r := &Registry{
		byUID:      make(map[string]string, len(datasources)),
		byName:     make(map[string]string, len(datasources)),
		lowered:    make(map[string]string, len(datasources)),
		uidsByType: make(map[string][]string),
		Source:     source,
		Count:      len(datasources),
	}
	for _, ds := range datasources {
		if ds.Type == "" {
			continue
		}
		if ds.UID != "" {
			r.byUID[ds.UID] = ds.Type
			key := strings.ToLower(ds.Type)
			r.uidsByType[key] = append(r.uidsByType[key], ds.UID)
		}
		if ds.Name != "" {
			r.byName[ds.Name] = ds.Type
			r.lowered[strings.ToLower(ds.Name)] = ds.Type
		}
		if ds.IsDefault {
			r.defaultType = ds.Type
		}
	}
	// /api/frontend/settings reports the default separately, by name or uid.
	if r.defaultType == "" && defaultRef != "" {
		if t, ok := r.Lookup(defaultRef); ok {
			r.defaultType = t
		}
	}
	return r
}

// Datasource is the subset of a Grafana datasource definition we need.
type Datasource struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	IsDefault bool   `json:"isDefault"`
}

// Lookup resolves a datasource UID or name to a plugin type.
func (r *Registry) Lookup(uidOrName string) (string, bool) {
	if r == nil || uidOrName == "" {
		return "", false
	}
	if t, ok := r.byUID[uidOrName]; ok {
		return t, true
	}
	if t, ok := r.byName[uidOrName]; ok {
		return t, true
	}
	t, ok := r.lowered[strings.ToLower(uidOrName)]
	return t, ok
}

// DefaultType returns the plugin type of the instance's default datasource.
func (r *Registry) DefaultType() string {
	if r == nil {
		return ""
	}
	return r.defaultType
}

// UIDsOfType returns the uids of every datasource whose plugin type the
// predicate accepts, sorted, so that callers can visit them in a stable order.
func (r *Registry) UIDsOfType(accept func(pluginType string) bool) []string {
	if r == nil {
		return nil
	}
	var uids []string
	for t, list := range r.uidsByType {
		if accept(t) {
			uids = append(uids, list...)
		}
	}
	sort.Strings(uids)
	return uids
}

// LoadRegistry fetches the datasource list. It prefers /api/datasources, which
// requires the datasources:read permission, and falls back to
// /api/frontend/settings, which exposes the same plugin types to any
// authenticated user (including Viewer-role service accounts).
func LoadRegistry(ctx context.Context, c *Client) (*Registry, error) {
	var list []Datasource
	err := c.GetJSON(ctx, "/api/datasources", nil, &list)
	if err == nil {
		return NewRegistry(list, "", "/api/datasources"), nil
	}
	if !IsStatus(err, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound) {
		return nil, fmt.Errorf("listing datasources: %w", err)
	}
	c.cfg.Logf("GET /api/datasources was rejected (%v), falling back to /api/frontend/settings", err)

	settings, ferr := loadFrontendSettings(ctx, c)
	if ferr != nil {
		return nil, fmt.Errorf("listing datasources: %w (fallback to /api/frontend/settings also failed: %v)", err, ferr)
	}
	return settings, nil
}

func loadFrontendSettings(ctx context.Context, c *Client) (*Registry, error) {
	var settings struct {
		Datasources       map[string]Datasource `json:"datasources"`
		DefaultDatasource string                `json:"defaultDatasource"`
	}
	if err := c.GetJSON(ctx, "/api/frontend/settings", nil, &settings); err != nil {
		return nil, err
	}

	list := make([]Datasource, 0, len(settings.Datasources))
	for name, ds := range settings.Datasources {
		if ds.Name == "" {
			ds.Name = name
		}
		list = append(list, ds)
	}
	return NewRegistry(list, settings.DefaultDatasource, "/api/frontend/settings"), nil
}
