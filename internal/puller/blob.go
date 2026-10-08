package puller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
)

// streamBufferSize is the read buffer for the non-chunked path.
const streamBufferSize = 64 << 10

// errDigestMismatch marks a verification failure so the retry loop knows it
// must discard the partial file (as opposed to a network error, where the
// partial is exactly what makes the retry cheap).
var errDigestMismatch = errors.New("digest mismatch")

// blobSpec is one unit of download work.
type blobSpec struct {
	desc registry.Descriptor
	kind string
}

// job carries the shared state of one Pull invocation.
type job struct {
	ctx      context.Context
	opts     Options
	client   *registry.Client
	ref      registry.Ref
	platform registry.Platform
	ledger   *Ledger
	sink     Sink

	blobsDir  string
	chunksDir string
	stageDir  string

	// reportOnce throttles OnProgress per digest so a fast local mirror
	// cannot flood the SSE stream (or a TUI redraw) with thousands of
	// callbacks per second.
	reportMu   sync.Mutex
	lastReport map[string]time.Time

	// warnMu guards the warnings slice.
	warnMu   sync.Mutex
	warnings []string
}

func (j *job) report(digest string, downloaded int64) {
	j.reportMu.Lock()
	last := j.lastReport[digest]
	now := time.Now()
	if now.Sub(last) < 200*time.Millisecond {
		j.reportMu.Unlock()
		return
	}
	j.lastReport[digest] = now
	j.reportMu.Unlock()

	if j.sink != nil {
		j.sink.OnProgress(digest, downloaded)
	}
}

// warn records a non-fatal notice (a server that ignores Range requests, an
// unknown size) for the caller to surface.
func (j *job) warn(format string, args ...any) {
	j.warnMu.Lock()
	j.warnings = append(j.warnings, fmt.Sprintf(format, args...))
	j.warnMu.Unlock()
}

// Warnings returns the notices collected so far.
func (j *job) Warnings() []string {
	j.warnMu.Lock()
	defer j.warnMu.Unlock()
	return append([]string(nil), j.warnings...)
}

// setLayer mutates one layer's state in the ledger and publishes it.
func (j *job) setLayer(digest string, fn func(*LayerState)) {
	st, err := j.ledger.Update(func(s *State) {
		i := s.IndexOf(digest)
		if i < 0 {
			return
		}
		fn(&s.Layers[i])
		s.DownloadedBytes = sumDownloaded(s.Layers)
	})
	if err == nil && j.sink != nil {
		j.sink.OnState(st)
	}
}

func sumDownloaded(layers []LayerState) int64 {
	var total int64
	for _, l := range layers {
		total += l.Downloaded
	}
	return total
}

func (j *job) blobPath(digest string) string {
	return filepath.Join(j.blobsDir, sanitizeDigest(digest))
}

func (j *job) okPath(digest string) string {
	return j.blobPath(digest) + ".ok"
}

func (j *job) chunkDir(digest string) string {
	return filepath.Join(j.chunksDir, sanitizeDigest(digest))
}

// downloadBlob fetches one blob with resume, retry and verification. ctx is
// the pool's cancelable context, so one failed blob stops its siblings.
func (j *job) downloadBlob(ctx context.Context, spec blobSpec) error {
	digest := spec.desc.Digest
	name := LayerName(spec.kind, digest)
	blobPath := j.blobPath(digest)
	okPath := j.okPath(digest)
	size := spec.desc.Size

	// A blob sealed by an earlier run is trusted: the .ok marker is only
	// written after its digest verified, so re-hashing it on every resume
	// would burn minutes of IO for nothing.
	//
	// "Trusted" still means one cheap sanity check: when the manifest gave us
	// the expected length, a payload of a different length means the marker no
	// longer describes what is on disk (truncated file, half-finished manual
	// copy). Re-downloading is far cheaper than shipping a short layer into
	// the archive and calling it a success — for an UNCOMPRESSED layer
	// nothing downstream would notice the truncation at all.
	if fileExists(okPath) {
		expected := spec.desc.Size
		if fi, err := os.Stat(blobPath); err == nil && fi.Size() > 0 &&
			(expected <= 0 || fi.Size() == expected) {
			j.setLayer(digest, func(l *LayerState) {
				l.Status = LayerSkipped
				l.Size = fi.Size()
				l.Downloaded = fi.Size()
				l.Error = ""
			})
			return nil
		}
		// Marker without a trustworthy payload: drop it and download for real.
		_ = os.Remove(okPath)
	}

	// A foreign layer is never in the registry, so asking it for the size would
	// be a guaranteed 403; the manifest always declares a size for those.
	if size <= 0 && !spec.desc.IsForeign() {
		if n, err := j.client.HeadBlob(ctx, j.ref, digest); err == nil && n > 0 {
			size = n
			name = sizeLabel(digest, size)
		}
	}

	j.setLayer(digest, func(l *LayerState) {
		l.Size = size
		l.Status = LayerDownloading
		l.Error = ""
		if l.Size > 0 && l.Downloaded > l.Size {
			// A previous run left more bytes than the blob has (server
			// changed, or a truncated manifest size). Start clean.
			l.Downloaded = 0
			l.Chunks = nil
		}
	})

	var lastErr error
	for attempt := 0; attempt <= j.opts.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			j.setLayer(digest, func(l *LayerState) { l.Status = LayerPending })
			return err
		}
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(attempt)); err != nil {
				return err
			}
			j.setLayer(digest, func(l *LayerState) {
				l.Status = LayerDownloading
				l.Error = ""
			})
		}

		var err error
		if size >= j.opts.ChunkThreshold && size > 0 {
			err = j.downloadChunked(ctx, spec, blobPath, size)
		} else {
			err = j.downloadStreaming(ctx, spec, blobPath, size)
		}
		if err == nil {
			err = j.seal(digest, blobPath, okPath)
		}
		if err == nil {
			return nil
		}
		lastErr = err

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, errDigestMismatch) {
			// Bad bytes are worse than no bytes: discard and start over.
			_ = os.Remove(blobPath)
			_ = os.RemoveAll(j.chunkDir(digest))
			j.setLayer(digest, func(l *LayerState) {
				l.Chunks = nil
				l.Downloaded = 0
			})
		}
	}

	j.setLayer(digest, func(l *LayerState) {
		l.Status = LayerFailed
		l.Error = lastErr.Error()
	})
	return fmt.Errorf("层 %s 下载失败: %w", name, lastErr)
}

// openRange fetches bytes of a blob from wherever that blob actually lives.
//
// A foreign (non-distributable) layer is NOT in the registry: a Windows base
// image declares its base layers with a `urls` array pointing at Microsoft's
// CDN, and asking /v2/<repo>/blobs/<digest> for one answers 403 AccessDenied.
// So a foreign layer is fetched from its declared URLs; everything else comes
// from the registry as usual. The digest is verified after the download either
// way, so a CDN cannot substitute content.
func (j *job) openRange(ctx context.Context, spec blobSpec, start, end int64) (*registry.BlobReader, error) {
	if urls := spec.desc.ForeignURLs(); len(urls) > 0 {
		return j.client.OpenBlobURLRange(ctx, urls, spec.desc.Digest, start, end)
	}
	return j.client.OpenBlobRange(ctx, j.ref, spec.desc.Digest, start, end)
}

// downloadStreaming writes the blob sequentially into blobPath, resuming
// from the existing file size when there is one.
func (j *job) downloadStreaming(ctx context.Context, spec blobSpec, blobPath string, size int64) error {
	digest := spec.desc.Digest
	var offset int64
	if fi, err := os.Stat(blobPath); err == nil {
		offset = fi.Size()
	}
	if size > 0 && offset > size {
		offset = 0
		_ = os.Remove(blobPath)
	}

	br, err := j.openRange(ctx, spec, offset, -1)
	if err != nil {
		return err
	}
	defer func() { _ = br.Body.Close() }()

	// When the manifest did not tell us how big the blob is and HEAD could not
	// either, the server's own Content-Range is the last source of the total.
	// Without this the progress panel reports 未知 forever even though the
	// response carried the real length.
	if size <= 0 && br.TotalSize > 0 {
		size = br.TotalSize
		j.setLayer(digest, func(l *LayerState) { l.Size = br.TotalSize })
	}

	switch br.StatusCode {
	case 416:
		// The server says our offset is already at the end: the file is
		// complete. Verification decides whether that is true.
		return nil
	case 200:
		if offset > 0 {
			// The server ignored our Range header. Appending now would
			// splice the whole blob onto a partial file and produce
			// garbage that only the checksum would catch — so restart.
			j.warn("%s 服务器不支持断点续传，重新下载完整文件", LayerName(KindLayer, digest))
			offset = 0
		}
	case 206:
		// Resuming exactly where we left off.
	default:
		return fmt.Errorf("意外的响应状态 %d", br.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(blobPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", blobPath, err)
	}

	written := offset
	buf := make([]byte, streamBufferSize)
	for {
		n, rerr := br.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Close()
				return fmt.Errorf("写入 %s 失败: %w", filepath.Base(blobPath), werr)
			}
			written += int64(n)
			j.setProgress(digest, written)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			// Leave the partial file in place: the next attempt resumes.
			return rerr
		}
		if cerr := ctx.Err(); cerr != nil {
			_ = f.Close()
			return cerr
		}
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// downloadChunked splits a large blob into fixed-size Range requests, each
// written to its own file, then merges them. Per-chunk completion lives in
// the ledger, so a restart replays only the missing chunks.
func (j *job) downloadChunked(ctx context.Context, spec blobSpec, blobPath string, size int64) error {
	digest := spec.desc.Digest
	chunkDir := j.chunkDir(digest)
	if err := os.MkdirAll(chunkDir, 0o755); err != nil {
		return err
	}

	chunkSize := j.opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	count := int((size + chunkSize - 1) / chunkSize)

	j.ensureChunks(digest, size, chunkSize, count)
	chunks := j.chunkSnapshot(digest)

	// Reconcile the ledger with what is actually on disk: a chunk is only
	// done when its file has the full expected length.
	for i := range chunks {
		path := chunkFile(chunkDir, chunks[i].Index)
		want := chunks[i].End - chunks[i].Start + 1
		if fi, err := os.Stat(path); err == nil && fi.Size() == want {
			if !chunks[i].Done {
				j.markChunk(digest, chunks[i].Index, true, want)
				chunks[i].Done = true
				chunks[i].Downloaded = want
			}
			continue
		}
		_ = os.Remove(path)
		if chunks[i].Done {
			j.markChunk(digest, chunks[i].Index, false, 0)
			chunks[i].Done = false
			chunks[i].Downloaded = 0
		}
	}

	workers := j.opts.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if workers > count {
		workers = count
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	for i := range chunks {
		if chunks[i].Done {
			continue
		}
		wg.Add(1)
		go func(cs ChunkState) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			var lastErr error
			for attempt := 0; attempt <= j.opts.MaxRetries; attempt++ {
				if err := ctx.Err(); err != nil {
					return
				}
				if attempt > 0 {
					if err := sleepCtx(ctx, backoffFor(attempt)); err != nil {
						return
					}
				}
				n, err := j.fetchChunk(ctx, spec, chunkDir, cs)
				if err == nil {
					j.markChunk(digest, cs.Index, true, n)
					j.report(digest, j.sumChunkProgress(digest))
					return
				}
				lastErr = err
				if errors.Is(err, context.Canceled) {
					return
				}
			}
			fail(fmt.Errorf("分片 %d 下载失败: %w", cs.Index+1, lastErr))
		}(chunks[i])
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return j.mergeChunks(digest, blobPath, chunkDir, chunks)
}

// fetchChunk downloads one chunk into its own file and returns its length.
func (j *job) fetchChunk(ctx context.Context, spec blobSpec, chunkDir string, cs ChunkState) (int64, error) {
	digest := spec.desc.Digest
	target := chunkFile(chunkDir, cs.Index)
	want := cs.End - cs.Start + 1

	br, err := j.openRange(ctx, spec, cs.Start, cs.End)
	if err != nil {
		return 0, err
	}
	defer func() { _ = br.Body.Close() }()

	switch br.StatusCode {
	case 416:
		// The server considers this range unsatisfiable, i.e. its blob is
		// shorter than the manifest claimed. Report it: claiming the chunk is
		// done would record bytes on disk that were never written, and the
		// failure would resurface in mergeChunks as a confusing "cannot read
		// chunk N".
		return 0, fmt.Errorf("分片 %d 无法下载：registry 返回 416，说明服务端 blob 小于清单声称的大小", cs.Index+1)
	case 200:
		// The server ignored Range and sent the whole blob. That is only
		// usable for the first chunk; for any other chunk the bytes would
		// belong to the wrong offset.
		if cs.Start != 0 {
			return 0, fmt.Errorf("服务器不支持 Range 请求，无法分片下载")
		}
	case 206:
		// Expected.
	default:
		return 0, fmt.Errorf("意外的响应状态 %d", br.StatusCode)
	}

	tmp := target + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}

	written, err := io.Copy(f, io.LimitReader(&progressReader{
		r:      br.Body,
		base:   j.chunkBase(digest, cs.Index),
		digest: digest,
		job:    j,
	}, want))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if written != want {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("分片 %d 长度不符: 期望 %d，实际 %d", cs.Index+1, want, written)
	}
	// Rename only once the chunk is whole, so a crash never leaves a
	// short file that the resume scan would mistake for a finished chunk.
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return written, nil
}

// progressReader reports a chunk's byte progress as it streams.
type progressReader struct {
	r      io.Reader
	job    *job
	digest string
	base   int64
	seen   int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.seen += int64(n)
		p.job.report(p.digest, p.base+p.seen)
	}
	return n, err
}

// mergeChunks concatenates the chunk files into the blob and returns the
// final file. Verification happens in seal(), on the merged file.
func (j *job) mergeChunks(digest, blobPath, chunkDir string, chunks []ChunkState) error {
	f, err := os.OpenFile(blobPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	for _, cs := range chunks {
		path := chunkFile(chunkDir, cs.Index)
		in, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("读取分片 %d 失败: %w", cs.Index+1, err)
		}
		want := cs.End - cs.Start + 1
		n, err := io.Copy(f, in)
		_ = in.Close()
		if err != nil {
			return err
		}
		if n != want {
			return fmt.Errorf("分片 %d 长度不符: 期望 %d，实际 %d", cs.Index+1, want, n)
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The merged blob supersedes the chunks.
	_ = os.RemoveAll(chunkDir)
	return nil
}

// seal verifies the blob's sha256 against its digest and, on success,
// writes the .ok marker that lets a later run skip it entirely.
func (j *job) seal(digest, blobPath, okPath string) error {
	actual, err := hashFile(blobPath)
	if err != nil {
		return fmt.Errorf("校验 %s 失败: %w", shortDigest(digest), err)
	}
	if !digestMatches(digest, actual) {
		return fmt.Errorf("%w: %s 期望 %s，实际 %s", errDigestMismatch,
			shortDigest(digest), shortDigest(digest), shortDigest(actual))
	}
	if err := os.WriteFile(okPath, []byte(actual+"\n"), 0o644); err != nil {
		return err
	}
	fi, err := os.Stat(blobPath)
	if err != nil {
		return err
	}
	j.setLayer(digest, func(l *LayerState) {
		l.Status = LayerCompleted
		l.Downloaded = fi.Size()
		l.Size = fi.Size()
		l.Error = ""
		l.Chunks = nil
	})
	return nil
}

// --- ledger helpers for the chunk ledger --------------------------------

func (j *job) ensureChunks(digest string, size, chunkSize int64, count int) {
	j.ledger.Update(func(s *State) {
		i := s.IndexOf(digest)
		if i < 0 {
			return
		}
		l := &s.Layers[i]
		if len(l.Chunks) == count {
			// Shape already matches; keep the recorded progress.
			ok := true
			for k := range l.Chunks {
				if l.Chunks[k].Index != k {
					ok = false
					break
				}
			}
			if ok {
				return
			}
		}
		chunks := make([]ChunkState, count)
		for k := 0; k < count; k++ {
			start := int64(k) * chunkSize
			end := start + chunkSize - 1
			if end > size-1 {
				end = size - 1
			}
			chunks[k] = ChunkState{Index: k, Start: start, End: end}
		}
		l.Chunks = chunks
	})
}

func (j *job) chunkSnapshot(digest string) []ChunkState {
	st := j.ledger.State()
	i := st.IndexOf(digest)
	if i < 0 {
		return nil
	}
	return append([]ChunkState(nil), st.Layers[i].Chunks...)
}

func (j *job) markChunk(digest string, index int, done bool, downloaded int64) {
	j.ledger.Update(func(s *State) {
		i := s.IndexOf(digest)
		if i < 0 {
			return
		}
		l := &s.Layers[i]
		for k := range l.Chunks {
			if l.Chunks[k].Index == index {
				l.Chunks[k].Done = done
				l.Chunks[k].Downloaded = downloaded
				break
			}
		}
		// Layer-level progress is the sum of finished chunks plus the
		// live bytes of the chunk currently streaming.
		var total int64
		for _, c := range l.Chunks {
			total += c.Downloaded
		}
		if total > l.Downloaded {
			l.Downloaded = total
		}
		s.DownloadedBytes = sumDownloaded(s.Layers)
	})
}

func (j *job) sumChunkProgress(digest string) int64 {
	st := j.ledger.State()
	i := st.IndexOf(digest)
	if i < 0 {
		return 0
	}
	var total int64
	for _, c := range st.Layers[i].Chunks {
		total += c.Downloaded
	}
	return total
}

// chunkBase is the byte offset of a chunk within the blob, used to convert
// a chunk's local progress into blob-relative progress.
func (j *job) chunkBase(digest string, index int) int64 {
	st := j.ledger.State()
	i := st.IndexOf(digest)
	if i < 0 {
		return 0
	}
	for _, c := range st.Layers[i].Chunks {
		if c.Index == index {
			return c.Start
		}
	}
	return 0
}

// setProgress updates a layer's byte counter without touching its status.
func (j *job) setProgress(digest string, downloaded int64) {
	j.ledger.Update(func(s *State) {
		i := s.IndexOf(digest)
		if i < 0 {
			return
		}
		l := &s.Layers[i]
		if downloaded > l.Downloaded {
			l.Downloaded = downloaded
		}
		s.DownloadedBytes = sumDownloaded(s.Layers)
	})
	j.report(digest, downloaded)
}

// --- small helpers ------------------------------------------------------

func chunkFile(dir string, index int) string {
	return filepath.Join(dir, fmt.Sprintf("chunk_%05d", index))
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// digestMatches compares an expected digest with an actual one, tolerating
// a registry that reports an algorithm prefix we did not ask for.
func digestMatches(expected, actual string) bool {
	return expected == "" || expected == actual
}

func backoffFor(attempt int) time.Duration {
	// Exponential with a ceiling: 2s, 4s, 8s … capped at 60s. The legacy
	// tool used the same ceiling but slept even for the first retry.
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sizeLabel renders a layer's log label with its size, when known.
func sizeLabel(digest string, size int64) string {
	name := shortDigest(digest)
	if size <= 0 {
		return name
	}
	return fmt.Sprintf("%s (%s)", name, FormatBytes(size))
}
