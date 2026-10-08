package registry

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// MCRSearcher searches the Microsoft Container Registry.
//
// MCR publishes no keyword search API — its website's own endpoint
// (/api/v1/catalog/all) answers 400 to a bare request — but it DOES serve its
// full repository catalog anonymously:
//
//	GET https://mcr.microsoft.com/v2/_catalog
//	-> {"repositories":[...]}            VERIFIED: 3861 repositories, ~160 KB
//
// The endpoint ignores n/last/query/search (all four were probed and returned
// the identical full list), so paging and matching happen locally. That is a
// real search, just a client-side one, and 160 KB is cheap enough to fetch once
// and reuse — hence the TTL cache below.
//
// This is the only upstream registry besides Docker Hub and Quay that can be
// searched at all: ghcr.io, registry.k8s.io, public.ecr.aws, nvcr.io, gcr.io
// and cgr.dev expose no anonymous search or catalog of any kind (`/v2/_catalog`
// answers 401 or 404), which is why those are reached by typing a full
// reference instead.
type MCRSearcher struct {
	client  *http.Client
	baseURL string

	// catalogCache holds the fetched repository list; MCR's catalog changes
	// slowly and re-fetching 160 KB on every keystroke would be rude.
	mu          sync.Mutex
	catalog     []string
	catalogAt   time.Time
	catalogSize int
}

// mcrCatalogTTL is how long a fetched catalog is reused.
const mcrCatalogTTL = 15 * time.Minute

// NewMCRSearcher builds an MCR searcher. baseURL is normally
// "https://mcr.microsoft.com". A nil client uses the package default.
func NewMCRSearcher(client *http.Client, baseURL string) *MCRSearcher {
	if client == nil {
		client = searchHTTPClient()
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://mcr.microsoft.com"
	}
	return &MCRSearcher{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// ID implements Searcher.
func (m *MCRSearcher) ID() string { return "mcr" }

// Name implements Searcher.
func (m *MCRSearcher) Name() string { return "MCR (Microsoft)" }

// catalogResponse is the shape of /v2/_catalog.
type catalogResponse struct {
	Repositories []string `json:"repositories"`
}

// fetchCatalog returns the repository list, using the cache when it is fresh.
//
// A failed refresh with a usable (stale) cache is not an error: the catalog
// barely changes, and answering a search from slightly old data beats failing
// because one request timed out.
func (m *MCRSearcher) fetchCatalog(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	if len(m.catalog) > 0 && time.Since(m.catalogAt) < mcrCatalogTTL {
		out := m.catalog
		m.mu.Unlock()
		return out, nil
	}
	stale := m.catalog
	m.mu.Unlock()

	var body catalogResponse
	if err := getJSON(ctx, m.client, m.baseURL+"/v2/_catalog", &body); err != nil {
		if len(stale) > 0 {
			return stale, nil
		}
		return nil, fmt.Errorf("读取 MCR 目录失败: %w", err)
	}

	repos := make([]string, 0, len(body.Repositories))
	for _, r := range body.Repositories {
		if r = strings.Trim(strings.TrimSpace(r), "/"); r != "" {
			repos = append(repos, r)
		}
	}
	// Sorted so paging is STABLE: an unsorted catalog would shuffle results
	// between pages as the caller clicks through them.
	sort.Strings(repos)

	m.mu.Lock()
	m.catalog = repos
	m.catalogAt = time.Now()
	m.catalogSize = len(repos)
	m.mu.Unlock()
	return repos, nil
}

// CatalogSize reports how many repositories the last fetch returned (0 before
// the first search).
func (m *MCRSearcher) CatalogSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.catalogSize
}

// Search implements Searcher by filtering the catalog.
//
// The keyword matches the repository path, case-insensitively. Multiple words
// must ALL match (in any order), which is what a user expects from "dotnet
// runtime" without needing exact paths.
func (m *MCRSearcher) Search(ctx context.Context, keyword string, page, pageSize int) (SearchPage, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return SearchPage{}, fmt.Errorf("关键词不能为空")
	}
	if page <= 0 {
		page = 1
	}
	pageSize = clampPageSize(pageSize)

	repos, err := m.fetchCatalog(ctx)
	if err != nil {
		return SearchPage{}, err
	}

	terms := strings.Fields(strings.ToLower(keyword))
	matches := make([]string, 0, 32)
	for _, repo := range repos {
		lower := strings.ToLower(repo)
		all := true
		for _, t := range terms {
			if !strings.Contains(lower, t) {
				all = false
				break
			}
		}
		if all {
			matches = append(matches, repo)
		}
	}

	out := SearchPage{
		Results:  make([]SearchResult, 0, pageSize),
		Total:    len(matches),
		Page:     page,
		PageSize: pageSize,
	}
	start := (page - 1) * pageSize
	if start >= len(matches) {
		return out, nil
	}
	end := start + pageSize
	if end > len(matches) {
		end = len(matches)
	}
	for _, repo := range matches[start:end] {
		out.Results = append(out.Results, SearchResult{
			Source:     m.ID(),
			Name:       repo,
			Repository: repo,
			Image:      pathBase(repo),
			// MCR's catalog carries names only: no description, stars or pull
			// counts exist to report, and inventing zeros as if they were
			// measurements would be worse than leaving them empty.
			Official: strings.HasPrefix(repo, "microsoft/"),
		})
	}
	return out, nil
}

// Tags implements Searcher against MCR's own tags/list, which is anonymous for
// public repositories (verified: dotnet/runtime reports 8315 tags).
func (m *MCRSearcher) Tags(ctx context.Context, repository string) ([]TagInfo, error) {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" {
		return nil, fmt.Errorf("仓库名不能为空")
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	endpoint := m.baseURL + "/v2/" + repository + "/tags/list?n=200"
	if err := getJSON(ctx, m.client, endpoint, &body); err != nil {
		return nil, err
	}
	// `latest` first, for the same reason the registry client does it: the list
	// is lexicographic and paginated, so the picker's default would otherwise be
	// an arbitrary early tag.
	out := make([]TagInfo, 0, len(body.Tags))
	hasLatest := false
	for _, t := range body.Tags {
		if t == "latest" {
			hasLatest = true
			continue
		}
		out = append(out, TagInfo{Name: t})
	}
	if hasLatest {
		out = append([]TagInfo{{Name: "latest"}}, out...)
	}
	return out, nil
}

// pathBase returns the last path segment, the part that is usually the image
// name ("dotnet/runtime" -> "runtime").
func pathBase(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 && i+1 < len(repo) {
		return repo[i+1:]
	}
	return repo
}
