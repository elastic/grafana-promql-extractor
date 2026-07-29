package grafana_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/grafana"
	"github.com/felixbarny/grafana-dashboard-extractor/internal/testsupport"
)

func TestLoadRegistryFromDatasourcesAPI(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	registry, err := grafana.LoadRegistry(context.Background(), client)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	if registry.Source != "/api/datasources" {
		t.Errorf("Source = %q, want /api/datasources", registry.Source)
	}
	if registry.Count != len(testsupport.Datasources()) {
		t.Errorf("Count = %d, want %d", registry.Count, len(testsupport.Datasources()))
	}
	if got := registry.DefaultType(); got != "prometheus" {
		t.Errorf("DefaultType() = %q, want prometheus", got)
	}
	if fake.Requests("/api/frontend/settings") != 0 {
		t.Error("the fallback endpoint should not be used when /api/datasources works")
	}

	assertLookup(t, registry, testsupport.PrometheusUID, "prometheus")
	assertLookup(t, registry, testsupport.PrometheusName, "prometheus")
	assertLookup(t, registry, testsupport.LokiUID, "loki")
	assertLookup(t, registry, testsupport.CloudWatchName, "cloudwatch")

	if _, ok := registry.Lookup("does-not-exist"); ok {
		t.Error("Lookup of an unknown datasource should fail")
	}
}

// TestLoadRegistryFallsBackToFrontendSettings covers Viewer-role tokens, for
// which /api/datasources is forbidden.
func TestLoadRegistryFallsBackToFrontendSettings(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{DatasourcesStatus: status})
		client := mustClient(t, grafana.Config{BaseURL: fake.URL})

		registry, err := grafana.LoadRegistry(context.Background(), client)
		if err != nil {
			t.Fatalf("LoadRegistry after %d: %v", status, err)
		}
		if registry.Source != "/api/frontend/settings" {
			t.Errorf("Source = %q, want /api/frontend/settings", registry.Source)
		}
		if got := registry.DefaultType(); got != "prometheus" {
			t.Errorf("DefaultType() = %q, want prometheus", got)
		}
		assertLookup(t, registry, testsupport.PrometheusUID, "prometheus")
		assertLookup(t, registry, testsupport.LokiName, "loki")
	}
}

func TestLoadRegistryFailsWhenBothEndpointsFail(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		DatasourcesStatus:      http.StatusForbidden,
		FrontendSettingsStatus: http.StatusForbidden,
	})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	_, err := grafana.LoadRegistry(context.Background(), client)
	if err == nil {
		t.Fatal("expected an error when both endpoints are forbidden")
	}
	if !strings.Contains(err.Error(), "frontend/settings") {
		t.Errorf("error should mention the attempted fallback, got %v", err)
	}
}

func TestLoadRegistryPropagatesUnexpectedErrors(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		DatasourcesStatus: http.StatusInternalServerError,
	})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL, Retries: 0})

	if _, err := grafana.LoadRegistry(context.Background(), client); err == nil {
		t.Fatal("expected a 500 to fail rather than fall back")
	}
	if fake.Requests("/api/frontend/settings") != 0 {
		t.Error("a 500 is not a permission problem and must not trigger the fallback")
	}
}

func TestRegistryDefaultFromSeparateField(t *testing.T) {
	// /api/frontend/settings reports the default by name rather than by flag.
	registry := grafana.NewRegistry([]grafana.Datasource{
		{UID: "a", Name: "Prom", Type: "prometheus"},
		{UID: "b", Name: "Logs", Type: "loki"},
	}, "Prom", "test")

	if got := registry.DefaultType(); got != "prometheus" {
		t.Errorf("DefaultType() = %q, want prometheus", got)
	}
}

func TestRegistryLookupIsCaseInsensitiveFallback(t *testing.T) {
	registry := grafana.NewRegistry([]grafana.Datasource{
		{UID: "abc", Name: "Prometheus Main", Type: "prometheus"},
	}, "", "test")

	assertLookup(t, registry, "prometheus main", "prometheus")
	assertLookup(t, registry, "Prometheus Main", "prometheus")
}

func TestRegistryNilSafe(t *testing.T) {
	var registry *grafana.Registry
	if _, ok := registry.Lookup("anything"); ok {
		t.Error("Lookup on a nil registry should report not found")
	}
	if registry.DefaultType() != "" {
		t.Error("DefaultType on a nil registry should be empty")
	}
}

func assertLookup(t *testing.T, registry *grafana.Registry, ref, want string) {
	t.Helper()
	got, ok := registry.Lookup(ref)
	if !ok {
		t.Errorf("Lookup(%q) not found, want %q", ref, want)
		return
	}
	if got != want {
		t.Errorf("Lookup(%q) = %q, want %q", ref, got, want)
	}
}
