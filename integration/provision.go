//go:build integration

// Package integration runs the extractor against a real Grafana instance in a
// container. Build with -tags=integration to include it.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/felixbarny/grafana-promql-extractor/internal/testsupport"
)

const (
	adminUser     = "admin"
	adminPassword = "extractor-integration"

	// FolderUID holds the generated dashboards, so the folder filter can be
	// tested against a known subset.
	FolderUID   = "fx-folder"
	folderTitle = "Generated dashboards"

	grafanaPort     = "3000/tcp"
	defaultImage    = "grafana/grafana:latest"
	startupTimeout  = 3 * time.Minute
	indexingTimeout = 60 * time.Second
)

// Instance is a running Grafana with the fixtures provisioned.
type Instance struct {
	URL string
	// ViewerToken is a service account token with the Viewer role, which is not
	// allowed to read /api/datasources.
	ViewerToken string
	// Fixtures are the curated dashboards, stored at the root level.
	Fixtures []testsupport.Fixture
	// Generated are the synthetic dashboards, stored in FolderUID.
	Generated []testsupport.Fixture

	client *http.Client
}

// All returns every provisioned dashboard.
func (i *Instance) All() []testsupport.Fixture {
	return append(append([]testsupport.Fixture{}, i.Fixtures...), i.Generated...)
}

// AdminUser and AdminPassword expose the basic auth credentials.
func (i *Instance) AdminUser() string     { return adminUser }
func (i *Instance) AdminPassword() string { return adminPassword }

// image returns the Grafana image under test.
func image() string {
	if img := strings.TrimSpace(os.Getenv("GRAFANA_IMAGE")); img != "" {
		return img
	}
	return defaultImage
}

// Start boots the Grafana under test, provisions datasources and dashboards,
// and mints a Viewer service account token. The container is terminated on test
// cleanup.
func Start(t *testing.T, generatedCount int) *Instance {
	t.Helper()
	return StartImage(t, image(), generatedCount)
}

// StartImage is Start against a named release, for tests that cover several.
func StartImage(t *testing.T, image string, generatedCount int) *Instance {
	t.Helper()
	requireDocker(t)

	ctx := context.Background()
	provisioningPath := writeProvisioningFile(t)

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{grafanaPort},
			Env: map[string]string{
				"GF_SECURITY_ADMIN_USER":         adminUser,
				"GF_SECURITY_ADMIN_PASSWORD":     adminPassword,
				"GF_AUTH_ANONYMOUS_ENABLED":      "false",
				"GF_ANALYTICS_REPORTING_ENABLED": "false",
				"GF_ANALYTICS_CHECK_FOR_UPDATES": "false",
				"GF_LOG_LEVEL":                   "warn",
			},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      provisioningPath,
				ContainerFilePath: "/etc/grafana/provisioning/datasources/fixtures.yaml",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForHTTP("/api/health").
				WithPort(grafanaPort).
				WithStartupTimeout(startupTimeout).
				WithResponseMatcher(func(body io.Reader) bool {
					var health struct{ Database string }
					if err := json.NewDecoder(body).Decode(&health); err != nil {
						return false
					}
					return health.Database == "ok"
				}),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("starting Grafana %s: %v", image, err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminating Grafana: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, grafanaPort)
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}

	instance := &Instance{
		URL:       fmt.Sprintf("http://%s:%s", host, port.Port()),
		Fixtures:  fixtures,
		Generated: testsupport.GeneratedFixtures(generatedCount),
		// Provisioning can run concurrently, so pool more than the two
		// connections per host the default transport keeps.
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 16},
		},
	}

	instance.waitForAPI(t)
	instance.createFolder(t)
	for _, fixture := range instance.Fixtures {
		instance.uploadDashboard(t, fixture, "")
	}
	for _, fixture := range instance.Generated {
		instance.uploadDashboard(t, fixture, FolderUID)
	}
	instance.waitForSearch(t, len(instance.All()))
	instance.ViewerToken = instance.createViewerToken(t)

	return instance
}

// waitForAPI blocks until Grafana answers an authenticated request. A healthy
// database is not the same as a working instance: some releases report health
// while still starting, and then leave the first real request waiting past the
// client timeout, which looks like a failed test rather than a slow boot.
func (i *Instance) waitForAPI(t *testing.T) {
	t.Helper()

	probe := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(startupTimeout)
	for attempt := 1; ; attempt++ {
		resp, err := probe.Do(i.request(t, http.MethodGet, "/api/search?limit=1", nil))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if attempt > 1 {
					t.Logf("Grafana answered its first request on attempt %d", attempt)
				}
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Grafana did not answer within %s: %v", startupTimeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// URLWithoutDatasourcesAPI returns a proxy in front of Grafana that rejects
// /api/datasources and forwards everything else untouched.
//
// Whether a Viewer-role token may read /api/datasources has changed across
// Grafana releases, so this makes the fallback to /api/frontend/settings
// testable against a real instance regardless of the version under test.
func (i *Instance) URLWithoutDatasourcesAPI(t *testing.T) string {
	t.Helper()

	target, err := url.Parse(i.URL)
	if err != nil {
		t.Fatalf("parsing instance url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/datasources" {
			http.Error(w, `{"message":"Access denied"}`, http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	return server.URL
}

func requireDocker(t *testing.T) {
	t.Helper()
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Skipf("skipping integration test: no docker provider: %v", err)
	}
	defer provider.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := provider.Health(ctx); err != nil {
		t.Skipf("skipping integration test: docker is not available: %v", err)
	}
}

// writeProvisioningFile renders the shared datasource definitions so the
// container and the unit tests agree on uids, names and types.
func writeProvisioningFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixtures.yaml")
	if err := os.WriteFile(path, []byte(testsupport.ProvisioningYAML()), 0o644); err != nil {
		t.Fatalf("writing provisioning file: %v", err)
	}
	return path
}

func (i *Instance) createFolder(t *testing.T) {
	t.Helper()
	body := map[string]string{"uid": FolderUID, "title": folderTitle}
	status, response := i.post(t, "/api/folders", body)
	// A pre-existing folder is fine; anything else is not.
	if status != http.StatusOK && status != http.StatusConflict && status != http.StatusPreconditionFailed {
		t.Fatalf("creating folder: status %d: %s", status, response)
	}
}

func (i *Instance) uploadDashboard(t *testing.T, fixture testsupport.Fixture, folderUID string) {
	t.Helper()

	var dashboard map[string]any
	if err := json.Unmarshal(fixture.JSON, &dashboard); err != nil {
		t.Fatalf("fixture %s: %v", fixture.Name, err)
	}
	// Grafana assigns the numeric id; sending one that does not exist fails.
	delete(dashboard, "id")

	payload := map[string]any{
		"dashboard": dashboard,
		"overwrite": true,
		"message":   "provisioned by integration tests",
	}
	if folderUID != "" {
		payload["folderUid"] = folderUID
	}

	status, response := i.post(t, "/api/dashboards/db", payload)
	if status != http.StatusOK {
		t.Fatalf("uploading dashboard %s: status %d: %s", fixture.Name, status, response)
	}
}

// waitForSearch waits until every uploaded dashboard is searchable, since
// newer Grafana releases can index asynchronously.
func (i *Instance) waitForSearch(t *testing.T, want int) {
	t.Helper()

	deadline := time.Now().Add(indexingTimeout)
	for {
		req := i.request(t, http.MethodGet, "/api/search?type=dash-db&limit=5000", nil)
		resp, err := i.client.Do(req)
		if err != nil {
			t.Fatalf("searching dashboards: %v", err)
		}
		var hits []struct {
			UID string `json:"uid"`
		}
		err = json.NewDecoder(resp.Body).Decode(&hits)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decoding search response: %v", err)
		}

		if len(hits) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d dashboards are searchable after %s", len(hits), want, indexingTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// createViewerToken mints a Viewer-role service account token. Such a token is
// rejected by /api/datasources, which is what makes it useful here.
func (i *Instance) createViewerToken(t *testing.T) string {
	t.Helper()

	status, response := i.post(t, "/api/serviceaccounts", map[string]any{
		"name":       "extractor-viewer",
		"role":       "Viewer",
		"isDisabled": false,
	})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("creating service account: status %d: %s", status, response)
	}
	var account struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(response, &account); err != nil {
		t.Fatalf("decoding service account: %v: %s", err, response)
	}

	status, response = i.post(t, fmt.Sprintf("/api/serviceaccounts/%d/tokens", account.ID),
		map[string]any{"name": "extractor-viewer-token"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("creating service account token: status %d: %s", status, response)
	}
	var token struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(response, &token); err != nil {
		t.Fatalf("decoding token: %v: %s", err, response)
	}
	if token.Key == "" {
		t.Fatalf("empty service account token: %s", response)
	}
	return token.Key
}

func (i *Instance) post(t *testing.T, path string, payload any) (int, []byte) {
	t.Helper()

	status, response, err := i.postJSON(path, payload)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return status, response
}

// postJSON returns the error instead of failing the test, so that callers can
// post concurrently, where t.Fatalf is not allowed.
func (i *Instance) postJSON(path string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, i.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(adminUser, adminPassword)
	req.Header.Set("Content-Type", "application/json")

	resp, err := i.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	response, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, response, nil
}

func (i *Instance) request(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, i.URL+path, body)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.SetBasicAuth(adminUser, adminPassword)
	return req
}
