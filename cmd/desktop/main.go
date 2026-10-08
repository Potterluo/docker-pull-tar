//go:build desktop

// desktop — the Wails delivery target: the SAME Go server and embedded
// UI as cmd/server, rendered in a native window through the OS webview.
//
// Architecture: the server listens on an EPHEMERAL loopback port and the
// window opens on http://127.0.0.1:<port>. The asset server serves only
// a one-line redirect page. Streaming rides a real TCP socket this way —
// WebView2 buffers responses served through its custom-scheme handler,
// which would break SSE (live progress). EventSource, fetches and
// websockets behave exactly as in a browser tab.
//
// Startup is deliberately paranoid, because a GUI binary has no console:
//
//   - The data directory is resolved through a probed fallback chain
//     (see resolveDataDir) instead of being nailed to %APPDATA%.
//   - WebView2's own profile directory is redirected under that data
//     directory, removing a second, independent %APPDATA% requirement.
//   - Every fatal error is written to <dataDir>/dockerpull-desktop.log AND
//     shown in a native message box, so "the app doesn't open" is never the
//     only symptom.
//
// Build (see `make desktop`):
//
//	go build -tags desktop,production ./cmd/desktop
//
// Platform notes: Windows needs no CGO (WebView2 ships with Win10/11).
// Linux needs webkit2gtk-4.1 dev packages; macOS needs the Xcode command
// line tools — both build with cgo enabled. For development with live
// reload, install the Wails CLI (`go install
// github.com/wailsapp/wails/v2/cmd/wails@latest`) and use `wails dev`.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing/fstest"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/Potterluo/docker-pull-tar/internal/app"
	"github.com/Potterluo/docker-pull-tar/internal/buildinfo"
	"github.com/Potterluo/docker-pull-tar/internal/config"
)

func main() {
	// config.Load() reads APP_DATA_DIR (including a .env entry), so it must
	// run before the data directory is resolved.
	cfg := config.Load()

	// Resolve a writable data directory and remember what was refused, so a
	// later failure can explain itself.
	dataDir, choices := resolveDataDir()
	cfg.DataDir = dataDir
	runtimePaths.DataDir = dataDir
	runtimePaths.Rejected = choices

	logPath, err := setupLogging(dataDir)
	runtimePaths.LogPath = logPath
	if err != nil {
		// Logging is the one thing that cannot degrade gracefully: report
		// through the dialog only.
		showFatalDialog("DockerPull 无法启动",
			"无法创建日志文件: "+err.Error()+"\n\n数据目录: "+dataDir)
		os.Exit(1)
	}

	slog.Info("desktop starting",
		"version", buildinfo.Version,
		"dataDir", dataDir,
		"log", logPath,
		"pid", os.Getpid(),
	)
	// Record every rejected candidate at startup, not only on failure: when a
	// user reports "it uses the wrong folder", the answer is already in the log.
	for _, c := range choices {
		if c.Err != nil {
			slog.Warn("data directory rejected", "dir", c.Dir, "source", c.Label, "err", c.Err)
		}
	}

	if err := run(cfg); err != nil {
		fatal("启动失败", err)
	}
	slog.Info("desktop exited cleanly")
}

// run boots the runtime and blocks until the window closes.
func run(cfg *config.Config) error {
	application, err := app.Boot(cfg)
	if err != nil {
		return err
	}
	defer application.Close()

	// Ephemeral loopback listener; the window is pointed at it below.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := application.Serve(ctx, ln); err != nil {
			slog.Error("serve", "err", err)
			cancel()
		}
	}()

	// WebView2 keeps its profile (cache, GPU state, localStorage) in its own
	// directory. Left unset it defaults to %APPDATA%\<exe name> — a SECOND hard
	// dependency on a writable %APPDATA%, one that would fail with an opaque
	// 800700aa after the data directory had already been fixed. Routing it under
	// the resolved data directory makes the app depend on exactly one writable
	// location.
	webviewDir := filepath.Join(cfg.DataDir, "webview2")
	if err := os.MkdirAll(webviewDir, 0o755); err != nil {
		return fmt.Errorf("创建 WebView2 配置目录失败: %w", err)
	}

	// The asset server exists only to bounce the window onto the real
	// origin — nothing else is ever served from wails://.
	startURL := fmt.Sprintf("http://127.0.0.1:%d/", port)
	redirectAssets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(fmt.Sprintf(
			`<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s">`+
				`</head><body style="background:#09090b"></body></html>`, startURL))},
	}

	slog.Info("desktop window opening", "url", startURL, "webview2Data", webviewDir)
	return wails.Run(&options.App{
		Title:     "DockerPull",
		Width:     1280,
		Height:    832,
		MinWidth:  960,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: redirectAssets,
		},
		// The context is kept for future bindings (native dialogs,
		// tray, single-instance lock).
		OnStartup: func(_ context.Context) {},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			// See above: this removes the second %APPDATA% dependency.
			WebviewUserDataPath: webviewDir,
		},
	})
}
