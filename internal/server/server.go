// Package server hosts the HTTP surface: the JSON API under /api/*, the
// SSE event stream, health probes, and the embedded web UI.
//
// Layout conventions:
//
//   - server.go     — struct, route table, lifecycle (this file)
//   - middleware.go — logging / recovery / security headers / rate limit
//   - respond.go    — JSON write/read helpers (the ONE response shape)
//   - handlers_*.go — one file per domain, methods on *Server
//   - events.go     — realtime (SSE)
//   - spa.go        — embedded single-page app
//   - devproxy.go   — dev-time reverse proxy to `next dev`
//
// There is no authentication: DockerPull is a single-user local tool, so
// every route is reachable and no handler filters rows by owner.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/buildinfo"
	"github.com/Potterluo/docker-pull-tar/internal/config"
	"github.com/Potterluo/docker-pull-tar/internal/events"
	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// Server owns every long-lived dependency. Built once in main via New.
type Server struct {
	cfg       *config.Config
	store     store.Store
	hub       *events.Hub
	tasks     *tasks.Manager
	searchers map[string]registry.Searcher
	limiter   *rateLimiter
	startedAt time.Time
}

// New wires the server together. hub must be the process-wide hub created
// by internal/app.Boot — publishers and subscribers have to share it.
func New(cfg *config.Config, st store.Store, hub *events.Hub, mgr *tasks.Manager) *Server {
	return &Server{
		cfg:       cfg,
		store:     st,
		hub:       hub,
		tasks:     mgr,
		searchers: registry.SearchSources(nil),
		limiter:   newRateLimiter(cfg.RateLimitRPM),
		startedAt: time.Now(),
	}
}

// BuildHandler assembles the complete HTTP handler — API, health probes,
// SPA root. Run() serves it over TCP; the desktop target (cmd/desktop)
// hands it to the Wails asset server instead, so both delivery shapes
// share one route table.
func (s *Server) BuildHandler() (http.Handler, error) {
	mux := http.NewServeMux()

	// Health probes — no middleware noise.
	healthz := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /livez", healthz)
	mux.HandleFunc("GET /readyz", healthz)

	// The /api/* router: a DECLARATIVE route table (see s.routes()).
	// Declarative beats a pile of HandleFunc calls because the table can be
	// introspected by tests — routes_test.go asserts every row names a
	// method and that the table is non-empty.
	api := http.NewServeMux()
	for _, rt := range s.routes() {
		api.HandleFunc(rt.method+" "+rt.pattern, s.limit(rt.h))
	}
	mux.Handle("/api/", s.chain(api))

	// Everything else: the SPA. In dev, proxy to `next dev` for HMR;
	// in production, serve the embedded static export.
	root, err := s.rootHandler()
	if err != nil {
		return nil, err
	}
	mux.Handle("/", s.chain(root))
	return mux, nil
}

// Hub exposes the server's event hub. Delivery hosts must publish through
// THIS hub — it is the same instance the SSE handler subscribes on.
func (s *Server) Hub() *events.Hub { return s.hub }

// Run starts the HTTP server and blocks until ctx is canceled. The
// shutdown path gives in-flight requests 5s to finish, then returns.
func (s *Server) Run(ctx context.Context) error {
	handler, err := s.BuildHandler()
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(bindHost(s.cfg.Bind), fmt.Sprintf("%d", s.cfg.Port))
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	slog.Info("server running",
		"url", fmt.Sprintf("http://localhost:%d", s.cfg.Port),
		"version", buildinfo.Version,
		"devProxy", s.cfg.DevProxy,
	)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// bindHost maps the Bind setting to a listen address.
func bindHost(bind string) string {
	if bind == "all" {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// limit applies the optional per-IP rate limit to a handler. The tool is
// local, so the key is always the client IP.
func (s *Server) limit(next http.HandlerFunc) http.HandlerFunc {
	return rateLimit(s.limiter, clientIP, next)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// platformString reports the host platform, for /api/status.
func platformString() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
