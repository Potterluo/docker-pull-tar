package server

import (
	"net/http"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/events"
	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// eventOf builds a hub event (the artifacts handler publishes directly).
func eventOf(eventType string, data any) events.Event {
	return events.Event{Type: eventType, Data: data}
}

// handleListRegistries returns the built-in public registry catalog.
//
//	GET /api/registries
//
// Read-only on purpose. The list is static data compiled into the binary
// (registry.BuiltinRegistries), so it cannot drift from what the code actually
// supports and a user cannot delete an entry that would reappear on the next
// boot. Pulling from any of these needs no configuration — a reference like
// `ghcr.io/owner/repo` already resolves — so this endpoint exists to make them
// DISCOVERABLE, and to say which ones can be searched by keyword.
func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	registries := registry.BuiltinRegistries
	if registries == nil {
		registries = []registry.Registry{}
	}
	// defaultPullHost is where a reference with NO host resolves today: the
	// `default_mirror` setting, which may name a built-in accelerator by id, a
	// user-added mirror by host, or nothing at all. Resolved server-side because
	// only the server can turn "nju" into "docker.nju.edu.cn" — and because the
	// answer must not be guessed as docker.io, since an operator behind a
	// firewall typically points it at an accelerator.
	writeOK(w, map[string]any{
		"registries":      registries,
		"defaultPullHost": s.tasks.MirrorHost(r.Context(), ""),
	})
}

// sourceRequest is the body of POST/PUT /api/sources.
type sourceRequest struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Host      string `json:"host"`
	Enabled   *bool  `json:"enabled"`
	Priority  *int   `json:"priority"`
	IsDefault *bool  `json:"isDefault"`
}

// handleListSources returns the configured search sources and mirrors.
//
//	GET /api/sources?kind=search|mirror
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && kind != tasks.KindSearch && kind != tasks.KindMirror {
		writeError(w, http.StatusBadRequest, "kind 必须是 search 或 mirror")
		return
	}
	list, err := s.store.ListSources(r.Context(), kind)
	if err != nil {
		storeError(w, err)
		return
	}
	if list == nil {
		list = []store.Source{}
	}
	writeOK(w, map[string]any{"sources": viewsFor(list)})
}

// sourceView is a source row plus the computed facts the UI needs.
//
// PullHost answers "if I search here, where will the download come from?".
// Without it the client can only send the bare repository name, and the pull
// lands on whatever the default mirror is — which is how searching 1ms ended up
// fetching from registry-1.docker.io. It is empty for a source that should use
// the configured default mirror.
type sourceView struct {
	store.Source
	PullHost string `json:"pullHost,omitempty"`
}

func viewsFor(list []store.Source) []sourceView {
	out := make([]sourceView, 0, len(list))
	for i := range list {
		v := sourceView{Source: list[i]}
		if list[i].Kind == tasks.KindSearch {
			// The stored row id is a random "src_…", so the mapping is by the
			// source's identity as a built-in (its name, or the id the seeder
			// derived it from).
			v.PullHost = registry.PullHostForSearchSource(builtinSearchID(list[i]))
		}
		out = append(out, v)
	}
	return out
}

// builtinSearchID recovers which built-in search source a row came from.
//
// SeedSources stores a generated id and the built-in's NAME (and URL), not the
// built-in id, so the name is what identifies it. A user-added source matches
// nothing and simply gets no pull host.
func builtinSearchID(row store.Source) string {
	trimmed := strings.TrimSpace(row.Name)
	for _, b := range registry.BuiltinSearchSources {
		if strings.EqualFold(b.Name, trimmed) || strings.EqualFold(b.URL, strings.TrimSpace(row.URL)) {
			return b.ID
		}
	}
	return ""
}

// handleCreateSource adds a search source or a mirror.
//
// A user-defined source is only usable if its API shape is one this build
// understands; that check happens at search time (see resolveSearcher), so
// creating a row always succeeds and a bad one fails with an actionable
// message rather than silently returning nothing.
func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if !readJSON(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind != tasks.KindSearch && kind != tasks.KindMirror {
		writeError(w, http.StatusBadRequest, "kind 必须是 search 或 mirror")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "名称不能为空")
		return
	}

	src := &store.Source{
		ID:       newSourceID(),
		Kind:     kind,
		Name:     name,
		URL:      strings.TrimSpace(req.URL),
		Host:     strings.TrimSpace(req.Host),
		Enabled:  true,
		Priority: 100,
	}
	if kind == tasks.KindMirror {
		if src.Host == "" {
			writeError(w, http.StatusBadRequest, "镜像源必须填写仓库地址 (host)")
			return
		}
	} else if src.URL == "" {
		writeError(w, http.StatusBadRequest, "搜索源必须填写 API 地址 (url)")
		return
	}
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		src.Priority = *req.Priority
	}
	if req.IsDefault != nil {
		src.IsDefault = *req.IsDefault
	}

	if err := s.store.CreateSource(r.Context(), src); err != nil {
		if isDuplicate(err) {
			writeError(w, http.StatusConflict, "已存在同名的数据源")
			return
		}
		storeError(w, err)
		return
	}
	if src.IsDefault {
		s.setDefaultSource(r, src)
	}
	s.hub.Publish(eventOf(tasks.EventSourceChanged, map[string]any{"id": src.ID}))
	writeOK(w, map[string]any{"source": src})
}

// handleUpdateSource edits a source. Only the supplied fields change.
func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id := trimPathParam(r.PathValue("id"))
	src, err := s.store.GetSource(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	var req sourceRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		src.Name = strings.TrimSpace(req.Name)
	}
	if req.URL != "" {
		src.URL = strings.TrimSpace(req.URL)
	}
	if req.Host != "" {
		src.Host = strings.TrimSpace(req.Host)
	}
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		src.Priority = *req.Priority
	}
	if req.IsDefault != nil {
		src.IsDefault = *req.IsDefault
	}
	if src.Kind == tasks.KindMirror && src.Host == "" {
		writeError(w, http.StatusBadRequest, "镜像源必须填写仓库地址 (host)")
		return
	}
	if src.Kind == tasks.KindSearch && src.URL == "" {
		writeError(w, http.StatusBadRequest, "搜索源必须填写 API 地址 (url)")
		return
	}

	if err := s.store.UpdateSource(r.Context(), src); err != nil {
		if isDuplicate(err) {
			writeError(w, http.StatusConflict, "已存在同名的数据源")
			return
		}
		storeError(w, err)
		return
	}
	if src.IsDefault {
		s.setDefaultSource(r, src)
	}
	s.hub.Publish(eventOf(tasks.EventSourceChanged, map[string]any{"id": src.ID}))
	writeOK(w, map[string]any{"source": src})
}

// handleDeleteSource removes a source. The built-in search source cannot be
// deleted — its id is the fallback the CLI and a fresh install use.
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := trimPathParam(r.PathValue("id"))
	src, err := s.store.GetSource(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	if isBuiltinSearchSourceName(src) {
		writeError(w, http.StatusConflict, "内置搜索源不能删除，只能停用")
		return
	}
	if err := s.store.DeleteSource(r.Context(), id); err != nil {
		storeError(w, err)
		return
	}
	s.hub.Publish(eventOf(tasks.EventSourceChanged, map[string]any{"id": id}))
	writeOK(w, map[string]any{"id": id})
}

// setDefaultSource clears the flag on every other source of the same kind,
// and records the choice in settings so the CLI and the resolution paths
// agree on it.
//
// For a mirror the settings value is the HOST: that works for a built-in
// (whose host the id maps to) and equally for a user-added internal registry,
// which the previous "only if it matches a built-in" rule silently dropped.
func (s *Server) setDefaultSource(r *http.Request, chosen *store.Source) {
	others, err := s.store.ListSources(r.Context(), chosen.Kind)
	if err != nil {
		return
	}
	for i := range others {
		if others[i].ID == chosen.ID || !others[i].IsDefault {
			continue
		}
		others[i].IsDefault = false
		_ = s.store.UpdateSource(r.Context(), &others[i])
	}

	if chosen.Kind == tasks.KindMirror {
		host := strings.TrimSpace(chosen.Host)
		if host == "" {
			return
		}
		// Prefer the built-in id when the host IS one, so the stored value
		// stays human-readable and survives the mirror list changing.
		if builtin, ok := registry.MirrorByHost(host); ok {
			host = builtin.ID
		}
		_, _ = s.tasks.SaveConfig(r.Context(), map[string]string{
			tasks.KeyDefaultMirror: host,
		})
		return
	}

	// A search source: remember the built-in id when the URL is one, so the
	// CLI resolves it without a database lookup.
	for _, b := range registry.BuiltinSearchSources {
		if strings.EqualFold(b.URL, chosen.URL) {
			_, _ = s.tasks.SaveConfig(r.Context(), map[string]string{
				tasks.KeyDefaultSearchSource: b.ID,
			})
			return
		}
	}
	// A custom search source stays addressed by its default flag, which
	// resolveSearcher consults (see defaultSearchSource below).
}

func isBuiltinSearchSourceName(src *store.Source) bool {
	if src.Kind != tasks.KindSearch {
		return false
	}
	for _, b := range registry.BuiltinSearchSources {
		if strings.EqualFold(b.URL, src.URL) {
			return true
		}
	}
	return false
}

// newSourceID mints a source id. Kept separate so the prefix is greppable.
func newSourceID() string {
	id, err := randomID("src_")
	if err != nil {
		return "src_" + strings.Repeat("0", 24)
	}
	return id
}
