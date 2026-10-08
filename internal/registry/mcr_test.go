package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newFakeMCR serves a catalog and tags/list, and counts catalog fetches so the
// cache can be asserted rather than assumed.
func newFakeMCR(t *testing.T, repos []string, tags map[string][]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var catalogHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/_catalog":
			catalogHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
			list, ok := tags[repo]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": list})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &catalogHits
}

// TestMCRSearchIsACatalogFilter covers why MCR became a search source at all.
//
// MCR has no keyword search API, but /v2/_catalog answers anonymously with the
// whole repository list (verified live: 3861 repositories, ~160 KB, and it
// ignores n/last/query/search). So searching is a local filter over that list.
func TestMCRSearchIsACatalogFilter(t *testing.T) {
	srv, hits := newFakeMCR(t, []string{
		"dotnet/runtime",
		"dotnet/sdk",
		"azure-cli",
		"microsoft/dotnet-framework",
		"nginx",
	}, map[string][]string{"dotnet/runtime": {"10.0", "latest", "9.0"}})

	m := NewMCRSearcher(srv.Client(), srv.URL)
	if m.ID() != "mcr" {
		t.Errorf("ID = %q, want mcr", m.ID())
	}

	page, err := m.Search(context.Background(), "dotnet", 1, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("Total = %d, want 3 (dotnet/runtime, dotnet/sdk, microsoft/dotnet-framework)", page.Total)
	}
	if len(page.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(page.Results))
	}
	// Sorted, so paging through results is stable.
	if page.Results[0].Name != "dotnet/runtime" || page.Results[1].Name != "dotnet/sdk" ||
		page.Results[2].Name != "microsoft/dotnet-framework" {
		t.Errorf("results = %v, want them sorted by repository", page.Results)
	}
	// A result must be directly usable as a pull reference.
	if got := page.Results[0].Repository; got != "dotnet/runtime" {
		t.Errorf("Repository = %q, want dotnet/runtime", got)
	}
	if got := page.Results[0].Image; got != "runtime" {
		t.Errorf("Image = %q, want the last path segment", got)
	}
	if page.Results[0].Source != "mcr" {
		t.Errorf("Source = %q, want mcr", page.Results[0].Source)
	}

	// Case-insensitive, and multiple terms must ALL match.
	if p, err := m.Search(context.Background(), "DOTNET", 1, 10); err != nil || p.Total != 3 {
		t.Errorf("uppercase search = %d results (%v), want 3", p.Total, err)
	}
	if p, err := m.Search(context.Background(), "dotnet framework", 1, 10); err != nil || p.Total != 1 {
		t.Errorf("two-term search = %d results (%v), want 1", p.Total, err)
	}

	// Paging is local and stable.
	p2, err := m.Search(context.Background(), "dotnet", 2, 2)
	if err != nil {
		t.Fatalf("Search page 2: %v", err)
	}
	if len(p2.Results) != 1 || p2.Results[0].Name != "microsoft/dotnet-framework" {
		t.Errorf("page 2 = %v, want the third result", p2.Results)
	}
	if p3, _ := m.Search(context.Background(), "dotnet", 9, 10); len(p3.Results) != 0 {
		t.Errorf("page 9 = %v, want empty rather than a panic or重复", p3.Results)
	}

	if hits.Load() != 1 {
		t.Errorf("catalog fetched %d times, want 1 (the cache must serve the rest)", hits.Load())
	}
	if m.CatalogSize() != 5 {
		t.Errorf("CatalogSize = %d, want 5", m.CatalogSize())
	}
}

// TestMCRTagsPutsLatestFirst: the same pagination trap as the registry client —
// MCR's list is lexicographic, so the picker's default must not be "0.10.0".
func TestMCRTagsPutsLatestFirst(t *testing.T) {
	srv, _ := newFakeMCR(t, []string{"azure-cli"}, map[string][]string{
		"azure-cli": {"0.10.0", "0.10.1", "latest", "2.60.0"},
	})
	m := NewMCRSearcher(srv.Client(), srv.URL)

	tags, err := m.Tags(context.Background(), "azure-cli")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 4 {
		t.Fatalf("tags = %v, want 4", tags)
	}
	if tags[0].Name != "latest" {
		t.Errorf("tags[0] = %q, want latest first", tags[0].Name)
	}

	// An unknown repository must be an error, not an empty list.
	if _, err := m.Tags(context.Background(), "nope/none"); err == nil {
		t.Error("Tags of an unknown repository returned no error")
	}
	// A blank repository is a programming error, not a request for everything.
	if _, err := m.Tags(context.Background(), "  "); err == nil {
		t.Error("Tags with a blank repository returned no error")
	}
}

// TestMCRResultsPullFromMCR is the routing half: selecting an MCR search result
// must send the download to mcr.microsoft.com, not to a Docker Hub accelerator
// (MCR's names mean nothing there).
func TestMCRResultsPullFromMCR(t *testing.T) {
	if got := PullHostForSearchSource("mcr"); got != "mcr.microsoft.com" {
		t.Errorf("PullHostForSearchSource(mcr) = %q, want mcr.microsoft.com", got)
	}
	// By NAME too — the seeded row stores the name, and the server maps it back.
	if got := PullHostForSearchSource("MCR (Microsoft)"); got != "mcr.microsoft.com" {
		t.Errorf("PullHostForSearchSource by name = %q, want mcr.microsoft.com", got)
	}
	// MCR is an own-namespace registry: prefixing library/ would break every
	// single-segment name it has ("nginx" is a real MCR repo, and is NOT
	// docker.io's official nginx).
	if NeedsLibraryPrefix("mcr.microsoft.com") {
		t.Error("NeedsLibraryPrefix(mcr.microsoft.com) = true, but MCR has its own namespace")
	}
	ref, err := Parse("dotnet/runtime:10.0", "mcr.microsoft.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ref.Registry != "mcr.microsoft.com" || ref.Repository != "dotnet/runtime" {
		t.Errorf("Parse = %s/%s, want mcr.microsoft.com/dotnet/runtime", ref.Registry, ref.Repository)
	}
}

// TestMCRIsOfferedAsASearchSource keeps the shipped list honest: the source must
// be advertised (so the store seeds it and the CLI resolves "mcr") and must have
// a searcher behind it.
func TestMCRIsOfferedAsASearchSource(t *testing.T) {
	var found bool
	for _, s := range BuiltinSearchSources {
		if s.ID == "mcr" {
			found = true
			if !strings.Contains(s.URL, "mcr.microsoft.com") {
				t.Errorf("mcr source URL = %q", s.URL)
			}
		}
	}
	if !found {
		t.Fatal("mcr is not among BuiltinSearchSources, so no install would offer it")
	}
	searchers := SearchSources(nil)
	searcher, ok := searchers["mcr"]
	if !ok {
		t.Fatal("SearchSources has no searcher for mcr")
	}
	if searcher.Name() == "" {
		t.Error("the mcr searcher has no display name")
	}
}
