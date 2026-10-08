package server

import (
	"net/http"
)

// route is one entry of the declarative /api/* table.
//
// Being data (instead of a pile of HandleFunc calls) lets routes_test.go
// enforce conventions over the whole surface at once. Generated entities
// append rows at the gen:routes marker (see `generator entity`).
type route struct {
	method  string
	pattern string
	h       http.HandlerFunc
}

// routes is the declarative /api/* route table.
//
// There is no auth level: every route is reachable, because the tool is a
// single-user local application. What a route DOES still declare is its
// HTTP method, so the table can be checked mechanically.
func (s *Server) routes() []route {
	return []route{
		// Bootstrap.
		{http.MethodGet, "/api/status", s.handleStatus},

		// F1 — image search (keyword) and tag listing.
		{http.MethodGet, "/api/search", s.handleSearch},
		{http.MethodGet, "/api/tags", s.handleTags},
		{http.MethodPost, "/api/images/inspect", s.handleInspectImage},

		// F2/F3/F4 — download tasks and their lifecycle.
		{http.MethodGet, "/api/tasks", s.handleListTasks},
		{http.MethodPost, "/api/tasks", s.handleCreateTask},
		{http.MethodGet, "/api/tasks/{id}", s.handleGetTask},
		{http.MethodDelete, "/api/tasks/{id}", s.handleDeleteTask},
		{http.MethodPost, "/api/tasks/{id}/pause", s.handlePauseTask},
		{http.MethodPost, "/api/tasks/{id}/resume", s.handleResumeTask},
		{http.MethodPost, "/api/tasks/{id}/cancel", s.handleCancelTask},
		{http.MethodPost, "/api/tasks/{id}/retry", s.handleRetryTask},

		// F5 — finished artifacts (the local .tar library).
		{http.MethodGet, "/api/artifacts", s.handleListArtifacts},
		{http.MethodGet, "/api/artifacts/{id}", s.handleDownloadArtifact},
		{http.MethodPost, "/api/artifacts/{id}/reveal", s.handleRevealArtifact},
		{http.MethodDelete, "/api/artifacts/{id}", s.handleDeleteArtifact},

		// Configuration: search sources, registry mirrors, settings.
		{http.MethodGet, "/api/sources", s.handleListSources},
		{http.MethodPost, "/api/sources", s.handleCreateSource},
		{http.MethodPut, "/api/sources/{id}", s.handleUpdateSource},
		{http.MethodDelete, "/api/sources/{id}", s.handleDeleteSource},
		// The built-in public registry catalog (read-only; see the handler).
		{http.MethodGet, "/api/registries", s.handleListRegistries},
		// Stored registry logins for private images. Secrets go in, never out.
		{http.MethodGet, "/api/credentials", s.handleListCredentials},
		{http.MethodPost, "/api/credentials", s.handleCreateCredential},
		{http.MethodPut, "/api/credentials/{id}", s.handleUpdateCredential},
		{http.MethodDelete, "/api/credentials/{id}", s.handleDeleteCredential},
		{http.MethodGet, "/api/settings", s.handleGetSettings},
		{http.MethodPut, "/api/settings", s.handleUpdateSettings},

		// Aggregates for the dashboard.
		{http.MethodGet, "/api/stats", s.handleGetStats},

		// Realtime (SSE).
		{http.MethodGet, "/api/events", s.handleEvents},

		// --- gen:routes ---
	}
}
