package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// createTaskRequest is the body of POST /api/tasks.
//
// Credentials are accepted but never stored or echoed: they belong to this
// one download. (There is no credential store — see AGENTS.md.)
//
// Note the two TLS-ish knobs, which are NOT the same thing:
//
//	insecure — skip TLS *certificate verification* (the UI's checkbox)
//	useHTTP  — talk to the registry over plain HTTP, no TLS at all
//
// The CLI's legacy `--insecure` maps to useHTTP; `--no-verify-tls` maps to
// insecure. Both names are documented in the README.
type createTaskRequest struct {
	Image     string `json:"image"`
	Registry  string `json:"registry"`
	Platform  string `json:"platform"`
	Workers   int    `json:"workers"`
	Insecure  bool   `json:"insecure"`
	UseHTTP   bool   `json:"useHTTP"`
	VerifyTLS *bool  `json:"verifyTls"`
	OutputDir string `json:"outputDir"`
	Username  string `json:"username"`
	Password  string `json:"password"`
}

// handleCreateTask starts a download (F2/F4).
//
// It returns as soon as the task row exists and the job is launched — the
// client watches /api/events for progress.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Image) == "" {
		writeError(w, http.StatusBadRequest, "镜像名称不能为空")
		return
	}
	if req.Platform != "" {
		if p := registry.ParsePlatform(req.Platform); p.Architecture == "" {
			writeError(w, http.StatusBadRequest, "架构格式无效: "+req.Platform)
			return
		}
	}

	spec := tasks.StartSpec{
		Image:       req.Image,
		Registry:    req.Registry,
		Platform:    req.Platform,
		Workers:     req.Workers,
		Insecure:    req.UseHTTP,
		OutputDir:   req.OutputDir,
		Credentials: registry.Credentials{Username: req.Username, Password: req.Password},
	}
	// Certificate verification: the stored `verify_tls` setting is the
	// baseline (the manager applies it), and either an explicit
	// verifyTls:false or the insecure flag can turn it off. Nothing in this
	// request can turn verification ON past a stored "off".
	if req.VerifyTLS != nil && !*req.VerifyTLS {
		spec.SkipVerifyTLS = true
	} else if req.VerifyTLS == nil && req.Insecure {
		spec.SkipVerifyTLS = true
	}

	task, err := s.tasks.Start(r.Context(), spec)
	if err != nil {
		// A bad image string is the user's mistake (400); anything else is
		// a registry or filesystem problem (502). The classification comes
		// from a typed sentinel — substring matching on the message misfiled
		// every unrecognised parse error as a 502.
		if errors.Is(err, tasks.ErrInvalidImage) || errors.Is(err, tasks.ErrInvalidRegistry) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"task": task})
}

// handleListTasks returns tasks newest-first.
//
//	GET /api/tasks?limit=50
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.tasks.List(r.Context(), limit)
	if err != nil {
		storeError(w, err)
		return
	}
	if list == nil {
		list = []tasks.Task{}
	}
	writeOK(w, map[string]any{"tasks": list})
}

// handleGetTask returns one task with its per-layer progress.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.tasks.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeOK(w, map[string]any{"task": task})
}

// handleDeleteTask removes a task. `?files=true` also deletes its working
// directory (the resumable byte cache) and the tar it produced.
func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	removeFiles := parseBoolQuery(r.URL.Query().Get("files"))
	if err := s.tasks.Delete(r.Context(), r.PathValue("id"), removeFiles); err != nil {
		storeError(w, err)
		return
	}
	writeOK(w, map[string]any{"id": r.PathValue("id")})
}

// handlePauseTask stops a running task, keeping its resume ledger.
func (s *Server) handlePauseTask(w http.ResponseWriter, r *http.Request) {
	s.taskLifecycle(w, r, s.tasks.Pause)
}

// handleResumeTask continues a paused/failed/canceled task from its
// recorded byte offsets.
func (s *Server) handleResumeTask(w http.ResponseWriter, r *http.Request) {
	s.taskLifecycle(w, r, s.tasks.Resume)
}

// handleCancelTask stops a running task and marks it canceled. The resume
// ledger survives, so a later Resume still works.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	s.taskLifecycle(w, r, s.tasks.Cancel)
}

// handleRetryTask discards the resume ledger and downloads from byte 0.
func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	s.taskLifecycle(w, r, s.tasks.Retry)
}

// taskLifecycle runs one of the lifecycle transitions and returns the
// refreshed task, so the UI can update without a second round trip.
func (s *Server) taskLifecycle(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, id string) error) {
	id := r.PathValue("id")
	if err := fn(r.Context(), id); err != nil {
		// "not found" is a 404; a state conflict is a 409, which tells the
		// UI to stop and re-read rather than retry blindly.
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	task, err := s.tasks.Get(r.Context(), id)
	if err != nil {
		writeOK(w, map[string]any{"id": id})
		return
	}
	writeOK(w, map[string]any{"task": task})
}
