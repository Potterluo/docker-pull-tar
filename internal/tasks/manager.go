// Package tasks owns a download's lifecycle: creating the task row,
// running the puller, mirroring progress into the store, fanning events out
// on the shared hub, and reconciling finished archives with the filesystem.
//
// It is the single place both front ends go through — the CLI's `pull`
// subcommand and the HTTP API both call Manager.Start, so a task started
// from the terminal shows up in the GUI with live progress.
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/events"
	"github.com/Potterluo/docker-pull-tar/internal/puller"
	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
)

// ErrInvalidImage marks a request whose image reference could not be parsed.
// It is a typed sentinel so the HTTP layer can answer 400 without matching
// on message substrings (which mis-filed every unrecognised parse error as a
// 502 and every directory-creation failure the same way).
var ErrInvalidImage = errors.New("invalid image reference")

// ErrInvalidRegistry marks a request whose download source is not a source this
// build knows and not host-shaped either. It is separate from ErrInvalidImage
// because the two produce different advice ("check the image name" vs "use a
// source id such as 1ms/mcr/quay, or a host such as registry.example.com:5000"),
// and because both are the caller's mistake and therefore a 400.
var ErrInvalidRegistry = errors.New("invalid download source")

// Task statuses.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Event types published on the hub (CONTRACT §5).
const (
	EventTaskCreated     = "task.created"
	EventTaskUpdated     = "task.updated"
	EventTaskProgress    = "task.progress"
	EventTaskFinished    = "task.finished"
	EventArtifactCreated = "artifact.created"
	EventArtifactDeleted = "artifact.deleted"
	EventSourceChanged   = "source.changed"
	EventSettingsChanged = "settings.changed"
)

// progressInterval bounds how often a running task's byte counters are
// written to the store and pushed over SSE. Layer counts and speeds change
// continuously; the UI does not need 500 updates a second.
const progressInterval = 300 * time.Millisecond

// Task is the wire-facing view: the stored row plus its layers.
type Task struct {
	store.Task
	Layers []store.TaskLayer `json:"layers"`
}

// TaskProgress is the compact progress payload sent on task.progress.
type TaskProgress struct {
	ID              string               `json:"id"`
	Status          string               `json:"status"`
	DownloadedBytes int64                `json:"downloadedBytes"`
	TotalBytes      int64                `json:"totalBytes"`
	Speed           float64              `json:"speed"`
	Layers          []LayerProgressBrief `json:"layers"`
}

// LayerProgressBrief is one layer's byte counters inside TaskProgress.
type LayerProgressBrief struct {
	Digest     string `json:"digest"`
	Downloaded int64  `json:"downloaded"`
	Size       int64  `json:"size"`
	Status     string `json:"status"`
}

// StartSpec is a resolved request to start a pull.
type StartSpec struct {
	Image    string
	Registry string
	Platform string
	Workers  int

	// Insecure is the registry's *transport*: true reaches it over plain
	// HTTP (the legacy `--insecure`, needed for localhost:5000). It is
	// stored on the row so a resumed job reaches the same registry the
	// same way.
	Insecure bool

	// SkipVerifyTLS turns certificate verification OFF for this pull.
	//
	// It is deliberately a negative flag: the zero value then means "verify
	// certificates", which is the safe default, and the stored
	// `verify_tls` setting is always the baseline. Making it a positive
	// `VerifyTLS bool` meant the zero value silently disabled verification,
	// and a caller who set nothing got the opposite of what they asked for.
	SkipVerifyTLS bool

	OutputDir string

	Credentials registry.Credentials

	// ProxyURL overrides the stored proxy for this pull. Empty means "use
	// the `proxy_mode` / `proxy_url` settings", which is the common case —
	// so there is no separate "use stored" flag to get wrong.
	ProxyURL string
}

// Manager orchestrates downloads.
type Manager struct {
	st      store.Store
	hub     *events.Hub
	dataDir string
	version string

	mu      sync.Mutex
	running map[string]*run

	wg sync.WaitGroup
}

// run is the live state of one executing task.
//
// paused is atomic because Pause/Cancel/Delete set it from an HTTP handler
// goroutine while the worker reads it after observing its cancellation.
// Ordering happened to hold before (the write preceded cancel()), but a pull
// that returns for a non-cancellation reason has no happens-before edge at
// all, so the field is explicitly synchronised rather than relying on luck.
//
// done is closed when the worker has finished its final bookkeeping, which
// is what lets Delete remove files without racing the puller that would
// otherwise recreate them.
type run struct {
	cancel context.CancelFunc
	done   chan struct{}
	paused atomic.Bool

	speedMu  sync.Mutex
	lastByte int64
	lastAt   time.Time
	speed    float64
}

// NewManager wires a manager. hub is the process-wide hub created by
// internal/app.Boot — never construct a second one.
func NewManager(st store.Store, hub *events.Hub, dataDir, version string) *Manager {
	return &Manager{
		st:      st,
		hub:     hub,
		dataDir: dataDir,
		version: version,
		running: make(map[string]*run),
	}
}

// DataDir returns the manager's data directory.
func (m *Manager) DataDir() string { return m.dataDir }

// Version returns the build version the manager reports.
func (m *Manager) Version() string { return m.version }

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to a
		// timestamp so the code cannot panic on an exotic platform.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Start resolves the spec, persists a task row, launches the job in the
// background and returns immediately.
func (m *Manager) Start(ctx context.Context, spec StartSpec) (*Task, error) {
	cfg, err := m.config(ctx)
	if err != nil {
		return nil, err
	}

	// Registry resolution: an explicit host in the image string always
	// wins; the selected registry/mirror fills in the default.
	custom := strings.TrimSpace(spec.Registry)
	if custom == "" {
		custom = m.MirrorHost(ctx, "")
	} else if host, ok := registry.PullHost(custom); ok {
		custom = host
	} else {
		// A value that is neither a known source nor host-shaped. Letting it
		// through turned a typo into a DNS lookup hours later ("lookup notahost:
		// no such host"); the same value is rejected by the settings validator,
		// so refusing it here keeps --mirror, the API field and the setting
		// consistent. ErrInvalidImage is the sentinel the server maps to 400.
		return nil, fmt.Errorf("%w: %q（可用 ID 如 1ms/mcr/quay，或直接写 host 如 registry.example.com:5000）",
			ErrInvalidRegistry, custom)
	}

	ref, err := registry.Parse(spec.Image, custom)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidImage, err.Error())
	}
	// Plain-HTTP transport is per-task state: a resumed job has to reach
	// the same registry the same way, so it is stored on the row rather
	// than passed only to the current attempt.
	ref.UseHTTP = spec.Insecure
	platform := registry.ParsePlatform(spec.Platform)

	workers := spec.Workers
	if workers <= 0 {
		workers = cfg.Workers
	}
	workers = clampInt(workers, minWorkers, maxWorkers)

	// Certificate verification: the stored setting is the baseline, and
	// SkipVerifyTLS can only turn it off. There is no way for a zero-valued
	// StartSpec to weaken TLS, by construction.
	verifyTLS := cfg.VerifyTLS
	if spec.SkipVerifyTLS {
		verifyTLS = false
	}

	proxyURL := strings.TrimSpace(spec.ProxyURL)
	if proxyURL == "" {
		proxyURL = cfg.ProxyURLForPuller()
	}

	outputDir := spec.OutputDir
	if strings.TrimSpace(outputDir) == "" {
		outputDir = cfg.OutputDir
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}

	id := newID()
	workDir := filepath.Join(m.dataDir, "work", id)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建工作目录失败: %w", err)
	}

	now := time.Now().UTC()
	task := &store.Task{
		ID:         id,
		Ref:        ref.String(),
		Registry:   ref.Registry,
		Repository: ref.Repository,
		Tag:        ref.TagOrDefault(),
		Digest:     ref.Digest,
		Platform:   platform.String(),
		Status:     StatusPending,
		WorkDir:    workDir,
		Workers:    workers,
		Insecure:   spec.Insecure,
		VerifyTLS:  verifyTLS,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := m.st.CreateTask(ctx, task); err != nil {
		return nil, err
	}

	// Credentials are resolved here and deliberately stay OUT of the store and
	// out of every event payload: they are secrets, and a task row is read by
	// the UI, exported in `--json` and logged.
	//
	// An explicit per-invocation credential always wins. Otherwise a stored
	// login for this registry is used, which is what makes "dockerpull login
	// ghcr.io" then "dockerpull pull ghcr.io/private/x" work with no flags.
	// A missing entry is normal (public images) and must not fail the pull; an
	// UNREADABLE one must, because silently pulling anonymously would surface
	// as a confusing 401 later.
	creds, _, err := m.CredentialsFor(ctx, ref.Registry, spec.Credentials)
	if err != nil {
		return nil, err
	}

	opts := puller.Options{
		Ref:            ref,
		Platform:       platform,
		WorkDir:        workDir,
		OutputDir:      outputDir,
		Workers:        workers,
		MaxRetries:     cfg.MaxRetries,
		Credentials:    creds,
		ProxyURL:       proxyURL,
		InsecureTLS:    !verifyTLS,
		ChunkThreshold: int64(cfg.ChunkThresholdMB) << 20,
	}

	out, err := m.toTask(ctx, task)
	if err != nil {
		return nil, err
	}
	m.publish(EventTaskCreated, out)

	m.launch(task, opts)
	return out, nil
}

// launch runs the job in the background.
func (m *Manager) launch(task *store.Task, opts puller.Options) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, done: make(chan struct{}), lastAt: time.Now()}

	m.mu.Lock()
	if prev, ok := m.running[task.ID]; ok && prev.cancel != nil {
		prev.cancel()
	}
	m.running[task.ID] = r
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(r.done)
		defer func() {
			m.mu.Lock()
			// Only clear the entry if it is still ours: a Retry may have
			// already installed a replacement.
			if cur, ok := m.running[task.ID]; ok && cur == r {
				delete(m.running, task.ID)
			}
			m.mu.Unlock()
			cancel()
		}()

		m.execute(ctx, task, opts, r)
	}()
}

// execute performs one pull and records its outcome.
func (m *Manager) execute(ctx context.Context, task *store.Task, opts puller.Options, r *run) {
	// Re-read the row: a Resume may have changed the working directory or
	// the output directory since the task was created.
	current, err := m.st.GetTask(ctx, task.ID)
	if err != nil {
		slog.Error("task: reload", "task", task.ID, "err", err)
		return
	}
	current.Status = StatusRunning
	if current.StartedAt.IsZero() {
		current.StartedAt = time.Now().UTC()
	}
	current.Error = ""
	if err := m.st.UpdateTask(ctx, current); err != nil {
		slog.Error("task: mark running", "task", task.ID, "err", err)
	}
	m.publishTask(ctx, current.ID, EventTaskUpdated)

	sink := &storeSink{mgr: m, taskID: current.ID, run: r}
	opts.Sink = sink

	// The pull happens on the caller's background context so that
	// cancelling the HTTP request that started it does not kill the
	// download; the manager's own cancel is what stops it.
	result, pullErr := puller.Pull(ctx, opts)

	final, gerr := m.st.GetTask(context.Background(), current.ID)
	if gerr != nil {
		slog.Error("task: reload final", "task", current.ID, "err", gerr)
		return
	}

	switch {
	case pullErr == nil && result != nil:
		final.Status = StatusSucceeded
		final.TarPath = result.TarPath
		final.TarSize = result.Size
		final.TotalBytes = result.Bytes
		final.DownloadedBytes = result.Bytes
		final.Error = ""
		for _, w := range result.Warnings {
			slog.Warn("task: puller warning", "task", current.ID, "warning", w)
		}
	case errors.Is(pullErr, context.Canceled):
		if r.paused.Load() {
			final.Status = StatusPaused
		} else {
			final.Status = StatusCanceled
		}
		final.Error = ""
	default:
		final.Status = StatusFailed
		final.Error = pullErr.Error()
	}

	final.FinishedAt = time.Now().UTC()
	final.Speed = 0
	if err := m.st.UpdateTask(context.Background(), final); err != nil {
		slog.Error("task: finalize", "task", final.ID, "err", err)
	}

	// Mirror the final layer states so the UI's last frame is accurate.
	if layers, err := m.st.ListTaskLayers(context.Background(), final.ID); err == nil {
		for i := range layers {
			if layers[i].Status == puller.LayerDownloading {
				layers[i].Status = puller.LayerPending
				_ = m.st.UpdateTaskLayer(context.Background(), &layers[i])
			}
		}
	}

	if final.Status == StatusSucceeded && final.TarPath != "" {
		art, err := m.recordArtifact(context.Background(), final)
		if err != nil {
			slog.Error("task: record artifact", "task", final.ID, "err", err)
		} else {
			m.publish(EventArtifactCreated, art)
		}
	}

	out, _ := m.toTask(context.Background(), final)
	m.publish(EventTaskUpdated, out)
	m.publish(EventTaskFinished, map[string]any{
		"id":      final.ID,
		"status":  final.Status,
		"tarPath": final.TarPath,
		"error":   final.Error,
	})
}

// storeSink bridges puller progress into the store and the event hub.
type storeSink struct {
	mgr    *Manager
	taskID string
	run    *run

	mu       sync.Mutex
	lastPush time.Time
	layers   []store.TaskLayer
}

// OnState implements puller.Sink. It is the authority on the task's layer
// list: the puller knows which blobs the image actually has.
func (s *storeSink) OnState(st puller.State) {
	ctx := context.Background()

	layers := make([]store.TaskLayer, 0, len(st.Layers))
	for i, l := range st.Layers {
		layers = append(layers, store.TaskLayer{
			ID:         layerID(s.taskID, l.Digest),
			TaskID:     s.taskID,
			Digest:     l.Digest,
			Kind:       l.Kind,
			Name:       l.Name,
			Position:   i,
			Size:       l.Size,
			Downloaded: l.Downloaded,
			Status:     l.Status,
			Error:      l.Error,
		})
	}

	s.mu.Lock()
	s.layers = layers
	s.mu.Unlock()

	if err := s.mgr.st.ReplaceTaskLayers(ctx, s.taskID, layers); err != nil {
		slog.Error("task: persist layers", "task", s.taskID, "err", err)
		return
	}

	total := st.TotalBytes
	if total == 0 {
		total = st.SumSizes()
	}
	s.mgr.updateProgress(ctx, s.taskID, s.run, st.DownloadedBytes, total, st.Status == puller.StatusFailed)

	// Publish the FULL task, never a partial map.
	//
	// CONTRACT §5 and the client both define task.updated as a complete Task,
	// and the /tasks page REPLACES the row when it arrives. A five-field
	// payload therefore blanked out 镜像 / 架构 / 创建时间 on every layer
	// transition until the next listTasks poll restored them.
	s.mgr.publishTask(ctx, s.taskID, EventTaskUpdated)
}

// OnProgress implements puller.Sink. The puller already throttles per
// digest; this adds a task-wide throttle so the store write rate stays
// bounded regardless of how many layers are in flight.
func (s *storeSink) OnProgress(digest string, downloaded int64) {
	ctx := context.Background()

	s.mu.Lock()
	changed := false
	for i := range s.layers {
		if s.layers[i].Digest != digest {
			continue
		}
		if downloaded > s.layers[i].Downloaded {
			s.layers[i].Downloaded = downloaded
			changed = true
		}
		break
	}
	if !changed {
		s.mu.Unlock()
		return
	}
	snapshot := append([]store.TaskLayer(nil), s.layers...)

	// updateProgress owns the throttle; peek at it under the sink lock so
	// concurrent layer callbacks do not each take a turn.
	due := time.Since(s.lastPush) >= progressInterval
	if due {
		s.lastPush = time.Now()
	}
	s.mu.Unlock()

	var downloadedTotal, sizeTotal int64
	for _, l := range snapshot {
		downloadedTotal += l.Downloaded
		sizeTotal += l.Size
	}

	if due {
		// Persist only on a due tick, and only the byte counters: the
		// layer rows were just written by OnState, and rewriting all of
		// them on every tick would dominate the download.
		if err := s.mgr.updateCounters(ctx, s.taskID, s.run, downloadedTotal, sizeTotal); err != nil {
			slog.Debug("task: update counters", "task", s.taskID, "err", err)
		}
	}

	brief := make([]LayerProgressBrief, 0, len(snapshot))
	for _, l := range snapshot {
		brief = append(brief, LayerProgressBrief{
			Digest:     l.Digest,
			Downloaded: l.Downloaded,
			Size:       l.Size,
			Status:     l.Status,
		})
	}
	s.mgr.publish(EventTaskProgress, TaskProgress{
		ID:              s.taskID,
		Status:          StatusRunning,
		DownloadedBytes: downloadedTotal,
		TotalBytes:      sizeTotal,
		Speed:           s.mgr.speed(s.run, downloadedTotal),
		Layers:          brief,
	})
}

func layerID(taskID, digest string) string {
	return taskID + ":" + digest
}

// speed samples the download rate from byte deltas.
func (m *Manager) speed(r *run, downloaded int64) float64 {
	if r == nil {
		return 0
	}
	r.speedMu.Lock()
	defer r.speedMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(r.lastAt).Seconds()
	if elapsed <= 0 {
		return r.speed
	}
	delta := downloaded - r.lastByte
	r.lastByte = downloaded
	r.lastAt = now
	if delta <= 0 {
		// No progress since the last sample: decay rather than reporting
		// a stale rate, so the UI stops showing a speed once it stalls.
		r.speed *= 0.5
		if r.speed < 1024 {
			r.speed = 0
		}
		return r.speed
	}
	instant := float64(delta) / elapsed
	if r.speed == 0 {
		r.speed = instant
	} else {
		// Exponential moving average: smooth enough to read, responsive
		// enough to notice a stall.
		r.speed = 0.6*r.speed + 0.4*instant
	}
	return r.speed
}

func (m *Manager) updateProgress(ctx context.Context, taskID string, r *run, downloaded, total int64, failed bool) {
	if err := m.updateCounters(ctx, taskID, r, downloaded, total); err != nil {
		slog.Debug("task: update progress", "task", taskID, "err", err)
	}
}

func (m *Manager) updateCounters(ctx context.Context, taskID string, r *run, downloaded, total int64) error {
	t, err := m.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	t.DownloadedBytes = downloaded
	if total > 0 {
		t.TotalBytes = total
	}
	t.Speed = m.speed(r, downloaded)
	t.UpdatedAt = time.Now().UTC()
	return m.st.UpdateTask(ctx, t)
}

// --- lifecycle ----------------------------------------------------------

// lookupRunning returns the live run for a task, or nil.
func (m *Manager) lookupRunning(id string) *run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[id]
}

// Pause stops a running task but keeps its working directory and resume
// ledger, so Resume continues from the same offsets.
func (m *Manager) Pause(ctx context.Context, id string) error {
	t, err := m.st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	r := m.lookupRunning(id)
	if r == nil {
		if t.Status == StatusRunning {
			// A row that claims to be running with no live job is a
			// leftover from a crash. Park it so the user can resume.
			t.Status = StatusPaused
			t.UpdatedAt = time.Now().UTC()
			return m.st.UpdateTask(ctx, t)
		}
		return fmt.Errorf("任务当前不可暂停（状态: %s）", t.Status)
	}
	r.paused.Store(true)
	r.cancel()
	return nil
}

// Cancel stops a running task and marks it canceled. The resume ledger
// stays on disk, so the user can still resume it later.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	t, err := m.st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if r := m.lookupRunning(id); r != nil {
		r.paused.Store(false)
		r.cancel()
		return nil
	}
	if t.Status == StatusPending || t.Status == StatusRunning {
		t.Status = StatusCanceled
		t.UpdatedAt = time.Now().UTC()
		if err := m.st.UpdateTask(ctx, t); err != nil {
			return err
		}
		out, _ := m.toTask(ctx, t)
		m.publish(EventTaskUpdated, out)
	}
	return nil
}

// Resume restarts a paused/failed/canceled task against the same working
// directory, i.e. it continues from the recorded byte offsets.
func (m *Manager) Resume(ctx context.Context, id string) error {
	return m.restart(ctx, id, false)
}

// Retry discards the resume ledger and downloads from byte 0.
func (m *Manager) Retry(ctx context.Context, id string) error {
	return m.restart(ctx, id, true)
}

func (m *Manager) restart(ctx context.Context, id string, fromScratch bool) error {
	t, err := m.st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if m.lookupRunning(id) != nil {
		return fmt.Errorf("任务正在运行中，请先暂停或取消")
	}
	// Resume means "continue where it stopped", which is meaningless for a
	// task that already finished. Retry (fromScratch) is exactly the escape
	// hatch for that case, so it must NOT be blocked here — blocking it made
	// the /tasks 重试 button dead for every successful download.
	if !fromScratch && t.Status == StatusSucceeded {
		return fmt.Errorf("任务已成功完成，如需重新下载请使用“重试”")
	}

	ref, err := registry.Parse(t.Ref, "")
	if err != nil {
		// The stored ref is authoritative; if it no longer parses the row
		// is corrupt and the user has to start a new task.
		return fmt.Errorf("任务记录中的镜像信息无效: %w", err)
	}
	// Restore the transport choice with the reference: resuming an HTTP
	// registry over HTTPS (or vice versa) would fail confusingly.
	ref.UseHTTP = t.Insecure
	platform := registry.ParsePlatform(t.Platform)

	if fromScratch {
		// Remove only the derived state, never the finished artifact: the
		// .ok markers and blob cache are what make a retry expensive to
		// recompute, so they go; the tar the user already has stays until
		// a new one replaces it.
		if err := os.RemoveAll(t.WorkDir); err != nil {
			return fmt.Errorf("清理下载缓存失败: %w", err)
		}
	}
	if err := os.MkdirAll(t.WorkDir, 0o755); err != nil {
		return fmt.Errorf("创建工作目录失败: %w", err)
	}

	// A resumed run must not be blocked by the previous run's terminal
	// state — clear it before the new attempt starts.
	t.Status = StatusPending
	t.Error = ""
	t.Speed = 0
	t.FinishedAt = time.Time{}
	t.UpdatedAt = time.Now().UTC()
	if err := m.st.UpdateTask(ctx, t); err != nil {
		return err
	}
	if err := m.st.ReplaceTaskLayers(ctx, id, nil); err != nil {
		return err
	}

	cfg, err := m.config(ctx)
	if err != nil {
		return err
	}
	outputDir := filepath.Dir(t.TarPath)
	if t.TarPath == "" || !dirExists(outputDir) {
		outputDir = cfg.OutputDir
	}

	// Re-resolve the stored login for this registry. A resumed private pull
	// needs it again, and the task row deliberately does not carry secrets —
	// so resume/retry must look them up exactly like a fresh start does.
	creds, _, err := m.CredentialsFor(ctx, t.Registry, registry.Credentials{})
	if err != nil {
		return err
	}

	opts := puller.Options{
		Ref:            ref,
		Platform:       platform,
		WorkDir:        t.WorkDir,
		OutputDir:      outputDir,
		Workers:        clampInt(t.Workers, minWorkers, maxWorkers),
		MaxRetries:     cfg.MaxRetries,
		Credentials:    creds,
		ProxyURL:       cfg.ProxyURLForPuller(),
		InsecureTLS:    !t.VerifyTLS,
		ChunkThreshold: int64(cfg.ChunkThresholdMB) << 20,
	}

	m.launch(t, opts)
	out, _ := m.toTask(ctx, t)
	m.publish(EventTaskUpdated, out)
	return nil
}

// Delete removes a task. removeFiles also deletes its working directory
// (the blob cache) and any artifact it produced.
func (m *Manager) Delete(ctx context.Context, id string, removeFiles bool) error {
	t, err := m.st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if r := m.lookupRunning(id); r != nil {
		r.paused.Store(false)
		r.cancel()
		// Wait for the worker to stop before touching its files: the puller
		// re-creates its directories (blobs/, chunks/, the ledger's parent)
		// as it runs, so removing them underneath a live worker leaves an
		// orphaned tree that no row references and nothing ever collects.
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			slog.Warn("task: worker did not stop before delete; files may be left behind", "task", id)
		}
	}

	if removeFiles {
		if t.TarPath != "" {
			if err := m.removeArtifactFile(ctx, t.TarPath); err != nil {
				slog.Warn("task: remove artifact", "path", t.TarPath, "err", err)
			}
		}
		if t.WorkDir != "" {
			if err := os.RemoveAll(t.WorkDir); err != nil {
				slog.Warn("task: remove workdir", "dir", t.WorkDir, "err", err)
			}
		}
	}
	return m.st.DeleteTask(ctx, id)
}

// removeArtifactFile deletes a tar and its artifacts row.
func (m *Manager) removeArtifactFile(ctx context.Context, path string) error {
	arts, err := m.st.ListArtifacts(ctx)
	if err != nil {
		return err
	}
	for _, a := range arts {
		if a.Path != path {
			continue
		}
		if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := m.st.DeleteArtifact(ctx, a.ID); err != nil {
			return err
		}
		m.publish(EventArtifactDeleted, map[string]any{"id": a.ID})
	}
	return nil
}

// Shutdown stops every running job, leaving resumable state on disk. It
// reports whether every worker finished its bookkeeping before the deadline.
//
// The return value matters: a false means a worker is still writing to the
// store, so the caller must NOT close it out from under that write. The task
// row is then left mid-flight, which the next boot's ReconcileTasks parks.
func (m *Manager) Shutdown(ctx context.Context) bool {
	m.mu.Lock()
	for id, r := range m.running {
		r.paused.Store(true) // park, do not cancel: the user can resume
		r.cancel()
		delete(m.running, id)
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(5 * time.Second):
		return false
	}
}

// ReconcileTasks parks tasks that a previous process left mid-flight.
//
// A hard kill (taskkill, closing the console, power loss) never runs
// Shutdown, so the row stays "running" with no live job. Without this, the
// /tasks page shows 下载中 forever, the client's 2-second refetch loop stays
// hot forever, and the dashboard's 进行中 counter is permanently inflated.
// Parking (rather than failing) keeps the resume ledger usable.
//
// Returns how many rows were parked. Call it once, at boot, before serving.
func (m *Manager) ReconcileTasks(ctx context.Context) (int, error) {
	rows, err := m.st.ListTasks(ctx, 0)
	if err != nil {
		return 0, err
	}
	parked := 0
	for i := range rows {
		if rows[i].Status != StatusRunning && rows[i].Status != StatusPending {
			continue
		}
		t := rows[i]
		t.Status = StatusPaused
		t.Speed = 0
		t.Error = ""
		t.UpdatedAt = time.Now().UTC()
		if err := m.st.UpdateTask(ctx, &t); err != nil {
			return parked, err
		}
		// Any layer still marked downloading belongs to the dead process.
		layers, err := m.st.ListTaskLayers(ctx, t.ID)
		if err == nil {
			for j := range layers {
				if layers[j].Status == "downloading" {
					layers[j].Status = "pending"
					_ = m.st.UpdateTaskLayer(ctx, &layers[j])
				}
			}
		}
		parked++
	}
	return parked, nil
}

// --- reads --------------------------------------------------------------

// Get returns a task with its layers.
func (m *Manager) Get(ctx context.Context, id string) (*Task, error) {
	t, err := m.st.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.toTask(ctx, t)
}

// List returns tasks, newest first, with their layers.
func (m *Manager) List(ctx context.Context, limit int) ([]Task, error) {
	rows, err := m.st.ListTasks(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(rows))
	for i := range rows {
		t, err := m.toTask(ctx, &rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

func (m *Manager) toTask(ctx context.Context, t *store.Task) (*Task, error) {
	layers, err := m.st.ListTaskLayers(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if layers == nil {
		// Always emit an array: "layers": null makes every client add a
		// null check, and a task that failed before resolving a manifest
		// legitimately has zero layers rather than an unknown number.
		layers = []store.TaskLayer{}
	}
	return &Task{Task: *t, Layers: layers}, nil
}

// Stats summarises the dashboard counters.
func (m *Manager) Stats(ctx context.Context) (map[string]any, error) {
	rows, err := m.st.ListTasks(ctx, 0)
	if err != nil {
		return nil, err
	}
	var running, succeeded, failed int
	var bytesTotal int64
	for _, t := range rows {
		switch t.Status {
		case StatusRunning, StatusPending:
			running++
		case StatusSucceeded:
			succeeded++
		case StatusFailed:
			failed++
		}
		bytesTotal += t.TarSize
	}

	arts, err := m.st.ListArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	var artifactsBytes int64
	for _, a := range arts {
		artifactsBytes += a.Size
	}

	return map[string]any{
		"tasksTotal":     len(rows),
		"tasksRunning":   running,
		"tasksSucceeded": succeeded,
		"tasksFailed":    failed,
		"bytesTotal":     bytesTotal,
		"artifactsTotal": len(arts),
		"artifactsBytes": artifactsBytes,
	}, nil
}

// --- artifacts ----------------------------------------------------------

// ScanArtifacts reconciles the artifacts table with what is on disk in
// outputDir: new .tar files are recorded, rows whose file vanished are
// dropped. Returns the resulting list, newest first.
func (m *Manager) ScanArtifacts(ctx context.Context) ([]store.Artifact, error) {
	cfg, err := m.config(ctx)
	if err != nil {
		return nil, err
	}
	return m.ScanArtifactsIn(ctx, cfg.OutputDir)
}

// ScanArtifactsIn is ScanArtifacts against an explicit directory.
func (m *Manager) ScanArtifactsIn(ctx context.Context, outputDir string) ([]store.Artifact, error) {
	existing, err := m.st.ListArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]store.Artifact, len(existing))
	for _, a := range existing {
		known[filepath.Clean(a.Path)] = a
	}

	found := make(map[string]bool)
	if entries, err := os.ReadDir(outputDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".tar") {
				continue
			}
			full := filepath.Clean(filepath.Join(outputDir, e.Name()))
			found[full] = true
			if _, ok := known[full]; ok {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			art := &store.Artifact{
				ID:        newID(),
				Name:      e.Name(),
				Path:      full,
				Size:      info.Size(),
				CreatedAt: info.ModTime().UTC(),
			}
			art.Repository, art.Tag, art.Platform = parseArtifactName(e.Name())
			if err := m.st.CreateArtifact(ctx, art); err != nil {
				// A concurrent scan may have inserted the same path.
				continue
			}
			known[full] = *art
		}
	}

	// Drop rows whose file is gone (deleted outside the app, or removed by
	// a task deletion that failed halfway).
	for path, a := range known {
		if found[path] {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := m.st.DeleteArtifact(ctx, a.ID); err != nil {
			continue
		}
		delete(known, path)
		m.publish(EventArtifactDeleted, map[string]any{"id": a.ID})
	}

	arts, err := m.st.ListArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(arts, func(i, j int) bool { return arts[i].CreatedAt.After(arts[j].CreatedAt) })
	return arts, nil
}

// recordArtifact adds the tar a finished task produced.
func (m *Manager) recordArtifact(ctx context.Context, t *store.Task) (*store.Artifact, error) {
	info, err := os.Stat(t.TarPath)
	if err != nil {
		return nil, err
	}
	path := filepath.Clean(t.TarPath)
	if existing, err := m.st.ListArtifacts(ctx); err == nil {
		for _, a := range existing {
			if filepath.Clean(a.Path) != path {
				continue
			}
			// The row already exists; a retry just rewrote the file. Refresh
			// it in place — a delete-then-insert would lose the row entirely
			// if the insert failed.
			if a.Size != info.Size() {
				a.Size = info.Size()
				if err := m.st.UpdateArtifact(ctx, &a); err != nil {
					slog.Warn("task: refresh artifact size", "path", path, "err", err)
					return &a, nil
				}
			}
			return &a, nil
		}
	}

	art := &store.Artifact{
		ID:         newID(),
		Name:       filepath.Base(path),
		Path:       path,
		Repository: t.Repository,
		Tag:        t.Tag,
		Platform:   t.Platform,
		Size:       info.Size(),
		TaskID:     t.ID,
		CreatedAt:  info.ModTime().UTC(),
	}
	if err := m.st.CreateArtifact(ctx, art); err != nil {
		return nil, err
	}
	return art, nil
}

// parseArtifactName recovers <repo>_<tag>_<arch>.tar into its parts. The
// split is ambiguous in general (repo, tag and arch may all contain "_"),
// so it is best-effort: the artifact table's task-derived columns are the
// authority, this only fills in rows discovered by scanning.
func parseArtifactName(name string) (repo, tag, arch string) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.Split(base, "_")
	switch {
	case len(parts) >= 3:
		return strings.Join(parts[:len(parts)-2], "_"), parts[len(parts)-2], parts[len(parts)-1]
	case len(parts) == 2:
		return parts[0], parts[1], ""
	default:
		return base, "", ""
	}
}

// --- filesystem helpers -------------------------------------------------

// ValidateArtifactPath guards every download/delete against a path that
// escaped the configured directories. The client supplies an artifact id
// (never a path); this is the belt-and-braces check on top of that.
func ValidateArtifactPath(path string, allowedDirs ...string) error {
	clean := filepath.Clean(path)
	if clean == "" || clean == "." {
		return fmt.Errorf("无效的文件路径")
	}
	for _, dir := range allowedDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		root := filepath.Clean(dir)
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return nil
	}
	return fmt.Errorf("文件路径不在允许的目录内")
}

func dirExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// publish broadcasts an event on the shared hub.
func (m *Manager) publish(eventType string, data any) {
	if m.hub == nil {
		return
	}
	m.hub.Publish(events.Event{Type: eventType, Data: data})
}

// publishTask reloads a task and publishes it.
func (m *Manager) publishTask(ctx context.Context, id, eventType string) {
	t, err := m.st.GetTask(ctx, id)
	if err != nil {
		return
	}
	out, err := m.toTask(ctx, t)
	if err != nil {
		return
	}
	m.publish(eventType, out)
}
