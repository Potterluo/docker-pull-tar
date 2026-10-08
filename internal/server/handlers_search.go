package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/puller"
	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
)

// handleSearch implements F1: keyword search across the configured search
// sources.
//
//	GET /api/search?q=nginx&source=<sourceRowID>&page=1&pageSize=20
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "关键词不能为空")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))

	searcher, err := s.resolveSearcher(r, r.URL.Query().Get("source"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := searcher.Search(r.Context(), q, page, pageSize)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{
		"results":  res.Results,
		"total":    res.Total,
		"page":     res.Page,
		"pageSize": res.PageSize,
		"source":   searcher.ID(),
	})
}

// handleTags lists a repository's tags.
//
//	GET /api/tags?repository=library/nginx&source=<sourceRowID>
func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repository"))
	if repo == "" {
		writeError(w, http.StatusBadRequest, "镜像名称不能为空")
		return
	}
	searcher, err := s.resolveSearcher(r, r.URL.Query().Get("source"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tags, err := searcher.Tags(r.Context(), repo)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"tags": tags, "source": searcher.ID()})
}

// inspectRequest is the body of POST /api/images/inspect.
type inspectRequest struct {
	Image    string `json:"image"`
	Registry string `json:"registry"`
	Platform string `json:"platform"`
	Source   string `json:"source"`
	// Insecure uses plain HTTP for the registry, matching taskRequest.Insecure.
	// It has to exist here too: a private registry on http:// would otherwise be
	// inspectable from the CLI (`pull --list-arch --insecure`) but never from
	// the GUI, so the tag/arch picker failed with "server gave HTTP response to
	// HTTPS client" for exactly the registries that need it most.
	Insecure bool `json:"insecure"`
}

// inspectResult is the resolved description of an image. Its JSON shape is
// fixed by CONTRACT §6 — the fields sit at the TOP LEVEL of the envelope
// (the UI reads `res.platforms`, not `res.image.platforms`).
type inspectResult struct {
	Image     string              `json:"image"`
	Ref       registry.Ref        `json:"ref"`
	MediaType string              `json:"mediaType"`
	IsIndex   bool                `json:"isIndex"`
	Platforms []registry.Platform `json:"platforms"`
	Actual    *registry.Platform  `json:"actual,omitempty"`
	Tags      []string            `json:"tags"`
	// TagsFrom names where the tag list came from: the registry host, or a
	// searcher id when the registry has no tags/list. Empty when neither had
	// anything.
	TagsFrom   string `json:"tagsFrom"`
	TotalBytes int64  `json:"totalBytes"`
	LayerCount int    `json:"layerCount"`
}

// handleInspectImage resolves an image's available architectures (and, on a
// best-effort basis, its tag list) WITHOUT downloading any layer. This is
// what makes the arch picker possible.
func (s *Server) handleInspectImage(w http.ResponseWriter, r *http.Request) {
	var req inspectRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Image) == "" {
		writeError(w, http.StatusBadRequest, "镜像名称不能为空")
		return
	}

	cfg, err := s.tasks.Config(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}

	custom := strings.TrimSpace(req.Registry)
	if custom == "" {
		custom = s.tasks.MirrorHost(r.Context(), "")
	} else if host, ok := registry.PullHost(custom); ok {
		// Accepts a mirror id, an upstream-registry id ('mcr', 'quay') or a host —
		// the same spellings the --mirror flag takes.
		custom = host
	}
	ref, err := registry.Parse(req.Image, custom)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Insecure {
		ref.UseHTTP = true
	}
	if req.Platform != "" {
		// Inspecting a digest-pinned or tag ref never needs a platform, but
		// a caller may ask "is this arch available?".
		_ = registry.ParsePlatform(req.Platform)
	}

	opts := puller.Options{
		Ref:         ref,
		WorkDir:     s.tasks.DataDir(),
		ProxyURL:    cfg.ProxyURLForPuller(),
		InsecureTLS: !cfg.VerifyTLS,
	}

	info, err := puller.InspectImage(r.Context(), opts)
	if err != nil {
		// Surface the registry's own message: it is actionable ("manifest
		// unknown", "unauthorized"), unlike a generic 502.
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Tags come from the registry being inspected FIRST, because that is the
	// host the download will use: it is the only list guaranteed to be pullable.
	// The searcher is a fallback for a registry with no tags/list endpoint, and
	// it may be a different registry entirely (Docker Hub's API while pulling
	// from ghcr.io), so it must never take precedence — that is how the picker
	// ends up offering another publisher's tag. tagsFrom records which one won,
	// so the UI can say where a tag list came from.
	tags := info.Tags
	tagsFrom := info.TagsFrom
	if len(tags) == 0 {
		if searcher, serr := s.resolveSearcher(r, req.Source); serr == nil {
			// Only a searcher that serves the SAME namespace may answer. The
			// Docker Hub API describing a ghcr.io repository offers tags the
			// download cannot have (the two lists genuinely differ), so a
			// mismatch reports no tags rather than the wrong ones.
			if registry.SearcherServesRegistry(ref.Registry, searcher.ID()) {
				if infos, terr := searcher.Tags(r.Context(), ref.Repository); terr == nil {
					for _, t := range infos {
						tags = append(tags, t.Name)
					}
					if len(tags) > 0 {
						tagsFrom = searcher.ID()
					}
				}
			}
		}
	}
	if tags == nil {
		tags = []string{}
	}

	out := inspectResult{
		Image:      req.Image,
		Ref:        info.Ref,
		MediaType:  info.MediaType,
		IsIndex:    info.IsIndex,
		Platforms:  info.Platforms,
		Tags:       tags,
		TagsFrom:   tagsFrom,
		TotalBytes: info.TotalBytes,
		LayerCount: info.LayerCount,
	}
	if out.Platforms == nil {
		out.Platforms = []registry.Platform{}
	}
	if !info.IsIndex && len(out.Platforms) == 1 {
		actual := out.Platforms[0]
		out.Actual = &actual
	}

	// The UI reads these fields straight off the envelope, so they are
	// marshalled flat rather than nested under "image".
	writeOK(w, map[string]any{
		"image":      out.Image,
		"ref":        out.Ref,
		"mediaType":  out.MediaType,
		"isIndex":    out.IsIndex,
		"platforms":  out.Platforms,
		"actual":     out.Actual,
		"tags":       out.Tags,
		"tagsFrom":   out.TagsFrom,
		"totalBytes": out.TotalBytes,
		"layerCount": out.LayerCount,
	})
}

// resolveSearcher turns the source identifier the UI sends (a stored
// `sources` row id) into a live Searcher.
//
// The UI lists configured search sources, so it sends a row id — not the
// built-in id. Both are accepted: passing "dockerhub" or "1ms" works too,
// which keeps the CLI and hand-written curl calls ergonomic.
func (s *Server) resolveSearcher(r *http.Request, idOrEmpty string) (registry.Searcher, error) {
	id := strings.TrimSpace(idOrEmpty)

	// No identifier: use whichever source the user marked as default, and
	// only fall back to the built-in setting when none is flagged. Reading
	// the flag is what makes 设为默认 work for a user-added search source —
	// the settings key alone can only name a built-in id.
	if id == "" {
		if row, err := s.defaultSearchSource(r); err == nil && row != nil {
			if searcher, rerr := s.searcherForRow(row); rerr == nil {
				return searcher, nil
			}
		}
		cfg, err := s.tasks.Config(r.Context())
		if err != nil {
			return nil, err
		}
		id = cfg.DefaultSearchSource
	}

	// A built-in id resolves directly.
	if searcher, err := registry.PickSearcher(s.searchers, id); err == nil {
		return searcher, nil
	}

	// Otherwise it is a stored source row.
	row, err := s.store.GetSource(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			return nil, fmt.Errorf("未知的搜索源 %q", idOrEmpty)
		}
		return nil, err
	}
	return s.searcherForRow(row)
}

// defaultSearchSource returns the enabled search source flagged as default,
// or nil when none is.
func (s *Server) defaultSearchSource(r *http.Request) (*store.Source, error) {
	rows, err := s.store.ListSources(r.Context(), "search")
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].IsDefault && rows[i].Enabled {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// searcherForRow maps a stored source onto a live Searcher, by matching its
// URL against a supported API shape. A self-hosted copy of one of those APIs
// therefore works, as long as its URL says so.
func (s *Server) searcherForRow(row *store.Source) (registry.Searcher, error) {
	if row.Kind != "search" {
		return nil, fmt.Errorf("数据源 %q 不是搜索源", row.Name)
	}
	url := strings.TrimSpace(row.URL)
	for _, builtin := range registry.BuiltinSearchSources {
		if strings.EqualFold(url, builtin.URL) {
			if searcher, ok := s.searchers[builtin.ID]; ok {
				return searcher, nil
			}
		}
	}
	if strings.Contains(url, "1ms") {
		return registry.NewOneMSSearcher(nil, url), nil
	}
	if strings.Contains(url, "hub.docker.com") || strings.Contains(url, "dockerhub") {
		return registry.NewDockerHubSearcher(nil, url), nil
	}
	if strings.Contains(url, "quay.io") {
		// Covers a self-hosted Quay, whose URL will not equal the built-in one.
		return registry.NewQuaySearcher(nil, url), nil
	}
	if strings.Contains(url, "mcr.microsoft.com") {
		// MCR: catalog-backed search (no keyword API of its own).
		return registry.NewMCRSearcher(nil, url), nil
	}
	if url == "" {
		return nil, fmt.Errorf("数据源 %q 未配置 API 地址", row.Name)
	}
	return nil, fmt.Errorf("无法识别的搜索源 %q（地址 %s）；目前支持 Docker Hub、Quay、1ms 与 MCR 四种接口形态", row.Name, url)
}
