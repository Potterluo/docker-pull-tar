package tasks

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
)

// Setting keys. These are the whole user-facing configuration surface; the
// GUI's Settings page and the CLI both write them through SaveConfig.
const (
	KeyOutputDir           = "output_dir"
	KeyWorkers             = "workers"
	KeyVerifyTLS           = "verify_tls"
	KeyProxyMode           = "proxy_mode"
	KeyProxyURL            = "proxy_url"
	KeyDefaultMirror       = "default_mirror"
	KeyDefaultSearchSource = "default_search_source"
	KeyChunkThresholdMB    = "chunk_threshold_mb"
	KeyMaxRetries          = "max_retries"
)

// Proxy modes.
const (
	ProxySystem = "system"
	ProxyNone   = "none"
	ProxyCustom = "custom"
)

// Bounds for the numeric settings. A user typing 0 or 100000 workers must
// not be able to open 100000 sockets.
const (
	minWorkers = 1
	maxWorkers = 32
	minRetries = 0
	maxRetries = 50
	minChunkMB = 1
	maxChunkMB = 1024
)

// Config is the resolved user configuration.
type Config struct {
	OutputDir           string `json:"outputDir"`
	Workers             int    `json:"workers"`
	VerifyTLS           bool   `json:"verifyTls"`
	ProxyMode           string `json:"proxyMode"`
	ProxyURL            string `json:"proxyUrl"`
	DefaultMirror       string `json:"defaultMirror"`
	DefaultSearchSource string `json:"defaultSearchSource"`
	ChunkThresholdMB    int    `json:"chunkThresholdMb"`
	MaxRetries          int    `json:"maxRetries"`
}

// ConfigDefaults returns the shipped defaults. dataDir supplies the default
// output directory so a fresh install writes next to its database.
func ConfigDefaults(dataDir string) map[string]string {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "."
	}
	return map[string]string{
		KeyOutputDir:           filepath.Join(dataDir, "downloads"),
		KeyWorkers:             strconv.Itoa(4),
		KeyVerifyTLS:           "true",
		KeyProxyMode:           ProxySystem,
		KeyProxyURL:            "",
		KeyDefaultMirror:       registry.DefaultMirror().ID,
		KeyDefaultSearchSource: registry.DefaultSearchSourceID,
		KeyChunkThresholdMB:    "50",
		KeyMaxRetries:          "10",
	}
}

// SettingKeys lists every key this package understands, so the GUI and the
// CLI can validate a bulk update.
func SettingKeys() []string {
	keys := []string{
		KeyOutputDir, KeyWorkers, KeyVerifyTLS, KeyProxyMode, KeyProxyURL,
		KeyDefaultMirror, KeyDefaultSearchSource, KeyChunkThresholdMB, KeyMaxRetries,
	}
	sort.Strings(keys)
	return keys
}

// IsSettingKey reports whether key is a recognised setting.
func IsSettingKey(key string) bool {
	for _, k := range SettingKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// ResolveConfig turns the raw stored key/value map into a validated Config.
// Unknown keys are ignored and out-of-range numbers are clamped: a bad
// stored value must never make the tool unusable, it must fall back.
//
// This is a pure function so it can be tested exhaustively without a store.
func ResolveConfig(values map[string]string, dataDir string) Config {
	defaults := ConfigDefaults(dataDir)
	get := func(key string) string {
		if v, ok := values[key]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return defaults[key]
	}

	cfg := Config{
		OutputDir:           get(KeyOutputDir),
		Workers:             clampInt(parseIntOr(get(KeyWorkers), 4), minWorkers, maxWorkers),
		VerifyTLS:           parseBoolOr(get(KeyVerifyTLS), true),
		ProxyMode:           normaliseProxyMode(get(KeyProxyMode)),
		ProxyURL:            get(KeyProxyURL),
		DefaultMirror:       get(KeyDefaultMirror),
		DefaultSearchSource: get(KeyDefaultSearchSource),
		ChunkThresholdMB:    clampInt(parseIntOr(get(KeyChunkThresholdMB), 50), minChunkMB, maxChunkMB),
		MaxRetries:          clampInt(parseIntOr(get(KeyMaxRetries), 10), minRetries, maxRetries),
	}

	// Fall back to the shipped defaults when a stored value points at
	// something that no longer exists (an edited-away mirror or source).
	//
	// default_mirror accepts EITHER a built-in mirror id or a raw registry
	// host. The host form is what makes the GUI's 设为默认 button work for a
	// user-added mirror (an internal Harbor, say): previously only the
	// built-in ids were accepted, so marking a custom row as default was
	// stored and then silently ignored — downloads kept going to
	// registry-1.docker.io.
	if !isMirrorIDOrHost(cfg.DefaultMirror) {
		cfg.DefaultMirror = registry.DefaultMirror().ID
	}
	if !isBuiltinSearchSource(cfg.DefaultSearchSource) {
		cfg.DefaultSearchSource = registry.DefaultSearchSourceID
	}
	if strings.TrimSpace(cfg.OutputDir) == "" {
		cfg.OutputDir = defaults[KeyOutputDir]
	}
	if cfg.ProxyMode != ProxyCustom {
		// A URL left over from a previous custom setting must not leak
		// into system/none mode.
		cfg.ProxyURL = ""
	}
	return cfg
}

func isBuiltinSearchSource(id string) bool {
	for _, s := range registry.BuiltinSearchSources {
		if s.ID == id {
			return true
		}
	}
	return false
}

// isMirrorIDOrHost accepts anything registry.PullHost can resolve: a built-in
// mirror id ("1ms"), an upstream-registry id or name ("mcr", "Quay.io (Red
// Hat)"), a search-source name, or a raw host ("harbor.internal:5000"). A bare
// word that is none of those ("mirror-that-was-deleted") is rejected so the
// shipped default applies instead of being dialled as a hostname.
//
// It must accept EXACTLY what PullHost accepts, or a value that passes
// validation would fail to resolve later (or the reverse: a value that works in
// --mirror would be rejected here).
func isMirrorIDOrHost(v string) bool {
	_, ok := registry.PullHost(v)
	return ok
}

func normaliseProxyMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ProxyNone:
		return ProxyNone
	case ProxyCustom:
		return ProxyCustom
	default:
		return ProxySystem
	}
}

func parseIntOr(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

func parseBoolOr(s string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on", "是":
		return true
	case "false", "0", "no", "off", "否":
		return false
	default:
		return fallback
	}
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// ProxyURLForPuller maps the proxy mode onto the value the registry client
// expects: "-" forces a direct connection, "" honours the environment.
func (c Config) ProxyURLForPuller() string {
	switch c.ProxyMode {
	case ProxyNone:
		return "-"
	case ProxyCustom:
		if strings.TrimSpace(c.ProxyURL) == "" {
			return "-"
		}
		return strings.TrimSpace(c.ProxyURL)
	default:
		return ""
	}
}

// MirrorHost resolves a mirror id OR a raw host into a registry host.
// A value that resolves to a known source (accelerator id/name, upstream
// registry id/name, search-source name) wins; a host-shaped value is used as
// typed; anything else is returned unchanged so the caller's own validation or
// the resolver's error is what the user sees. Empty means "the configured
// default mirror".
func (m *Manager) MirrorHost(ctx context.Context, idOrHost string) string {
	v := strings.TrimSpace(idOrHost)
	if v == "" {
		if cfg, err := m.Config(ctx); err == nil {
			v = cfg.DefaultMirror
		}
	}
	if host, ok := registry.PullHost(v); ok {
		return host
	}
	return v
}

// Config loads and resolves the stored configuration.
func (m *Manager) Config(ctx context.Context) (Config, error) {
	return m.config(ctx)
}

// config is the internal loader used by both Config and Start.
func (m *Manager) config(ctx context.Context) (Config, error) {
	rows, err := m.st.ListSettings(ctx)
	if err != nil {
		return Config{}, err
	}
	values := make(map[string]string, len(rows))
	for _, r := range rows {
		values[r.Key] = r.Value
	}
	return ResolveConfig(values, m.dataDir), nil
}

// SaveConfig merges patch into the stored settings. Unknown keys are
// rejected rather than silently stored, so a typo surfaces immediately.
func (m *Manager) SaveConfig(ctx context.Context, patch map[string]string) (Config, error) {
	for key := range patch {
		if !IsSettingKey(key) {
			return Config{}, fmt.Errorf("未知的配置项: %q", key)
		}
	}
	for key, value := range patch {
		if err := m.st.SetSetting(ctx, key, value); err != nil {
			return Config{}, err
		}
	}
	return m.config(ctx)
}

// EnsureConfig writes the defaults for any key that has no stored value,
// and seeds the source tables. Safe to call on every boot.
func (m *Manager) EnsureConfig(ctx context.Context) error {
	rows, err := m.st.ListSettings(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(rows))
	for _, r := range rows {
		have[r.Key] = true
	}
	for key, value := range ConfigDefaults(m.dataDir) {
		if have[key] {
			continue
		}
		if err := m.st.SetSetting(ctx, key, value); err != nil {
			return err
		}
	}
	return m.SeedSources(ctx)
}

// SeedSources inserts the built-in search sources and mirrors that are not
// already stored, so the GUI has something to show on first run and the CLI
// resolves the same ids.
func (m *Manager) SeedSources(ctx context.Context) error {
	existing, err := m.st.ListSources(ctx, "")
	if err != nil {
		return err
	}
	type key struct{ kind, name string }
	have := make(map[key]bool, len(existing))
	for _, s := range existing {
		have[key{s.Kind, s.Name}] = true
	}

	priority := 0
	for _, s := range registry.BuiltinSearchSources {
		k := key{KindSearch, s.Name}
		if have[k] {
			continue
		}
		src := &store.Source{
			ID:        newID(),
			Kind:      KindSearch,
			Name:      s.Name,
			URL:       s.URL,
			Enabled:   true,
			Priority:  priority,
			IsDefault: s.ID == registry.DefaultSearchSourceID,
		}
		if err := m.st.CreateSource(ctx, src); err != nil {
			return err
		}
		have[k] = true
		priority++
	}

	priority = 0
	for _, mir := range registry.BuiltinMirrors {
		k := key{KindMirror, mir.Name}
		if have[k] {
			continue
		}
		src := &store.Source{
			ID:        newID(),
			Kind:      KindMirror,
			Name:      mir.Name,
			Host:      mir.Host,
			Enabled:   true,
			Priority:  priority,
			IsDefault: mir.IsDefault,
		}
		if err := m.st.CreateSource(ctx, src); err != nil {
			return err
		}
		have[k] = true
		priority++
	}
	return nil
}

// Source kinds.
//
// Note there is deliberately no "registry" kind. The public registries in
// registry.BuiltinRegistries are STATIC catalog data served read-only by
// GET /api/registries, not rows: seeding them would duplicate the list, let a
// user "delete" a built-in only to have it reappear on the next boot, and give
// two places for the same fact to drift. A user's own registry (an internal
// Harbor) is added as a mirror row with its host, which already works.
const (
	KindSearch = "search"
	KindMirror = "mirror"
)
