// Package app assembles the runtime from its long-lived pieces — config,
// store, event hub, task manager, HTTP server — in ONE place so both
// delivery targets share identical wiring:
//
//	cmd/server   CLI + headless single binary (serve over TCP)
//	cmd/desktop  Wails desktop shell (window → localhost server)
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/buildinfo"
	"github.com/Potterluo/docker-pull-tar/internal/config"
	"github.com/Potterluo/docker-pull-tar/internal/events"
	"github.com/Potterluo/docker-pull-tar/internal/server"
	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// App holds the booted runtime. Close must be called on shutdown.
type App struct {
	Cfg    *config.Config
	Store  store.Store
	Hub    *events.Hub
	Tasks  *tasks.Manager
	Server *server.Server
}

// Boot opens the store (with migrations), seeds the shipped defaults and
// wires the server. It does NOT listen — callers choose their delivery:
// Server.Run(ctx) for a TCP bind, or Serve(ctx, ln) for the desktop shell.
//
// The event hub is created HERE and handed to both the task manager and the
// server so there is exactly ONE hub per process. Two hubs = publishers and
// subscribers on different objects = events silently vanish (worse than a
// crash, since the SSE connection still establishes).
func Boot(cfg *config.Config) (*App, error) {
	st, err := store.New(&store.StorageConfig{
		Type:        cfg.DBType,
		DSN:         cfg.DBDSN,
		AutoMigrate: cfg.AutoMigrate,
	}, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	hub := events.New()
	mgr := tasks.NewManager(st, hub, cfg.DataDir, buildinfo.Version)

	// Seed the shipped sources and settings on first boot, so the CLI and a
	// fresh GUI agree on what exists.
	if err := mgr.EnsureConfig(context.Background()); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed defaults: %w", err)
	}

	// Park anything a previous process left mid-flight. A hard kill never
	// runs Shutdown, so without this a row stays "running" with no live job:
	// the /tasks page would show 下载中 forever, its 2-second refetch loop
	// would stay hot, and the dashboard's 进行中 counter would never clear.
	if parked, err := mgr.ReconcileTasks(context.Background()); err != nil {
		slog.Warn("could not reconcile interrupted tasks", "err", err)
	} else if parked > 0 {
		slog.Info("parked interrupted downloads", "count", parked,
			"hint", "resume them from the downloads list")
	}

	return &App{
		Cfg:    cfg,
		Store:  st,
		Hub:    hub,
		Tasks:  mgr,
		Server: server.New(cfg, st, hub, mgr),
	}, nil
}

// Close releases the store. Call once at shutdown, after Shutdown.
func (a *App) Close() {
	_ = a.Store.Close()
}

// Shutdown stops running downloads (leaving their resume state on disk) and
// releases the store.
//
// When a worker does not stop within the deadline the store is still closed —
// a process that refuses to exit is worse than a row that needs the next
// boot's ReconcileTasks to park it. The warning says so instead of pretending
// the drain succeeded.
func (a *App) Shutdown(ctx context.Context) {
	if a.Tasks != nil {
		if !a.Tasks.Shutdown(ctx) {
			slog.Warn("downloads did not stop within the deadline; " +
				"their resume state is on disk and they will be parked on next start")
		}
	}
	a.Close()
}

// Serve serves the built handler on an EXISTING listener and blocks until
// ctx is canceled or the listener fails. The desktop shell uses this with
// an ephemeral loopback listener: streaming responses (SSE) must ride a
// real TCP socket — WebView2's custom-scheme handler buffers responses,
// which would turn live progress into "nothing until done".
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	handler, err := a.Server.BuildHandler()
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
