// Package store is the persistence layer: a single Store interface with
// two drivers (SQLite via modernc.org/sqlite — pure Go, no CGO — and
// PostgreSQL via lib/pq).
//
// Design rules this project follows:
//
//   - One interface, defined in terms of plain record structs, split into
//     per-domain sub-interfaces embedded into Store. Handlers never touch
//     *sql.DB; tests can fake the interface.
//   - Queries are written once with `?` placeholders; rebind() rewrites
//     them to $n for postgres so there is exactly one copy of each
//     statement.
//   - Schema lives in migrationSQL() as idempotent CREATE TABLE IF NOT
//     EXISTS statements. Column additions to existing installs go through
//     imperative migrate* steps guarded by tableHasColumn.
//   - Missing rows surface as ErrNotFound, never sql.ErrNoRows.
//
// There is no user, session or API-key table: DockerPull is a single-user
// local tool, so every row on this machine belongs to the same operator
// and no ownership column exists.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Get*/lookup methods when no row matches.
var ErrNotFound = errors.New("not found")

// ErrDuplicate is returned when an INSERT violates a UNIQUE constraint.
// Drivers report this differently; the store normalizes it so handlers can
// map it to a 409 cleanly.
var ErrDuplicate = errors.New("duplicate")

// Task is one download job. The registry columns are the *resolved* pull
// options, so a task can be resumed or retried without re-parsing the
// user's original string.
type Task struct {
	ID         string `json:"id"`
	Ref        string `json:"ref"` // "registry/repository:tag"
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	Platform   string `json:"platform"` // "linux/amd64"

	Status string `json:"status"`
	Error  string `json:"error"`

	TotalBytes      int64   `json:"totalBytes"`
	DownloadedBytes int64   `json:"downloadedBytes"`
	Speed           float64 `json:"speed"` // bytes/sec, last sampled

	// WorkDir holds the partial downloads and the resume ledger. It is the
	// ONLY path a resume needs: everything else is derived from it.
	WorkDir string `json:"workDir"`
	TarPath string `json:"tarPath"`
	TarSize int64  `json:"tarSize"`

	Workers int `json:"workers"`
	// Insecure is the registry's TRANSPORT: true reaches it over plain HTTP
	// (`--insecure`, needed for a local registry such as localhost:5000).
	// It is per-task state because a resumed job must reach the same
	// registry the same way.
	Insecure bool `json:"insecure"`
	// VerifyTLS is the unrelated CERTIFICATE switch: true means the
	// registry's certificate is checked. It defaults to true, so a caller
	// must pass the setting through rather than rely on a zero value.
	VerifyTLS bool `json:"verifyTls"`

	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

// TaskLayer is one blob of a task: a filesystem layer, or the image config
// blob (Kind "config"). Position is the manifest order, which is also the
// order the layer tars are stacked into the artifact.
type TaskLayer struct {
	ID         string `json:"id"`
	TaskID     string `json:"taskId"`
	Digest     string `json:"digest"`
	Kind       string `json:"kind"` // "layer" | "config"
	Name       string `json:"name"`
	Position   int    `json:"position"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status"` // pending|downloading|completed|failed|skipped
	Error      string `json:"error"`
}

// Source is a configurable search backend (Kind "search") or registry
// mirror (Kind "mirror"). The built-ins are seeded on first boot from
// internal/registry, so the CLI and the GUI agree on the list.
type Source struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // "search" | "mirror"
	Name      string    `json:"name"`
	URL       string    `json:"url"`  // search sources
	Host      string    `json:"host"` // mirror sources
	Enabled   bool      `json:"enabled"`
	Priority  int       `json:"priority"`
	IsDefault bool      `json:"isDefault"`
	CreatedAt time.Time `json:"createdAt"`
}

// Setting is one key/value preference row. The recognised keys and their
// defaults live in internal/tasks (SettingKeys / ConfigDefaults).
type Setting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Artifact is a finished .tar on disk. Path is unique, which is what makes
// the filesystem reconciliation idempotent.
type Artifact struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"` // file name
	Path       string    `json:"path"` // absolute path
	Repository string    `json:"repository"`
	Tag        string    `json:"tag"`
	Platform   string    `json:"platform"`
	Size       int64     `json:"size"`
	TaskID     string    `json:"taskId"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Store is the entire persistence surface. Methods are coarse (one per use
// case) and grouped by domain in one file each:
//
//	tasks.go        TaskStore        tasks + their layers
//	sources.go      SourceStore      search sources and mirrors
//	settings.go     SettingStore     key/value preferences
//	artifacts.go    ArtifactStore    finished .tar files
//	credentials.go  CredentialStore  saved registry logins (sealed secrets)
type Store interface {
	Close() error

	// Downloads and their app-level state.
	TaskStore
	SourceStore
	SettingStore
	ArtifactStore
	CredentialStore

	// Generated entities embed their Store interfaces here — see
	// `generator entity` (cmd/generator). Each entity file defines a
	// <Name>Store interface + the DBStore methods implementing it.
	// --- gen:store-interfaces ---
}

// Compile-time assertion that the sql driver implements the whole surface.
var _ Store = (*DBStore)(nil)

// ctxGuard is a small helper for the few call sites that need a non-nil
// context without threading one through.
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
