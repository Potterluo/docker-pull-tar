package server

import (
	"net/http"
	"strconv"

	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// handleGetSettings returns the effective configuration. The values are the
// resolved ones (defaults filled in), so the UI never has to know them.
//
//	GET /api/settings
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.tasks.Config(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	// The UI renders a flat string map, matching the key/value store, so
	// the same keys work for a read and a write.
	writeOK(w, map[string]any{
		"settings": map[string]string{
			tasks.KeyOutputDir:           cfg.OutputDir,
			tasks.KeyWorkers:             strconv.Itoa(cfg.Workers),
			tasks.KeyVerifyTLS:           strconv.FormatBool(cfg.VerifyTLS),
			tasks.KeyProxyMode:           cfg.ProxyMode,
			tasks.KeyProxyURL:            cfg.ProxyURL,
			tasks.KeyDefaultMirror:       cfg.DefaultMirror,
			tasks.KeyDefaultSearchSource: cfg.DefaultSearchSource,
			tasks.KeyChunkThresholdMB:    strconv.Itoa(cfg.ChunkThresholdMB),
			tasks.KeyMaxRetries:          strconv.Itoa(cfg.MaxRetries),
		},
		"resolved": cfg,
	})
}

// handleUpdateSettings applies a bulk key/value patch.
//
//	PUT /api/settings   {"workers":"8","verify_tls":"false"}
//
// Unknown keys are rejected rather than stored, so a typo surfaces as a 400
// instead of silently doing nothing.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]string
	if !readJSON(w, r, &patch) {
		return
	}
	if len(patch) == 0 {
		writeError(w, http.StatusBadRequest, "没有需要更新的配置项")
		return
	}

	cfg, err := s.tasks.SaveConfig(r.Context(), patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.Publish(eventOf(tasks.EventSettingsChanged, map[string]any{}))
	writeOK(w, map[string]any{"resolved": cfg})
}
