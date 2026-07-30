package grafana_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/felixbarny/grafana-dashboard-extractor/internal/grafana"
)

// bulkServer serves a dashboard collection of the given size, paginated the way
// Grafana does it: a page holds at most pageSize dashboards and carries the
// offset of the next one as an opaque token.
type bulkServer struct {
	total    int
	pageSize int
	// breakAfter, when positive, cuts the response short after that many
	// dashboards of the first page, as a connection dropping mid-stream would.
	breakAfter int
	// emptyAt, when positive, answers the request for that offset with nothing
	// at all, once, the way a busy instance has been seen to.
	emptyAt  int
	requests int
	tokens   []string
}

func (b *bulkServer) start(t *testing.T) *grafana.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			fmt.Fprint(w, `{"database":"ok","version":"13.0.0"}`)
			return
		}
		b.requests++
		token := r.URL.Query().Get("continue")
		b.tokens = append(b.tokens, token)

		start := 0
		if token != "" {
			start, _ = strconv.Atoi(token)
		}
		limit := b.pageSize
		if asked, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && asked < limit {
			limit = asked
		}
		end := min(start+limit, b.total)

		if b.emptyAt > 0 && start == b.emptyAt {
			b.emptyAt = 0
			fmt.Fprint(w, `{"kind":"DashboardList","metadata":{},"items":[]}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		var body strings.Builder
		body.WriteString(`{"kind":"DashboardList","metadata":{`)
		if end < b.total {
			fmt.Fprintf(&body, `"continue":"%d"`, end)
		}
		body.WriteString(`},"items":[`)
		for i := start; i < end; i++ {
			if i > start {
				body.WriteString(",")
			}
			fmt.Fprintf(&body, `{"metadata":{"name":"dash-%d"},"spec":{"title":"Dashboard %d"}}`, i, i)
		}
		body.WriteString("]}")

		if b.breakAfter > 0 && start == 0 {
			// Send a prefix that ends mid-item, then hang up.
			cut := strings.Index(body.String(), fmt.Sprintf(`{"metadata":{"name":"dash-%d"}`, b.breakAfter))
			b.breakAfter = 0
			w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
			w.Write([]byte(body.String()[:cut]))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		w.Write([]byte(body.String()))
	}))
	t.Cleanup(server.Close)

	client, err := grafana.New(grafana.Config{BaseURL: server.URL, Retries: 3})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return client
}

func collect(t *testing.T, client *grafana.Client, opt grafana.ListOptions) []string {
	t.Helper()

	var uids []string
	err := client.ListDashboards(context.Background(), opt, func(doc grafana.DashboardDocument) error {
		var spec struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(doc.Document, &spec); err != nil {
			t.Errorf("dashboard %s carries no usable document: %v", doc.UID, err)
		}
		if spec.Title == "" {
			t.Errorf("dashboard %s carries an empty document", doc.UID)
		}
		uids = append(uids, doc.UID)
		return nil
	})
	if err != nil {
		t.Fatalf("ListDashboards: %v", err)
	}
	return uids
}

func TestListsEveryDashboardAcrossPages(t *testing.T) {
	server := &bulkServer{total: 25, pageSize: 10}
	uids := collect(t, server.start(t), grafana.ListOptions{})

	if len(uids) != 25 {
		t.Fatalf("listed %d dashboards, want 25", len(uids))
	}
	for i, uid := range uids {
		if want := fmt.Sprintf("dash-%d", i); uid != want {
			t.Fatalf("dashboard %d is %s, want %s", i, uid, want)
		}
	}
	if server.requests != 3 {
		t.Errorf("made %d requests for 25 dashboards in pages of 10, want 3", server.requests)
	}
}

func TestListStopsAtMax(t *testing.T) {
	server := &bulkServer{total: 100, pageSize: 10}
	uids := collect(t, server.start(t), grafana.ListOptions{Max: 12})

	if len(uids) != 12 {
		t.Fatalf("listed %d dashboards, want the 12 asked for", len(uids))
	}
	if server.requests > 2 {
		t.Errorf("made %d requests for 12 dashboards in pages of 10, want no more than 2", server.requests)
	}
}

func TestListResumesFromContinueToken(t *testing.T) {
	server := &bulkServer{total: 25, pageSize: 10}
	uids := collect(t, server.start(t), grafana.ListOptions{ContinueToken: "20"})

	if len(uids) != 5 || uids[0] != "dash-20" {
		t.Fatalf("listed %v, want the five dashboards after the token", uids)
	}
}

// TestListReportsThePageItIsOn covers the token an interrupted run resumes from:
// it has to point at the page being yielded, not the one after it.
func TestListReportsThePageItIsOn(t *testing.T) {
	server := &bulkServer{total: 25, pageSize: 10}
	var reported []string
	opt := grafana.ListOptions{OnPage: func(token string) { reported = append(reported, token) }}
	collect(t, server.start(t), opt)

	want := []string{"", "10", "20"}
	if fmt.Sprint(reported) != fmt.Sprint(want) {
		t.Errorf("reported tokens %v, want %v", reported, want)
	}
}

// TestListRetriesAPageThatBreaksMidStream covers a body that stops arriving
// halfway. The page is decoded as it streams, so the retry has to skip what was
// already delivered rather than deliver it twice.
func TestListRetriesAPageThatBreaksMidStream(t *testing.T) {
	server := &bulkServer{total: 25, pageSize: 10, breakAfter: 4}
	uids := collect(t, server.start(t), grafana.ListOptions{})

	if len(uids) != 25 {
		t.Fatalf("listed %d dashboards, want 25", len(uids))
	}
	seen := make(map[string]int, len(uids))
	for _, uid := range uids {
		seen[uid]++
	}
	for uid, count := range seen {
		if count != 1 {
			t.Errorf("dashboard %s was delivered %d times", uid, count)
		}
	}
}

// TestListDoesNotBelieveAnEmptyPageAtOnce covers a busy Grafana answering a
// page in the middle of a listing with nothing at all: an empty page is how a
// listing ends, so taking it at face value would drop the rest of the instance
// without anything looking wrong.
func TestListDoesNotBelieveAnEmptyPageAtOnce(t *testing.T) {
	server := &bulkServer{total: 25, pageSize: 10, emptyAt: 10}
	uids := collect(t, server.start(t), grafana.ListOptions{})

	if len(uids) != 25 {
		t.Fatalf("listed %d dashboards, want all 25 despite the empty page", len(uids))
	}
}

// TestListAcceptsAnEmptyInstance is the other side of that: an instance without
// dashboards answers the very first request with an empty page, and that is the
// end of the listing rather than something to insist on.
func TestListAcceptsAnEmptyInstance(t *testing.T) {
	server := &bulkServer{total: 0, pageSize: 10}
	if uids := collect(t, server.start(t), grafana.ListOptions{}); len(uids) != 0 {
		t.Fatalf("listed %v, want nothing", uids)
	}
	if server.requests != 1 {
		t.Errorf("made %d requests against an empty instance, want 1", server.requests)
	}
}

func TestBulkAvailability(t *testing.T) {
	t.Run("serves dashboards", func(t *testing.T) {
		server := &bulkServer{total: 3, pageSize: 10}
		available, err := server.start(t).BulkAvailable(context.Background())
		if err != nil || !available {
			t.Errorf("BulkAvailable() = %v, %v, want true", available, err)
		}
	})

	// An older Grafana does not know the endpoint at all, which is the one
	// answer that settles the question.
	t.Run("older release", func(t *testing.T) {
		available := probeStatus(t, http.StatusNotFound)
		if available {
			t.Error("a 404 must not be taken for a working bulk API")
		}
	})

	// An instance can answer the probe with nothing because it is empty,
	// because the dashboards are in another namespace, or because the page
	// came back empty for the reason pages do. The endpoint is there in every
	// case, and a listing that yields nothing costs a run the fallback it
	// would otherwise have started with.
	t.Run("empty answer", func(t *testing.T) {
		server := &bulkServer{total: 0, pageSize: 10}
		available, err := server.start(t).BulkAvailable(context.Background())
		if err != nil || !available {
			t.Errorf("BulkAvailable() = %v, %v, want true", available, err)
		}
	})
	t.Run("restricted", func(t *testing.T) {
		if probeStatus(t, http.StatusForbidden) {
			t.Error("a 403 must not be taken for a working bulk API")
		}
	})
}

func probeStatus(t *testing.T, status int) bool {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(status), status)
	}))
	t.Cleanup(server.Close)

	client, err := grafana.New(grafana.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	available, err := client.BulkAvailable(context.Background())
	if err != nil {
		t.Fatalf("BulkAvailable: %v", err)
	}
	return available
}

// TestBulkPathFollowsTheOrganization covers what the collection is addressed
// by. Grafana names the first organization's namespace "default" and the others
// "org-<id>", and a namespace that does not match returns an empty list rather
// than an error, so getting it wrong would look like an instance without
// dashboards. The API version is part of the contract too: only v0alpha1 hands
// back the document as stored, and a later one would quietly return dashboards
// migrated to the current schema, which no longer say which datasource an
// exported ${DS_...} reference means.
func TestBulkPathFollowsTheOrganization(t *testing.T) {
	for _, tc := range []struct {
		orgID int
		want  string
	}{
		{orgID: 0, want: "/apis/dashboard.grafana.app/v0alpha1/namespaces/default/dashboards"},
		{orgID: 1, want: "/apis/dashboard.grafana.app/v0alpha1/namespaces/default/dashboards"},
		{orgID: 7, want: "/apis/dashboard.grafana.app/v0alpha1/namespaces/org-7/dashboards"},
	} {
		var requested string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested = r.URL.Path
			fmt.Fprint(w, `{"items":[]}`)
		}))
		t.Cleanup(server.Close)

		client, err := grafana.New(grafana.Config{BaseURL: server.URL, OrgID: tc.orgID})
		if err != nil {
			t.Fatalf("building client: %v", err)
		}
		if _, err := client.BulkAvailable(context.Background()); err != nil {
			t.Fatalf("BulkAvailable: %v", err)
		}
		if requested != tc.want {
			t.Errorf("org %d asked for %s, want %s", tc.orgID, requested, tc.want)
		}
	}
}
