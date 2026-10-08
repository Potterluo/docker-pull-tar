// Package puller downloads a Docker image and writes a `docker load`
// compatible tar archive.
//
// The piece that matters most is resumability: every byte that reaches disk
// is recorded, so an interrupted, crashed or killed run continues from where
// it stopped — including across a process restart — instead of starting the
// layer over. See CONTRACT.md §3 for the exact guarantees.
package puller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
)

// Job statuses recorded in progress.json and reported to the Sink.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Per-blob statuses.
const (
	LayerPending     = "pending"
	LayerDownloading = "downloading"
	LayerCompleted   = "completed"
	LayerFailed      = "failed"
	LayerSkipped     = "skipped"
)

// Blob kinds.
const (
	KindLayer  = "layer"
	KindConfig = "config"
)

// ProgressFileName is the resume ledger inside a task's WorkDir.
const ProgressFileName = "progress.json"

// Sink receives progress callbacks. Implementations must be safe for
// concurrent use, because the puller calls them from several goroutines.
type Sink interface {
	// OnState is called when a status changes or a size is discovered —
	// low frequency (a handful per blob).
	OnState(st State)
	// OnProgress is called often, with byte counters only.
	OnProgress(digest string, downloaded int64)
}

// NopSink discards every callback.
type NopSink struct{}

// OnState implements Sink.
func (NopSink) OnState(State) {}

// OnProgress implements Sink.
func (NopSink) OnProgress(string, int64) {}

// ChunkState is one Range request's slice of a large blob. The ledger of
// these is what makes a chunked download resumable at sub-blob granularity.
type ChunkState struct {
	Index      int   `json:"index"`
	Start      int64 `json:"start"`
	End        int64 `json:"end"` // inclusive
	Done       bool  `json:"done"`
	Downloaded int64 `json:"downloaded"`
}

// LayerState is one blob's durable state.
type LayerState struct {
	Digest     string `json:"digest"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`

	Chunks []ChunkState `json:"chunks,omitempty"`
}

// State is the whole job's durable state. It is written to
// <WorkDir>/progress.json after every material change and reported to the
// Sink.
type State struct {
	// Ref and Platform identify the job. A ledger whose identity does not
	// match the requested image is discarded, never reused: resuming into
	// the wrong image would produce a corrupt tar.
	Ref      string `json:"ref"`
	Platform string `json:"platform"`

	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	TotalBytes      int64 `json:"totalBytes"`
	DownloadedBytes int64 `json:"downloadedBytes"`

	Layers []LayerState `json:"layers"`

	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Clone deep-copies the state so a Sink cannot mutate the puller's copy.
func (s *State) Clone() State {
	out := *s
	out.Layers = make([]LayerState, len(s.Layers))
	for i, l := range s.Layers {
		out.Layers[i] = l
		if l.Chunks != nil {
			out.Layers[i].Chunks = append([]ChunkState(nil), l.Chunks...)
		}
	}
	return out
}

// IndexOf finds a layer by digest, or -1.
func (s *State) IndexOf(digest string) int {
	for i := range s.Layers {
		if s.Layers[i].Digest == digest {
			return i
		}
	}
	return -1
}

// Ledger serializes state changes and writes progress.json atomically.
// Save is safe to call from concurrent blob workers.
type Ledger struct {
	mu   sync.Mutex
	path string
	st   State
}

// NewLedger opens (or creates) the ledger at <workDir>/progress.json.
func NewLedger(workDir string, st State) *Ledger {
	return &Ledger{
		path: filepath.Join(workDir, ProgressFileName),
		st:   st,
	}
}

// State returns a deep copy of the current state.
func (l *Ledger) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st.Clone()
}

// Update mutates the state under the lock and persists it. The callback
// must not call back into the ledger.
func (l *Ledger) Update(fn func(*State)) (State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn(&l.st)
	l.st.UpdatedAt = time.Now().UTC()
	snapshot := l.st.Clone()
	return snapshot, l.writeLocked()
}

func (l *Ledger) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&l.st, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file in the same directory, then rename: a crash
	// mid-write must never leave a half-parsed ledger behind.
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Remove deletes the ledger. Called once a job succeeds.
func (l *Ledger) Remove() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := os.Remove(l.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadState reads a job ledger and returns it only when it belongs to the
// requested image and platform. A missing, unreadable, corrupt or
// mismatched ledger yields (nil, nil): the caller starts fresh, which is
// always safe, whereas reusing foreign state is not.
func LoadState(workDir string, ref registry.Ref, platform registry.Platform) (*State, error) {
	path := filepath.Join(workDir, ProgressFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		// Corrupt ledger: treat as absent rather than blocking the pull.
		return nil, nil
	}
	if st.Ref != ref.String() || st.Platform != platform.String() {
		return nil, nil
	}
	return &st, nil
}

// SaveState writes a state file atomically. Exported for callers that keep
// their own ledger (the CLI's --ci path, tests).
func SaveState(workDir string, st *State) error {
	l := NewLedger(workDir, *st)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeLocked()
}

// sanitizeDigest turns "sha256:abc" into a filesystem-safe name.
func sanitizeDigest(digest string) string {
	return strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(digest)
}

// shortDigest renders a digest for display.
func shortDigest(digest string) string {
	s := strings.TrimPrefix(digest, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "blob"
	}
	return s
}

// LayerName is the display name for a blob: a short digest, or "Config" for
// the image config (matching what the legacy UI showed).
func LayerName(kind, digest string) string {
	if kind == KindConfig {
		return "Config"
	}
	return shortDigest(digest)
}

// ValidateState reports why a ledger is unusable, for diagnostics.
func ValidateState(st *State) error {
	if st == nil {
		return fmt.Errorf("空进度文件")
	}
	if st.Ref == "" {
		return fmt.Errorf("进度文件缺少镜像信息")
	}
	return nil
}
