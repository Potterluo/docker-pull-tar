package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// recordedRequest is a snapshot of one request the fake search endpoint saw,
// stored by value so the race detector does not see the handler goroutine
// sharing state with the test goroutine.
type recordedRequest struct {
	Path  string
	Query url.Values
}

// jsonServer starts a test server that always answers with body and reports
// every request it saw through the returned function.
func jsonServer(t *testing.T, body string) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []recordedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, recordedRequest{Path: r.URL.Path, Query: r.URL.Query()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), seen...)
	}
}

// TestDockerHubSearchNormalisation protects the search-result shape the UI
// renders: official images get the library/ prefix, user images keep their
// namespace, dates are truncated to a day, and newlines in a description
// cannot break the table row.
func TestDockerHubSearchNormalisation(t *testing.T) {
	body := `{"count":3,"results":[
		{"repo_name":"library/nginx","short_description":"Official image\nfor web serving","star_count":10,"pull_count":20,"last_updated":"2024-01-02T03:04:05.999Z"},
		{"repo_name":"user/app","short_description":"  padded  ","star_count":3,"pull_count":4,"last_updated":"2023-12-31"},
		{"repo_name":"redis","short_description":"","star_count":1,"pull_count":2,"last_updated":"2024-02-03T04:05:06Z"}
	]}`
	srv, requests := jsonServer(t, body)
	searcher := NewDockerHubSearcher(srv.Client(), srv.URL)

	page, err := searcher.Search(context.Background(), "web", 1, 25)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if page.Total != 3 {
		t.Errorf("Total = %d, want 3", page.Total)
	}
	if page.Page != 1 || page.PageSize != 25 {
		t.Errorf("page metadata = %d/%d, want 1/25", page.Page, page.PageSize)
	}
	if len(page.Results) != 3 {
		t.Fatalf("len(Results) = %d, want 3", len(page.Results))
	}

	first := page.Results[0]
	if first.Name != "library/nginx" || first.Repository != "library/nginx" {
		t.Errorf("official row = %+v, want the library/ namespace", first)
	}
	if !first.Official {
		t.Errorf("official row %+v is not flagged Official", first)
	}
	if first.Image != "nginx" {
		t.Errorf("Image = %q, want nginx (the last path segment)", first.Image)
	}
	if strings.Contains(first.Description, "\n") {
		t.Errorf("Description = %q, want newlines collapsed", first.Description)
	}
	if first.Updated != "2024-01-02" {
		t.Errorf("Updated = %q, want YYYY-MM-DD", first.Updated)
	}
	if first.Source != "dockerhub" || first.Stars != 10 || first.Pulls != 20 {
		t.Errorf("row fields = %+v", first)
	}

	second := page.Results[1]
	if second.Name != "user/app" || second.Official {
		t.Errorf("user row = %+v, want user/app and Official=false", second)
	}
	if second.Description != "padded" {
		t.Errorf("Description = %q, want the surrounding whitespace trimmed", second.Description)
	}
	if second.Updated != "2023-12-31" {
		t.Errorf("Updated = %q, want the short date passed through", second.Updated)
	}

	// A bare name from Docker Hub means an official image.
	third := page.Results[2]
	if third.Name != "library/redis" || !third.Official {
		t.Errorf("bare repo_name = %+v, want library/redis and Official=true", third)
	}

	// The outgoing query must carry the keyword and the paging.
	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if !strings.HasPrefix(reqs[0].Path, "/v2/search/repositories/") {
		t.Errorf("path = %q, want the Docker Hub search endpoint", reqs[0].Path)
	}
	q := reqs[0].Query
	if q.Get("query") != "web" || q.Get("page") != "1" || q.Get("page_size") != "25" {
		t.Errorf("query = %v", q)
	}
}

// TestDockerHubSearchPageDefaults protects the paging bounds: page 0 means
// page 1 and an oversized page_size is clamped to the API's maximum.
func TestDockerHubSearchPageDefaults(t *testing.T) {
	srv, requests := jsonServer(t, `{"count":0,"results":[]}`)
	searcher := NewDockerHubSearcher(srv.Client(), srv.URL)

	page, err := searcher.Search(context.Background(), "nginx", 0, 5000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if page.Page != 1 {
		t.Errorf("Page = %d, want 1", page.Page)
	}
	if page.PageSize != 100 {
		t.Errorf("PageSize = %d, want the clamp to 100", page.PageSize)
	}
	q := requests()[0].Query
	if q.Get("page") != "1" || q.Get("page_size") != "100" {
		t.Errorf("query = %v, want the clamped values on the wire", q)
	}
}

// TestDockerHubTags protects the tag list the arch picker feeds on:
// platforms deduped, size the largest image, date truncated.
func TestDockerHubTags(t *testing.T) {
	body := `{"count":2,"results":[
		{"name":"1.26","last_updated":"2024-05-06T07:08:09Z","images":[
			{"architecture":"amd64","os":"linux","size":1000},
			{"architecture":"amd64","os":"linux","size":1000},
			{"architecture":"arm","os":"linux","variant":"v7","size":3000},
			{"architecture":"arm","variant":"v7","size":2500}
		]},
		{"name":"latest","last_updated":"2024-06-07T08:09:10Z","images":[
			{"architecture":"amd64","os":"linux","size":4000}
		]}
	]}`
	srv, requests := jsonServer(t, body)
	searcher := NewDockerHubSearcher(srv.Client(), srv.URL)

	tags, err := searcher.Tags(context.Background(), "library/nginx")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("Tags = %+v, want 2", tags)
	}
	if tags[0].Name != "1.26" || tags[1].Name != "latest" {
		t.Errorf("Tags order = %q, %q; want the response order preserved", tags[0].Name, tags[1].Name)
	}
	if got := tags[0].Platforms; len(got) != 2 ||
		got[0].String() != "linux/amd64" || got[1].String() != "linux/arm/v7" {
		t.Errorf("Platforms = %+v, want the deduped linux pair in order", got)
	}
	if tags[0].Size != 3000 {
		t.Errorf("Size = %d, want the largest image size 3000", tags[0].Size)
	}
	if tags[0].Updated != "2024-05-06" {
		t.Errorf("Updated = %q, want YYYY-MM-DD", tags[0].Updated)
	}
	// An image without an explicit os is a linux image.
	if got := tags[0].Platforms[1]; got.OS != "linux" {
		t.Errorf("missing os = %+v, want linux", got)
	}

	if path := requests()[0].Path; path != "/v2/repositories/library/nginx/tags/" {
		t.Errorf("path = %q", path)
	}
}

// TestSearchErrorsAreNotSwallowed protects the failure modes: an empty
// keyword and a non-200 response must both be reported.
func TestSearchErrorsAreNotSwallowed(t *testing.T) {
	ctx := context.Background()

	t.Run("empty keyword", func(t *testing.T) {
		searcher := NewDockerHubSearcher(http.DefaultClient, "http://127.0.0.1:0")
		if _, err := searcher.Search(ctx, "  ", 1, 10); err == nil {
			t.Fatal("Search with an empty keyword succeeded")
		}
		if _, err := searcher.Tags(ctx, "  "); err == nil {
			t.Fatal("Tags with an empty repository succeeded")
		}
	})

	t.Run("HTTP error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		searcher := NewDockerHubSearcher(srv.Client(), srv.URL)
		_, err := searcher.Search(ctx, "nginx", 1, 10)
		if err == nil {
			t.Fatal("Search succeeded on a 500")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want the status surfaced", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		srv, _ := jsonServer(t, "{not json")
		searcher := NewDockerHubSearcher(srv.Client(), srv.URL)
		if _, err := searcher.Search(ctx, "nginx", 1, 10); err == nil {
			t.Fatal("Search accepted a malformed body")
		}
	})
}

// TestDockerHubSearcherDefaults protects the base-URL handling: an empty
// override means Docker Hub, and a trailing slash never doubles up.
func TestDockerHubSearcherDefaults(t *testing.T) {
	d := NewDockerHubSearcher(http.DefaultClient, "")
	if d.baseURL != "https://hub.docker.com" {
		t.Errorf("baseURL = %q, want the Docker Hub default", d.baseURL)
	}
	if d.ID() != "dockerhub" || d.Name() == "" {
		t.Errorf("ID/Name = %q/%q", d.ID(), d.Name())
	}

	d = NewDockerHubSearcher(http.DefaultClient, "http://example.com/")
	if d.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q, want the trailing slash trimmed", d.baseURL)
	}

	m := NewOneMSSearcher(http.DefaultClient, "")
	if m.baseURL != "https://1ms.run/api/v1/registry" {
		t.Errorf("1ms baseURL = %q", m.baseURL)
	}
	if m.ID() != "1ms" || m.Name() == "" {
		t.Errorf("ID/Name = %q/%q", m.ID(), m.Name())
	}
}

// TestOneMSSearch protects the 1ms shaper, including the two response
// shapes the API is known to return (wrapped and bare).
func TestOneMSSearch(t *testing.T) {
	ctx := context.Background()

	t.Run("wrapped envelope", func(t *testing.T) {
		body := `{"code":0,"data":{"total":1,"list":[
			{"namespace":"library","name":"redis","description":"key\nvalue store",
			 "star_count":5,"pull_count":6,"last_modified":"2024-02-03T04:05:06Z"}
		]}}`
		srv, requests := jsonServer(t, body)
		searcher := NewOneMSSearcher(srv.Client(), srv.URL)

		page, err := searcher.Search(ctx, "redis", 2, 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if page.Total != 1 || page.Page != 2 || page.PageSize != 10 {
			t.Errorf("page metadata = %+v", page)
		}
		if len(page.Results) != 1 {
			t.Fatalf("Results = %+v, want 1", page.Results)
		}
		row := page.Results[0]
		if row.Name != "library/redis" || row.Repository != "library/redis" || row.Image != "redis" {
			t.Errorf("row = %+v, want library/redis", row)
		}
		if !row.Official || row.Source != "1ms" {
			t.Errorf("row = %+v, want Official and Source=1ms", row)
		}
		if row.Updated != "2024-02-03" {
			t.Errorf("Updated = %q", row.Updated)
		}
		if strings.Contains(row.Description, "\n") {
			t.Errorf("Description = %q, want newlines collapsed", row.Description)
		}
		if p := requests()[0].Path; p != "/search" {
			t.Errorf("path = %q, want /search", p)
		}
	})

	t.Run("unwrapped payload", func(t *testing.T) {
		body := `{"total":1,"list":[
			{"name":"redis","description":"bare shape","last_pushed":"2024-03-04T05:06:07Z"}
		]}`
		srv, _ := jsonServer(t, body)
		searcher := NewOneMSSearcher(srv.Client(), srv.URL)

		page, err := searcher.Search(ctx, "redis", 1, 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(page.Results) != 1 {
			t.Fatalf("Results = %+v, want 1", page.Results)
		}
		row := page.Results[0]
		// A missing namespace means the official library.
		if row.Name != "library/redis" || !row.Official {
			t.Errorf("row = %+v, want the library namespace defaulted", row)
		}
		// last_pushed stands in when last_modified is absent.
		if row.Updated != "2024-03-04" {
			t.Errorf("Updated = %q, want last_pushed to be used", row.Updated)
		}
	})

	t.Run("non-zero code is an error", func(t *testing.T) {
		srv, _ := jsonServer(t, `{"code":500,"message":"internal error"}`)
		searcher := NewOneMSSearcher(srv.Client(), srv.URL)
		if _, err := searcher.Search(ctx, "redis", 1, 10); err == nil {
			t.Fatal("Search accepted code=500")
		} else if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want the failing code surfaced", err)
		}
	})

	t.Run("empty keyword", func(t *testing.T) {
		searcher := NewOneMSSearcher(http.DefaultClient, "http://127.0.0.1:0")
		if _, err := searcher.Search(ctx, " ", 1, 10); err == nil {
			t.Fatal("Search with an empty keyword succeeded")
		}
	})

	t.Run("rows without a name are dropped", func(t *testing.T) {
		srv, _ := jsonServer(t, `{"code":0,"data":{"total":2,"list":[{"namespace":"library"},{"namespace":"library","name":"ok"}]}}`)
		searcher := NewOneMSSearcher(srv.Client(), srv.URL)
		page, err := searcher.Search(ctx, "ok", 1, 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(page.Results) != 1 || page.Results[0].Name != "library/ok" {
			t.Errorf("Results = %+v, want only the named row", page.Results)
		}
	})
}

// TestOneMSTags protects the 1ms tag list: platforms deduped, the newest
// tag first, and "latest" pinned to the top where users look for it.
func TestOneMSTags(t *testing.T) {
	body := `{"code":0,"data":{"list":[
		{"tag_name":"v1.0","images":[{"os":"linux","architecture":"amd64","size":100}]},
		{"tag_name":"latest","images":[{"os":"linux","architecture":"amd64","size":200},{"os":"linux","architecture":"amd64","size":200}]},
		{"tag_name":"v2.0","images":[{"os":"linux","architecture":"arm64","size":300}]}
	]}}`
	srv, requests := jsonServer(t, body)
	searcher := NewOneMSSearcher(srv.Client(), srv.URL)

	tags, err := searcher.Tags(context.Background(), "library/redis")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("Tags = %+v, want 3", tags)
	}
	if tags[0].Name != "latest" {
		t.Errorf("Tags[0] = %q, want latest first", tags[0].Name)
	}
	if tags[1].Name != "v2.0" || tags[2].Name != "v1.0" {
		t.Errorf("Tags order = %q, %q; want newest-first", tags[1].Name, tags[2].Name)
	}
	if got := tags[0].Platforms; len(got) != 1 {
		t.Errorf("Platforms = %+v, want the duplicate removed", got)
	}
	if tags[0].Size != 200 {
		t.Errorf("Size = %d, want 200", tags[0].Size)
	}

	q := requests()[0].Query
	if q.Get("repositories") != "library/redis" || q.Get("page_size") != "100" {
		t.Errorf("query = %v", q)
	}
}

// TestPickSearcher protects source selection: empty means the default, an
// unknown id is a hard error that lists the alternatives (a typo must never
// silently search somewhere else).
func TestPickSearcher(t *testing.T) {
	sources := SearchSources(nil)
	// Every DECLARED source must have an implementation, and every
	// implementation a declaration. That is the invariant worth pinning —
	// a hard-coded count just breaks whenever a source is added.
	if len(sources) != len(BuiltinSearchSources) {
		t.Fatalf("SearchSources = %v, want one per BuiltinSearchSources (%d)", sources, len(BuiltinSearchSources))
	}
	for _, s := range BuiltinSearchSources {
		if _, ok := sources[s.ID]; !ok {
			t.Errorf("SearchSources is missing %q", s.ID)
		}
	}
	for id := range sources {
		var declared bool
		for _, s := range BuiltinSearchSources {
			if s.ID == id {
				declared = true
			}
		}
		if !declared {
			t.Errorf("SearchSources has an undeclared source %q", id)
		}
	}

	tests := []struct {
		id     string
		wantID string
	}{
		{"", "dockerhub"},
		{"dockerhub", "dockerhub"},
		{"1ms", "1ms"},
		{"1MS", "1ms"},
		{"  1ms  ", "1ms"},
		{"quay", "quay"},
		{"QUAY", "quay"},
	}
	for _, tc := range tests {
		got, err := PickSearcher(sources, tc.id)
		if err != nil {
			t.Fatalf("PickSearcher(%q): %v", tc.id, err)
		}
		if got.ID() != tc.wantID {
			t.Errorf("PickSearcher(%q).ID() = %q, want %q", tc.id, got.ID(), tc.wantID)
		}
	}

	if _, err := PickSearcher(sources, "nope"); err == nil {
		t.Fatal("PickSearcher accepted an unknown source id")
	} else {
		for _, want := range []string{"nope", "1ms", "dockerhub"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to mention %q", err, want)
			}
		}
	}

	// The valid ids in the error are sorted, so the message is stable.
	custom := map[string]Searcher{
		"zeta":  NewOneMSSearcher(http.DefaultClient, "http://x"),
		"alpha": NewDockerHubSearcher(http.DefaultClient, "http://x"),
	}
	if _, err := PickSearcher(custom, "beta"); err == nil || !strings.Contains(err.Error(), "alpha, zeta") {
		t.Errorf("error = %v, want a sorted id list", err)
	}
}

// TestClampPageSize protects the paging bounds shared by both searchers.
func TestClampPageSize(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{0, 20},
		{-1, 20},
		{-100, 20},
		{1, 1},
		{20, 20},
		{37, 37},
		{100, 100},
		{101, 100},
		{500, 100},
	}
	for _, tc := range tests {
		if got := clampPageSize(tc.in); got != tc.want {
			t.Errorf("clampPageSize(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestBuiltinSearchSources protects the shipped source list the store seeds.
func TestBuiltinSearchSources(t *testing.T) {
	if len(BuiltinSearchSources) < 2 {
		t.Fatalf("BuiltinSearchSources = %+v, want the shipped sources", BuiltinSearchSources)
	}
	// Ids must be unique: PickSearcher keys off them, and a duplicate would
	// silently shadow one backend with another.
	seen := map[string]bool{}
	for _, s := range BuiltinSearchSources {
		if seen[s.ID] {
			t.Errorf("duplicate built-in source id %q", s.ID)
		}
		seen[s.ID] = true
	}
	if BuiltinSearchSources[0].ID != DefaultSearchSourceID {
		t.Errorf("first built-in = %q, want the default %q", BuiltinSearchSources[0].ID, DefaultSearchSourceID)
	}
	for _, s := range BuiltinSearchSources {
		if s.ID == "" || s.Name == "" || !strings.HasPrefix(s.URL, "https://") {
			t.Errorf("incomplete built-in source %+v", s)
		}
	}
}
