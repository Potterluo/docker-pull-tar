package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SearchResult is one image row in a search result table.
type SearchResult struct {
	Source      string `json:"source"`
	Name        string `json:"name"`       // "library/nginx"
	Repository  string `json:"repository"` // same as Name; explicit for clients
	Image       string `json:"image"`      // "nginx"
	Description string `json:"description"`
	Stars       int64  `json:"stars"`
	Pulls       int64  `json:"pulls"`
	Updated     string `json:"updated"` // YYYY-MM-DD
	Official    bool   `json:"official"`
}

// SearchPage is one page of search results.
type SearchPage struct {
	Results  []SearchResult `json:"results"`
	Total    int            `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"pageSize"`
}

// TagInfo is one tag of a repository.
type TagInfo struct {
	Name      string     `json:"name"`
	Platforms []Platform `json:"platforms,omitempty"`
	Size      int64      `json:"size,omitempty"`
	Updated   string     `json:"updated,omitempty"`
}

// Searcher is one search backend.
type Searcher interface {
	// ID is the stable source identifier ("dockerhub", "1ms").
	ID() string
	// Name is the human label shown in the UI.
	Name() string
	// Search finds images by keyword. page is 1-based.
	Search(ctx context.Context, keyword string, page, pageSize int) (SearchPage, error)
	// Tags lists a repository's tags. repository is "library/nginx".
	Tags(ctx context.Context, repository string) ([]TagInfo, error)
}

// BuiltinSearchSources describes the shipped search backends. The store
// seeds these so the CLI and GUI share one list.
var BuiltinSearchSources = []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}{
	{ID: "dockerhub", Name: "Docker Hub 官方", URL: "https://hub.docker.com"},
	{ID: "quay", Name: "Quay.io (Red Hat)", URL: "https://quay.io"},
	{ID: "1ms", Name: "1ms 加速源", URL: "https://1ms.run/api/v1/registry"},
	// MCR has no keyword API but does serve its full catalog anonymously, so it
	// is a searchable source (client-side filtering; see MCRSearcher).
	{ID: "mcr", Name: "MCR (Microsoft)", URL: "https://mcr.microsoft.com"},
}

// DefaultSearchSourceID is the backend used when none is selected.
const DefaultSearchSourceID = "dockerhub"

// searchHTTPClient is shared by every searcher. Searches are small JSON
// calls, so one short timeout and no per-task transport is enough.
func searchHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     60 * time.Second,
		},
	}
}

// SearchSources builds the built-in searcher registry keyed by source id.
func SearchSources(client *http.Client) map[string]Searcher {
	if client == nil {
		client = searchHTTPClient()
	}
	out := make(map[string]Searcher, len(BuiltinSearchSources))
	for _, s := range BuiltinSearchSources {
		switch s.ID {
		case "dockerhub":
			out[s.ID] = NewDockerHubSearcher(client, s.URL)
		case "quay":
			out[s.ID] = NewQuaySearcher(client, s.URL)
		case "1ms":
			out[s.ID] = NewOneMSSearcher(client, s.URL)
		case "mcr":
			out[s.ID] = NewMCRSearcher(client, s.URL)
		}
	}
	return out
}

// PickSearcher resolves a source id, falling back to the default when the
// id is empty. An unknown id is an error, not a silent fallback — a typo
// should not quietly search somewhere else.
func PickSearcher(sources map[string]Searcher, id string) (Searcher, error) {
	id = strings.TrimSpace(strings.ToLower(id))
	if id == "" {
		id = DefaultSearchSourceID
	}
	if s, ok := sources[id]; ok {
		return s, nil
	}
	ids := make([]string, 0, len(sources))
	for k := range sources {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return nil, fmt.Errorf("未知的搜索源 %q，可用: %s", id, strings.Join(ids, ", "))
}

// clampPageSize bounds a caller-supplied page size.
func clampPageSize(n int) int {
	if n <= 0 {
		return 20
	}
	if n > 100 {
		return 100
	}
	return n
}

func dateOnly(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// getJSON performs a GET and decodes the body into dst.
func getJSON(ctx context.Context, client *http.Client, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("读取 %s 响应失败: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s 返回 %d: %s", rawURL, resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("解析 %s 响应失败: %w", rawURL, err)
	}
	return nil
}

// --- Docker Hub ---------------------------------------------------------

// DockerHubSearcher searches Docker Hub and lists its tags.
type DockerHubSearcher struct {
	client  *http.Client
	baseURL string
}

// NewDockerHubSearcher builds a Docker Hub searcher. baseURL is normally
// "https://hub.docker.com". A nil client uses the package default.
func NewDockerHubSearcher(client *http.Client, baseURL string) *DockerHubSearcher {
	if client == nil {
		client = searchHTTPClient()
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://hub.docker.com"
	}
	return &DockerHubSearcher{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// ID implements Searcher.
func (d *DockerHubSearcher) ID() string { return "dockerhub" }

// Name implements Searcher.
func (d *DockerHubSearcher) Name() string { return "Docker Hub 官方" }

type dockerHubSearchResponse struct {
	Count   int `json:"count"`
	Results []struct {
		RepoName         string `json:"repo_name"`
		ShortDescription string `json:"short_description"`
		StarCount        int64  `json:"star_count"`
		PullCount        int64  `json:"pull_count"`
		LastUpdated      string `json:"last_updated"`
	} `json:"results"`
}

// Search implements Searcher.
func (d *DockerHubSearcher) Search(ctx context.Context, keyword string, page, pageSize int) (SearchPage, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return SearchPage{}, fmt.Errorf("关键词不能为空")
	}
	pageSize = clampPageSize(pageSize)
	if page <= 0 {
		page = 1
	}

	q := url.Values{}
	q.Set("query", keyword)
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))

	var body dockerHubSearchResponse
	if err := getJSON(ctx, d.client, d.baseURL+"/v2/search/repositories/?"+q.Encode(), &body); err != nil {
		return SearchPage{}, err
	}

	out := SearchPage{
		Results:  make([]SearchResult, 0, len(body.Results)),
		Total:    body.Count,
		Page:     page,
		PageSize: pageSize,
	}
	for _, r := range body.Results {
		name := strings.TrimSpace(r.RepoName)
		if name == "" {
			continue
		}
		// Docker Hub omits the "library/" namespace for official images in
		// some response shapes; normalise so the repository is always
		// directly usable.
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}
		out.Results = append(out.Results, SearchResult{
			Source:      d.ID(),
			Name:        name,
			Repository:  name,
			Image:       name[strings.LastIndex(name, "/")+1:],
			Description: strings.ReplaceAll(strings.TrimSpace(r.ShortDescription), "\n", " "),
			Stars:       r.StarCount,
			Pulls:       r.PullCount,
			Updated:     dateOnly(r.LastUpdated),
			Official:    strings.HasPrefix(name, "library/"),
		})
	}
	return out, nil
}

type dockerHubTagsResponse struct {
	Count   int `json:"count"`
	Results []struct {
		Name        string `json:"name"`
		LastUpdated string `json:"last_updated"`
		Images      []struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
			Size         int64  `json:"size"`
		} `json:"images"`
	} `json:"results"`
}

// Tags implements Searcher.
func (d *DockerHubSearcher) Tags(ctx context.Context, repository string) ([]TagInfo, error) {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" {
		return nil, fmt.Errorf("镜像名称不能为空")
	}
	q := url.Values{}
	q.Set("page_size", "100")
	q.Set("ordering", "last_updated")

	var body dockerHubTagsResponse
	if err := getJSON(ctx, d.client, d.baseURL+"/v2/repositories/"+repository+"/tags/?"+q.Encode(), &body); err != nil {
		return nil, err
	}

	out := make([]TagInfo, 0, len(body.Results))
	for _, r := range body.Results {
		if r.Name == "" {
			continue
		}
		ti := TagInfo{Name: r.Name, Updated: dateOnly(r.LastUpdated)}
		for _, img := range r.Images {
			osName := img.OS
			if osName == "" {
				osName = "linux"
			}
			ti.Platforms = append(ti.Platforms, Platform{
				OS:           osName,
				Architecture: img.Architecture,
				Variant:      img.Variant,
			})
			if img.Size > ti.Size {
				ti.Size = img.Size
			}
		}
		ti.Platforms = DedupePlatforms(ti.Platforms)
		out = append(out, ti)
	}
	return out, nil
}

// --- Quay.io ------------------------------------------------------------

// QuaySearcher searches quay.io.
//
// Quay is the one non-Docker-Hub registry with a public search API
// (`/api/v1/find/repositories`). GHCR deliberately has none, so its images are
// reached by owner/repo plus a tag listing rather than by keyword.
type QuaySearcher struct {
	client  *http.Client
	baseURL string
}

// NewQuaySearcher builds a Quay searcher. baseURL is normally
// "https://quay.io". A nil client uses the package default.
func NewQuaySearcher(client *http.Client, baseURL string) *QuaySearcher {
	if client == nil {
		client = searchHTTPClient()
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://quay.io"
	}
	return &QuaySearcher{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// ID implements Searcher.
func (q *QuaySearcher) ID() string { return "quay" }

// Name implements Searcher.
func (q *QuaySearcher) Name() string { return "Quay.io (Red Hat)" }

// quaySearchResponse is the shape of /api/v1/find/repositories:
//
//	{"results":[{"name":"prometheus","namespace":{"name":"prometheus"},
//	             "description":"...","is_public":true, ...}],
//	 "has_additional":true,"page":1,"page_size":10}
//
// Note it ignores the `limit` parameter and always returns page_size items.
type quaySearchResponse struct {
	Results []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		IsPublic    bool   `json:"is_public"`
		Namespace   struct {
			Name string `json:"name"`
		} `json:"namespace"`
	} `json:"results"`
	HasAdditional bool `json:"has_additional"`
	Page          int  `json:"page"`
	PageSize      int  `json:"page_size"`
}

// Search implements Searcher.
func (q *QuaySearcher) Search(ctx context.Context, keyword string, page, pageSize int) (SearchPage, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return SearchPage{}, fmt.Errorf("关键词不能为空")
	}
	if page <= 0 {
		page = 1
	}
	pageSize = clampPageSize(pageSize)

	values := url.Values{}
	values.Set("query", keyword)
	values.Set("page", strconv.Itoa(page))

	var body quaySearchResponse
	if err := getJSON(ctx, q.client, q.baseURL+"/api/v1/find/repositories?"+values.Encode(), &body); err != nil {
		return SearchPage{}, err
	}

	out := SearchPage{
		Results:  make([]SearchResult, 0, len(body.Results)),
		Page:     page,
		PageSize: pageSize,
	}
	for _, r := range body.Results {
		ns := strings.Trim(strings.TrimSpace(r.Namespace.Name), "/")
		name := strings.Trim(strings.TrimSpace(r.Name), "/")
		if name == "" || ns == "" {
			// A result without a namespace cannot be addressed as a pull
			// reference, so skip it rather than emit something unusable.
			continue
		}
		repo := ns + "/" + name
		out.Results = append(out.Results, SearchResult{
			Source:      q.ID(),
			Name:        repo,
			Repository:  repo,
			Image:       name,
			Description: strings.ReplaceAll(strings.TrimSpace(r.Description), "\n", " "),
			Official:    ns == "library",
		})
	}
	// Quay reports a boolean, not a count. When it says there is more, the
	// total is at least one more page — enough for the UI's pager to enable
	// "next" without pretending to know the exact number.
	if body.HasAdditional {
		out.Total = (page + 1) * pageSize
	} else {
		out.Total = (page-1)*pageSize + len(out.Results)
	}
	return out, nil
}

// quayTagsResponse is the shape of /api/v2/repository/<ns>/<repo>/tag/:
//
//	{"tags":[{"name":"latest","manifest_digest":"sha256:…","size":123,
//	          "last_modified":"2024-01-01T00:00:00Z","is_manifest_list":true}],
//	 "page":1,"has_additional":false}
type quayTagsResponse struct {
	Tags []struct {
		Name           string `json:"name"`
		Size           int64  `json:"size"`
		LastModified   string `json:"last_modified"`
		IsManifestList bool   `json:"is_manifest_list"`
	} `json:"tags"`
}

// Tags implements Searcher. repository is "prometheus/prometheus".
func (q *QuaySearcher) Tags(ctx context.Context, repository string) ([]TagInfo, error) {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" {
		return nil, fmt.Errorf("镜像名称不能为空")
	}
	// The tag endpoint requires the namespace, i.e. "ns/repo". Quay has no
	// implicit "library" namespace: a bare "nginx" is not addressable, so say
	// so instead of building a URL that 404s.
	if !strings.Contains(repository, "/") {
		return nil, fmt.Errorf("Quay 需要 命名空间/仓库 的形式，例如 prometheus/prometheus（%q 缺少命名空间）", repository)
	}

	values := url.Values{}
	values.Set("limit", "100")
	values.Set("onlyActiveTags", "true")

	var body quayTagsResponse
	if err := getJSON(ctx, q.client,
		q.baseURL+"/api/v2/repository/"+repository+"/tag/?"+values.Encode(), &body); err != nil {
		return nil, err
	}

	out := make([]TagInfo, 0, len(body.Tags))
	for _, t := range body.Tags {
		if t.Name == "" {
			continue
		}
		out = append(out, TagInfo{
			Name:    t.Name,
			Size:    t.Size,
			Updated: dateOnly(t.LastModified),
		})
	}
	return out, nil
}

// --- 1ms.run ------------------------------------------------------------

// OneMSSearcher searches the 1ms.run registry accelerator.
type OneMSSearcher struct {
	client  *http.Client
	baseURL string
}

// NewOneMSSearcher builds a 1ms searcher. baseURL is normally
// "https://1ms.run/api/v1/registry". A nil client uses the package default.
func NewOneMSSearcher(client *http.Client, baseURL string) *OneMSSearcher {
	if client == nil {
		client = searchHTTPClient()
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://1ms.run/api/v1/registry"
	}
	return &OneMSSearcher{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// ID implements Searcher.
func (m *OneMSSearcher) ID() string { return "1ms" }

// Name implements Searcher.
func (m *OneMSSearcher) Name() string { return "1ms 加速源" }

// envelope matches the 1ms API's {"code":0,"data":{...}} wrapper. The code
// field is not always present, so its absence is not treated as failure.
type envelope struct {
	Code int             `json:"code"`
	Data json.RawMessage `json:"data"`
}

func unwrap(body []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("接口返回异常: code=%d", env.Code)
	}
	if len(env.Data) == 0 {
		// Some deployments return the payload unwrapped.
		return body, nil
	}
	return env.Data, nil
}

type oneMSSearchData struct {
	Total int `json:"total"`
	List  []struct {
		Namespace    string `json:"namespace"`
		Name         string `json:"name"`
		Description  string `json:"description"`
		StarCount    int64  `json:"star_count"`
		PullCount    int64  `json:"pull_count"`
		LastModified string `json:"last_modified"`
		LastPushed   string `json:"last_pushed"`
	} `json:"list"`
}

// Search implements Searcher.
func (m *OneMSSearcher) Search(ctx context.Context, keyword string, page, pageSize int) (SearchPage, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return SearchPage{}, fmt.Errorf("关键词不能为空")
	}
	pageSize = clampPageSize(pageSize)
	if page <= 0 {
		page = 1
	}

	q := url.Values{}
	q.Set("query", keyword)
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))

	raw, err := getRaw(ctx, m.client, m.baseURL+"/search?"+q.Encode())
	if err != nil {
		return SearchPage{}, err
	}
	data, err := unwrap(raw)
	if err != nil {
		return SearchPage{}, fmt.Errorf("搜索接口返回异常: %s", truncate(string(raw), 200))
	}
	var body oneMSSearchData
	if err := json.Unmarshal(data, &body); err != nil {
		return SearchPage{}, fmt.Errorf("解析搜索响应失败: %w", err)
	}

	out := SearchPage{
		Results:  make([]SearchResult, 0, len(body.List)),
		Total:    body.Total,
		Page:     page,
		PageSize: pageSize,
	}
	for _, r := range body.List {
		ns := strings.TrimSpace(r.Namespace)
		if ns == "" {
			ns = "library"
		}
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		full := ns + "/" + name
		updated := r.LastModified
		if updated == "" {
			updated = r.LastPushed
		}
		out.Results = append(out.Results, SearchResult{
			Source:      m.ID(),
			Name:        full,
			Repository:  full,
			Image:       name,
			Description: strings.ReplaceAll(strings.TrimSpace(r.Description), "\n", " "),
			Stars:       r.StarCount,
			Pulls:       r.PullCount,
			Updated:     dateOnly(updated),
			Official:    ns == "library",
		})
	}
	return out, nil
}

type oneMSTagsData struct {
	List []struct {
		TagName string `json:"tag_name"`
		Images  []struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
			Size         int64  `json:"size"`
		} `json:"images"`
	} `json:"list"`
}

// Tags implements Searcher.
func (m *OneMSSearcher) Tags(ctx context.Context, repository string) ([]TagInfo, error) {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" {
		return nil, fmt.Errorf("镜像名称不能为空")
	}
	q := url.Values{}
	q.Set("repositories", repository)
	q.Set("page", "1")
	q.Set("page_size", "100")
	q.Set("sort_by", "last_updated")
	q.Set("sort_order", "DESC")

	raw, err := getRaw(ctx, m.client, m.baseURL+"/get_tags?"+q.Encode())
	if err != nil {
		return nil, err
	}
	data, err := unwrap(raw)
	if err != nil {
		return nil, fmt.Errorf("Tag 接口返回异常: %s", truncate(string(raw), 200))
	}
	var body oneMSTagsData
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("解析 Tag 响应失败: %w", err)
	}

	out := make([]TagInfo, 0, len(body.List))
	for _, r := range body.List {
		if r.TagName == "" {
			continue
		}
		ti := TagInfo{Name: r.TagName}
		for _, img := range r.Images {
			osName := img.OS
			if osName == "" {
				osName = "linux"
			}
			ti.Platforms = append(ti.Platforms, Platform{
				OS:           osName,
				Architecture: img.Architecture,
				Variant:      img.Variant,
			})
			if img.Size > ti.Size {
				ti.Size = img.Size
			}
		}
		ti.Platforms = DedupePlatforms(ti.Platforms)
		out = append(out, ti)
	}
	// Some deployments order arbitrarily; newest-first is what users expect.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name == "latest" {
			return true
		}
		if out[j].Name == "latest" {
			return false
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// getRaw performs a GET and returns the body, for endpoints with a
// non-standard envelope.
func getRaw(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 响应失败: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d: %s", rawURL, resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}
