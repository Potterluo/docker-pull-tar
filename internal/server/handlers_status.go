package server

import (
	"net/http"

	"github.com/Potterluo/docker-pull-tar/internal/buildinfo"
)

// handleStatus is the bootstrap probe the UI calls before it renders
// anything. It replaces the scaffold's auth-driven three-state probe:
// there is nothing to configure and nobody to log in, so all it reports is
// the build and where the data lives.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.tasks.Config(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeOK(w, map[string]any{
		// `configured` is declared by CONTRACT §6 and the client's Status
		// type. There is nothing left to configure now that auth is gone, but
		// a field the client reads must exist rather than be undefined.
		"configured": true,
		"version":    buildinfo.Version,
		"commit":     buildinfo.Commit,
		"date":       buildinfo.Date,
		"platform":   platformString(),
		"dataDir":    s.tasks.DataDir(),
		"outputDir":  cfg.OutputDir,
		"workers":    cfg.Workers,
		// auth is always false now; the field survives so an older UI
		// build does not have to special-case a missing key.
		"auth":     false,
		"projects": len(s.searchers),
	})
}

// handleGetStats returns the dashboard counters.
func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.tasks.Stats(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeOK(w, stats)
}
