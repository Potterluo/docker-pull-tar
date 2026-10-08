// Package config loads the bootstrap configuration from APP_* environment
// variables.
//
// There is no config file by design: deployment-time settings belong in
// the deployment manifest (systemd unit, docker-compose, k8s env).
// Everything user-facing (users, api keys, domain data) lives in the
// database — see internal/store.
//
// Every variable has a usable default, so a bare `./app` (or
// `docker run`) boots a working instance on SQLite with zero setup.
package config

import (
	"log/slog"
	"os"
	"strconv"
)

// Config is the process-level bootstrap configuration.
type Config struct {
	// HTTP server.
	Port int    // APP_PORT  — default 8080
	Bind string // APP_BIND  — "loopback" (default) or "all"

	// Storage.
	DataDir     string // APP_DATA_DIR      — sqlite home, default ./data
	DBType      string // APP_DB_TYPE       — "sqlite" (default) or "postgres"
	DBDSN       string // APP_DB_DSN        — empty = sqlite at $APP_DATA_DIR/app.db
	AutoMigrate bool   // APP_DB_AUTO_MIGRATE — default true

	// RateLimitRPM caps requests per minute per CLIENT IP on /api/*.
	// 0 disables the limit, which is the default: the tool is local and a
	// limit would only get in the operator's way. (There is no per-user
	// keying any more — see AGENTS.md convention 2.)
	RateLimitRPM int // APP_RATE_LIMIT_RPM — requests/minute per IP; 0 = unlimited

	// Development.
	//
	// DevProxy, when set (e.g. http://localhost:3000), reverse-proxies
	// everything that is not /api/* or /ws to a `next dev` process. The
	// browser talks to THIS server only, so cookies stay same-origin and
	// frontend HMR works through the proxy (httputil.ReverseProxy handles
	// the WebSocket upgrade Next's dev server needs). Production builds
	// leave it unset and serve the embedded static export instead.
	DevProxy string // APP_DEV_PROXY

	LogLevel string // APP_LOG_LEVEL — "debug" / "info" (default) / "warn" / "error"
}

// Load reads the bootstrap configuration. Defaults first, then .env
// (developer convenience — see dotenv.go), then environment overrides
// after — the table above documents every knob.
func Load() *Config {
	loadDotEnv()
	cfg := &Config{
		Port:        8080,
		Bind:        "loopback",
		DataDir:     "./data",
		DBType:      "sqlite",
		AutoMigrate: true,
		LogLevel:    "info",
	}

	if v := os.Getenv("APP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.Port = p
		}
	}
	if v := os.Getenv("APP_BIND"); v != "" {
		cfg.Bind = v
	}
	if v := os.Getenv("APP_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("APP_DB_TYPE"); v != "" {
		cfg.DBType = v
	}
	if v := os.Getenv("APP_DB_DSN"); v != "" {
		cfg.DBDSN = v
	}
	if v := os.Getenv("APP_DB_AUTO_MIGRATE"); v != "" {
		cfg.AutoMigrate = !(v == "false" || v == "0")
	}

	if v := os.Getenv("APP_RATE_LIMIT_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RateLimitRPM = n
		}
	}
	if v := os.Getenv("APP_DEV_PROXY"); v != "" {
		cfg.DevProxy = v
	}
	if v := os.Getenv("APP_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	return cfg
}

// ScrubBootSecrets removes credential-bearing env vars from the process
// environment AFTER they have been read into Config. Call once from main
// right after the store is open.
//
// Why: every subprocess spawned later (scripts, editors, debug tooling)
// inherits this env. Anything still set would be readable via
// /proc/<pid>/environ by anything running as the same user. Env is
// treated as one-time bootstrap input, not a live config source.
func ScrubBootSecrets() {
	for _, k := range []string{"APP_DB_DSN"} {
		_ = os.Unsetenv(k)
	}
}

// SetupLogging configures the default slog logger from APP_LOG_LEVEL.
func SetupLogging(level string) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})))
}
