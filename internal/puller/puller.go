package puller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
)

// Defaults for a pull job. They match the tuned values the legacy tool
// converged on, minus the nested thread pools that produced workers²
// simultaneous connections.
const (
	DefaultWorkers        = 4
	DefaultChunkSize      = 10 << 20 // 10 MiB per Range request
	DefaultChunkThreshold = 50 << 20 // blobs above this are chunked
	DefaultMaxRetries     = 10
)

// Options is one pull job. Every field is resolved by the caller (the CLI,
// the HTTP handler, or tasks.Manager) — the puller never prompts.
type Options struct {
	Ref      registry.Ref
	Platform registry.Platform

	// WorkDir holds the partial downloads and the resume ledger. One per
	// task: resuming a job means pointing a new Pull at the same WorkDir.
	WorkDir string

	// OutputDir is where the finished .tar is written. Empty means
	// DefaultOutputDir(WorkDir, ...).
	OutputDir string

	Workers        int
	ChunkSize      int64
	ChunkThreshold int64
	MaxRetries     int

	// Credentials authenticate against the registry.
	Credentials registry.Credentials
	// ProxyURL: "" honours the environment, "-" forces a direct
	// connection, otherwise an explicit proxy URL.
	ProxyURL string
	// InsecureTLS skips certificate verification. Default false: TLS is
	// verified unless the user explicitly opts out.
	InsecureTLS bool

	// Sink receives progress. May be nil.
	Sink Sink
}

// MaxRetriesUnset asks normalize to use DefaultMaxRetries. Zero is a
// meaningful value ("do not retry"), so it cannot double as "unset" — that
// conflation made a configured 0 silently become 10 retries with
// exponential backoff.
const MaxRetriesUnset = -1

// normalize fills in defaults and validates the job.
func (o *Options) normalize() error {
	if o.Ref.IsZero() {
		return fmt.Errorf("镜像信息不完整")
	}
	if o.Platform.IsZero() {
		o.Platform = registry.ParsePlatform("amd64")
	}
	if o.Platform.OS == "" {
		o.Platform.OS = "linux"
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	if o.Workers > 32 {
		o.Workers = 32
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultChunkSize
	}
	if o.ChunkThreshold <= 0 {
		o.ChunkThreshold = DefaultChunkThreshold
	}
	// Chunking below one chunk is pointless and would loop forever.
	if o.ChunkThreshold < o.ChunkSize {
		o.ChunkThreshold = o.ChunkSize
	}
	if o.MaxRetries == MaxRetriesUnset {
		o.MaxRetries = DefaultMaxRetries
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.WorkDir == "" {
		return fmt.Errorf("WorkDir 不能为空")
	}
	if o.OutputDir == "" {
		o.OutputDir = DefaultOutputDir(o.WorkDir, o.Ref, o.Platform)
	}
	return nil
}

// ClientFor builds the registry client a job with these options would use.
// Exported so callers (the inspect endpoint, the CLI's --list-arch) talk to
// the registry with identical proxy/TLS/credential settings.
func ClientFor(o Options) *registry.Client {
	insecure := o.InsecureTLS
	return registry.NewClient(registry.ClientOptions{
		Credentials: o.Credentials,
		ProxyURL:    o.ProxyURL,
		InsecureTLS: insecure,
	})
}

// Result describes a finished job.
type Result struct {
	TarPath   string        `json:"tarPath"`
	Size      int64         `json:"size"`
	Bytes     int64         `json:"bytes"`
	Duration  time.Duration `json:"duration"`
	Layers    int           `json:"layers"`
	Completed int           `json:"completed"` // blobs satisfied without downloading
	Platform  string        `json:"platform"`
	Warnings  []string      `json:"warnings,omitempty"`
}

// Pull runs one job to completion.
//
// It is resumable: calling Pull again with the same WorkDir continues from
// the recorded offsets, including after a process restart. Cancelling ctx
// stops the job cleanly, leaving the ledger (and therefore the resume
// points) on disk.
func Pull(ctx context.Context, opts Options) (*Result, error) {
	started := time.Now()
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	sink := opts.Sink
	if sink == nil {
		sink = NopSink{}
	}

	client := ClientFor(opts)

	// Resolve the tag/index to a concrete image manifest for the platform.
	manifest, actualPlatform, err := client.ResolveImage(ctx, opts.Ref, opts.Platform)
	if err != nil {
		return nil, err
	}
	if actualPlatform.IsZero() {
		actualPlatform = opts.Platform
	}
	// Record what we actually pulled, not what was asked for: a mirror may
	// serve a differently-spelled variant (arm64v8 for arm64).
	opts.Platform = actualPlatform

	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("清单中没有层")
	}
	if manifest.Config.Digest == "" {
		return nil, fmt.Errorf("清单中没有配置信息")
	}

	blobsDir := filepath.Join(opts.WorkDir, "blobs")
	chunksDir := filepath.Join(opts.WorkDir, "chunks")
	stageDir := filepath.Join(opts.WorkDir, "stage")
	for _, dir := range []string{blobsDir, chunksDir, opts.OutputDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	// Build the work list: the config blob first (it is small and its
	// absence is fatal), then the layers in order.
	specs := make([]blobSpec, 0, len(manifest.Layers)+1)
	specs = append(specs, blobSpec{desc: manifest.Config, kind: KindConfig})
	for _, l := range manifest.Layers {
		specs = append(specs, blobSpec{desc: l, kind: KindLayer})
	}

	state := newState(opts, specs)
	if prev, err := LoadState(opts.WorkDir, opts.Ref, opts.Platform); err == nil && prev != nil {
		carryOver(state, prev)
	}

	ledger := NewLedger(opts.WorkDir, *state)
	job := &job{
		ctx:        ctx,
		opts:       opts,
		client:     client,
		ref:        opts.Ref,
		platform:   opts.Platform,
		ledger:     ledger,
		sink:       sink,
		blobsDir:   blobsDir,
		chunksDir:  chunksDir,
		stageDir:   stageDir,
		lastReport: make(map[string]time.Time),
	}

	st, err := ledger.Update(func(s *State) { s.Status = StatusRunning })
	if err == nil {
		sink.OnState(st)
	}

	// Download every blob, up to Workers at a time. Each blob retries and
	// resumes on its own, so one slow layer cannot stall the others.
	if err := job.downloadAll(ctx, specs); err != nil {
		final := stateAfter(ledger, StatusFailed, err.Error())
		sink.OnState(final)
		return nil, err
	}

	// Everything is verified; build the archive.
	tarPath := filepath.Join(opts.OutputDir, TarName(opts.Ref, opts.Platform))
	tarSize, err := job.assembleTar(manifest, manifest.Config.Digest, tarPath)
	if err != nil {
		final := stateAfter(ledger, StatusFailed, err.Error())
		sink.OnState(final)
		return nil, err
	}

	final := stateAfter(ledger, StatusCompleted, "")
	sink.OnState(final)
	// The job succeeded: its ledger has no further use. The blob files
	// stay, so re-exporting or re-pulling this image needs no network.
	_ = ledger.Remove()

	var bytes int64
	for _, l := range final.Layers {
		bytes += l.Size
	}

	return &Result{
		TarPath:  tarPath,
		Size:     tarSize,
		Bytes:    bytes,
		Duration: time.Since(started),
		Layers:   len(manifest.Layers),
		// Counted from the FINAL state, not the initial one: a blob with a
		// valid .ok marker is only known to be reusable once the downloader
		// has checked it, and a successful previous run deletes its ledger
		// (leaving the markers as the only evidence the bytes are cached).
		Completed: countSettled(final),
		Platform:  opts.Platform.String(),
		Warnings:  job.Warnings(),
	}, nil
}

// downloadAll runs the blob downloads with a bounded worker pool and stops
// the rest as soon as one fails irrecoverably.
func (j *job) downloadAll(ctx context.Context, specs []blobSpec) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := j.opts.Workers
	if workers > len(specs) {
		workers = len(specs)
	}
	if workers < 1 {
		workers = 1
	}

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		sem      = make(chan struct{}, workers)
	)

	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for _, spec := range specs {
		wg.Add(1)
		go func(spec blobSpec) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := j.downloadBlob(ctx, spec); err != nil && ctx.Err() == nil {
				fail(err)
			}
		}(spec)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// newState builds a fresh ledger from the work list.
func newState(opts Options, specs []blobSpec) *State {
	st := &State{
		Ref:       opts.Ref.String(),
		Platform:  opts.Platform.String(),
		Status:    StatusPending,
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Layers:    make([]LayerState, 0, len(specs)),
	}
	for _, s := range specs {
		st.Layers = append(st.Layers, LayerState{
			Digest: s.desc.Digest,
			Kind:   s.kind,
			Name:   LayerName(s.kind, s.desc.Digest),
			Size:   s.desc.Size,
			Status: LayerPending,
		})
	}
	st.TotalBytes = st.SumSizes()
	return st
}

// carryOver copies resume progress from a previous ledger onto the fresh
// one, but only for blobs that are still part of the image. A tag that
// moved to a new digest therefore loses exactly the stale entries.
//
// Layers the previous run finished are marked LayerSkipped rather than
// LayerPending so the caller (and Result.Completed) can report "N layers
// reused"; the downloader still re-checks the .ok marker before trusting
// the bytes, so a blob deleted behind our back is re-fetched either way.
func carryOver(dst, prev *State) {
	for i := range dst.Layers {
		j := prev.IndexOf(dst.Layers[i].Digest)
		if j < 0 || prev.Layers[j].Kind != dst.Layers[i].Kind {
			continue
		}
		old := prev.Layers[j]
		dst.Layers[i].Downloaded = old.Downloaded
		dst.Layers[i].Chunks = old.Chunks
		if old.Size > 0 {
			dst.Layers[i].Size = old.Size
		}
		if old.Status == LayerCompleted || old.Status == LayerSkipped {
			dst.Layers[i].Status = LayerSkipped
			if old.Size > 0 {
				dst.Layers[i].Downloaded = old.Size
			}
			continue
		}
		dst.Layers[i].Status = LayerPending
	}
	dst.TotalBytes = dst.SumSizes()
	dst.DownloadedBytes = sumDownloaded(dst.Layers)
}

// SumSizes totals the blobs' known sizes.
func (s *State) SumSizes() int64 {
	var total int64
	for _, l := range s.Layers {
		total += l.Size
	}
	return total
}

// countSettled counts blobs that a previous run already satisfied, i.e.
// those the puller reported as skipped.
func countSettled(st State) int {
	var n int
	for _, l := range st.Layers {
		if l.Status == LayerCompleted || l.Status == LayerSkipped {
			n++
		}
	}
	return n
}

func stateAfter(ledger *Ledger, status, errMsg string) State {
	st, _ := ledger.Update(func(s *State) {
		s.Status = status
		if errMsg != "" {
			s.Error = errMsg
		}
		s.DownloadedBytes = sumDownloaded(s.Layers)
	})
	return st
}

// ScanPlatforms resolves the platforms an image publishes, so the caller
// can render an architecture picker.
func ScanPlatforms(ctx context.Context, opts Options) ([]registry.Platform, error) {
	client := ClientFor(opts)
	platforms, _, err := client.ScanPlatforms(ctx, opts.Ref)
	if err != nil {
		return nil, err
	}
	return platforms, nil
}

// Inspect is the resolved description of an image, used by the GUI's
// tag/arch picker and the CLI's --list-arch.
type Inspect struct {
	Ref        registry.Ref
	MediaType  string
	IsIndex    bool
	Platforms  []registry.Platform
	TotalBytes int64
	LayerCount int
	// Tags come from the registry being inspected, NOT from a search API, so
	// the picker offers tags the download can actually have. Best-effort: a
	// registry without tags/list leaves it empty and the caller may fall back
	// to a searcher.
	Tags     []string
	TagsFrom string
}

// ListTags reports the tags the registry itself has for the reference's
// repository, and the host that answered.
//
// It deliberately does NOT resolve a manifest. InspectImage does, which means
// `tags -i host/repo` inherited the requirement that the ref's tag exist — so
// listing the tags of a repository whose `latest` is absent failed with
// "manifest unknown: unknown tag=latest" instead of listing what IS there. A
// tag listing is about the repository, not about one tag.
func ListTags(ctx context.Context, opts Options) ([]string, string, error) {
	client := ClientFor(opts)
	tags, err := client.ListTags(ctx, opts.Ref)
	if err != nil {
		return nil, "", err
	}
	return tags, opts.Ref.Registry, nil
}

// InspectImage fetches an image's manifest chain and reports what is
// available without downloading any layer.
func InspectImage(ctx context.Context, opts Options) (*Inspect, error) {
	client := ClientFor(opts)
	platforms, manifest, err := client.ScanPlatforms(ctx, opts.Ref)
	if err != nil {
		return nil, err
	}
	out := &Inspect{
		Ref:       opts.Ref,
		MediaType: manifest.MediaType,
		IsIndex:   manifest.IsIndex,
		Platforms: platforms,
	}
	if !manifest.IsIndex {
		out.LayerCount = len(manifest.Layers)
		out.TotalBytes = manifest.TotalLayerBytes()
	}
	// Tags are a nice-to-have: an arch list is still useful without them, so a
	// registry that cannot list tags must not fail the inspect.
	if tags, err := client.ListTags(ctx, opts.Ref); err == nil {
		out.Tags = tags
		out.TagsFrom = opts.Ref.Registry
	}
	return out, nil
}

// FormatBytes renders a byte count for humans (1024-based), matching the
// legacy tool's "1.0MB" style.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f%s", value, u)
		}
	}
	return fmt.Sprintf("%.1fEB", value/unit)
}

// FormatDuration renders an elapsed time the way the legacy tool's
// summary line did: "30秒", "1分30秒", "1小时1分".
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return "0秒"
	}
	secs := int(d.Seconds())
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%d小时%d分", h, m)
	case m > 0:
		return fmt.Sprintf("%d分%d秒", m, s)
	default:
		return fmt.Sprintf("%d秒", s)
	}
}

// CleanWorkDir removes a task's working directory entirely, including the
// blob cache. Used by "retry from scratch" and by task deletion.
func CleanWorkDir(workDir string) error {
	if workDir == "" {
		return nil
	}
	return os.RemoveAll(workDir)
}
