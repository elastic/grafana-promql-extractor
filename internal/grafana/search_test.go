package grafana_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/elastic/grafana-promql-extractor/internal/grafana"
	"github.com/elastic/grafana-promql-extractor/internal/testsupport"
)

func collectUIDs(t *testing.T, client *grafana.Client, opt grafana.SearchOptions) []string {
	t.Helper()
	var uids []string
	err := client.SearchDashboards(context.Background(), opt, func(hit grafana.DashboardHit) error {
		uids = append(uids, hit.UID)
		return nil
	})
	if err != nil {
		t.Fatalf("SearchDashboards: %v", err)
	}
	return uids
}

func TestSearchPaginatesThroughEveryDashboard(t *testing.T) {
	dashboards := testsupport.GeneratedFixtures(25)
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: dashboards})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	uids := collectUIDs(t, client, grafana.SearchOptions{PageSize: 10})

	if len(uids) != 25 {
		t.Fatalf("got %d dashboards, want 25", len(uids))
	}
	for i, uid := range uids {
		if want := dashboards[i].UID; uid != want {
			t.Errorf("dashboard %d = %q, want %q", i, uid, want)
		}
	}
	// Three full pages plus a short one that ends the iteration.
	if got := fake.Requests("/api/search"); got != 3 {
		t.Errorf("made %d search requests, want 3", got)
	}
}

// TestSearchStopsOnExactPageBoundary covers the case where the dashboard count
// is a multiple of the page size, so an extra empty page is needed to finish.
func TestSearchStopsOnExactPageBoundary(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(20)})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	if uids := collectUIDs(t, client, grafana.SearchOptions{PageSize: 10}); len(uids) != 20 {
		t.Fatalf("got %d dashboards, want 20", len(uids))
	}
	if got := fake.Requests("/api/search"); got != 3 {
		t.Errorf("made %d search requests, want 3", got)
	}
}

func TestSearchRespectsMax(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(25)})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	uids := collectUIDs(t, client, grafana.SearchOptions{PageSize: 10, Max: 12})

	if len(uids) != 12 {
		t.Fatalf("got %d dashboards, want 12", len(uids))
	}
	if got := fake.Requests("/api/search"); got != 2 {
		t.Errorf("made %d search requests, want 2: iteration must stop once Max is reached", got)
	}
}

func TestSearchCapsPageSize(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(1)})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	collectUIDs(t, client, grafana.SearchOptions{PageSize: 99999})

	if got := fake.LastSearchQuery("limit"); got != fmt.Sprint(grafana.MaxPageSize) {
		t.Errorf("limit = %q, want %d", got, grafana.MaxPageSize)
	}
	if got := fake.LastSearchQuery("type"); got != "dash-db" {
		t.Errorf("type = %q, want dash-db", got)
	}
}

func TestSearchStartPage(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(25)})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	uids := collectUIDs(t, client, grafana.SearchOptions{PageSize: 10, StartPage: 3})

	if len(uids) != 5 || uids[0] != "gen-0021" {
		t.Errorf("got %d dashboards starting at %v, want 5 starting at gen-0021", len(uids), uids)
	}
}

// TestSearchFallsBackWhenSortIsRejected covers older Grafana releases that
// reject the sort parameter.
func TestSearchFallsBackWhenSortIsRejected(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards: testsupport.GeneratedFixtures(3),
		RejectSort: true,
	})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	if uids := collectUIDs(t, client, grafana.SearchOptions{PageSize: 10}); len(uids) != 3 {
		t.Fatalf("got %d dashboards, want 3", len(uids))
	}
	if got := fake.LastSearchQuery("sort"); got != "" {
		t.Errorf("sort = %q, want it dropped after the rejection", got)
	}
}

func TestSearchDetectsIgnoredPageParameter(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{
		Dashboards:      testsupport.GeneratedFixtures(30),
		IgnorePageParam: true,
	})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	err := client.SearchDashboards(context.Background(), grafana.SearchOptions{PageSize: 10},
		func(grafana.DashboardHit) error { return nil })

	if !errors.Is(err, grafana.ErrPagingStuck) {
		t.Fatalf("error = %v, want ErrPagingStuck", err)
	}
}

func TestSearchPropagatesYieldError(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(10)})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	sentinel := errors.New("stop here")
	seen := 0
	err := client.SearchDashboards(context.Background(), grafana.SearchOptions{PageSize: 10},
		func(grafana.DashboardHit) error {
			seen++
			if seen == 3 {
				return sentinel
			}
			return nil
		})

	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the sentinel", err)
	}
	if seen != 3 {
		t.Errorf("yielded %d times, want 3", seen)
	}
}

func TestCountDashboards(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(42)})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	count, err := client.CountDashboards(context.Background(), grafana.SearchOptions{PageSize: 10})
	if err != nil {
		t.Fatalf("CountDashboards: %v", err)
	}
	if count != 42 {
		t.Errorf("count = %d, want 42", count)
	}
	// Counting uses the maximum page size regardless of the configured one.
	if got := fake.Requests("/api/search"); got != 1 {
		t.Errorf("made %d search requests, want 1 at the maximum page size", got)
	}
}

// A page number means nothing without a page size, so counting a resumed run
// has to skip exactly the dashboards the run itself will skip.
func TestCountDashboardsMatchesAResumedRun(t *testing.T) {
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: testsupport.GeneratedFixtures(25)})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})
	opt := grafana.SearchOptions{PageSize: 10, StartPage: 3}

	count, err := client.CountDashboards(context.Background(), opt)
	if err != nil {
		t.Fatalf("CountDashboards: %v", err)
	}
	if want := len(collectUIDs(t, client, opt)); count != want {
		t.Errorf("count = %d, want %d, the number of dashboards the run yields", count, want)
	}
}

func TestDashboardJSON(t *testing.T) {
	fixtures, err := testsupport.Fixtures()
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	fake := testsupport.NewFakeGrafana(t, testsupport.FakeOptions{Dashboards: fixtures})
	client := mustClient(t, grafana.Config{BaseURL: fake.URL})

	body, err := client.DashboardJSON(context.Background(), "fx-basic")
	if err != nil {
		t.Fatalf("DashboardJSON: %v", err)
	}
	defer body.Close()

	if _, err := client.DashboardJSON(context.Background(), "no-such-dashboard"); !grafana.IsStatus(err, 404) {
		t.Errorf("expected a 404 for an unknown uid, got %v", err)
	}
}
