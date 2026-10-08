package puller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// --- hand-built images --------------------------------------------------
//
// registrytest.BuildImage only produces a single-layer image. The resume and
// chunk tests need several blobs (with known contents) behind one tag, so
// this file builds images and indexes by hand — without touching the shared
// fake registry.

// testLayer is one layer of a test image.
type testLayer struct {
	fileName string
	content  []byte
}

// testBlob is a layer as it exists on the wire and in the tar.
type testBlob struct {
	digest string
	gz     []byte
	rawTar []byte
	size   int64

	fileName string
	content  []byte
}

// testImage is a hand-built single-arch image.
type testImage struct {
	repo string
	tag  string
	arch string
	os   string

	layers []*testBlob

	configBytes    []byte
	configDigest   string
	manifestBytes  []byte
	manifestDigest string
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func buildLayerTar(t *testing.T, fileName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     fileName,
		Mode:     0o644,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func newTestImage(t *testing.T, repo, tag, arch string, layers ...testLayer) *testImage {
	t.Helper()
	im := &testImage{repo: repo, tag: tag, arch: arch, os: "linux"}

	descriptors := make([]map[string]any, 0, len(layers))
	diffIDs := make([]string, 0, len(layers))
	for _, l := range layers {
		raw := buildLayerTar(t, l.fileName, l.content)
		gz := gzipBytes(t, raw)
		blob := &testBlob{
			digest:   registrytest.Digest(gz),
			gz:       gz,
			rawTar:   raw,
			size:     int64(len(gz)),
			fileName: l.fileName,
			content:  l.content,
		}
		im.layers = append(im.layers, blob)
		diffIDs = append(diffIDs, registrytest.Digest(raw))
		descriptors = append(descriptors, map[string]any{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"digest":    blob.digest,
			"size":      len(gz),
		})
	}

	im.configBytes = mustJSON(t, map[string]any{
		"architecture": arch,
		"os":           im.os,
		"config":       map[string]any{"Labels": map[string]string{"test.image": repo}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	im.configDigest = registrytest.Digest(im.configBytes)

	im.manifestBytes = mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     registrytest.MediaTypeManifest,
		"config": map[string]any{
			"mediaType": registrytest.MediaTypeConfig,
			"digest":    im.configDigest,
			"size":      len(im.configBytes),
		},
		"layers": descriptors,
	})
	im.manifestDigest = registrytest.Digest(im.manifestBytes)
	return im
}

// register publishes the image's blobs and its manifest under both the tag
// and its own digest (the digest form is how an index refers to it).
func (im *testImage) register(r *registrytest.Registry) {
	for _, b := range im.layers {
		r.AddBlob(b.digest, b.gz)
	}
	r.AddBlob(im.configDigest, im.configBytes)
	r.AddManifest(im.repo, im.tag, registrytest.MediaTypeManifest, im.manifestBytes)
	r.AddManifest(im.repo, im.manifestDigest, registrytest.MediaTypeManifest, im.manifestBytes)
}

// registerIndex publishes an index over imgs behind repo:tag.
func registerIndex(t *testing.T, r *registrytest.Registry, repo, tag string, imgs ...*testImage) {
	t.Helper()
	entries := make([]map[string]any, 0, len(imgs))
	for _, im := range imgs {
		im.register(r)
		entries = append(entries, map[string]any{
			"mediaType": registrytest.MediaTypeManifest,
			"digest":    im.manifestDigest,
			"size":      len(im.manifestBytes),
			"platform":  map[string]any{"os": im.os, "architecture": im.arch},
		})
	}
	index := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     registrytest.MediaTypeIndex,
		"manifests":     entries,
	})
	r.AddManifest(repo, tag, registrytest.MediaTypeIndex, index)
}

// randomBytes returns incompressible data, so gzip cannot shrink a chunk
// test's layer and the chunk arithmetic stays predictable.
func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

// mockRef points a Ref at a registrytest (or proxy) host over plain HTTP.
func mockRef(host, repo, tag string) registry.Ref {
	return registry.Ref{Registry: host, Repository: repo, Tag: tag, UseHTTP: true}
}

func testPlatform(arch string) registry.Platform {
	return registry.Platform{OS: "linux", Architecture: arch}
}

// --- reading the produced archive ---------------------------------------

// readManifestMembers decodes manifest.json out of a produced archive.
func readManifestMembers(t *testing.T, members map[string][]byte) []saveManifestEntry {
	t.Helper()
	raw, ok := members["manifest.json"]
	if !ok {
		t.Fatalf("manifest.json missing; members: %v", memberNames(members))
	}
	var entries []saveManifestEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("manifest.json: %v (%s)", err, raw)
	}
	return entries
}

func readRepositories(t *testing.T, members map[string][]byte) map[string]map[string]string {
	t.Helper()
	raw, ok := members["repositories"]
	if !ok {
		t.Fatalf("repositories missing; members: %v", memberNames(members))
	}
	var repos map[string]map[string]string
	if err := json.Unmarshal(raw, &repos); err != nil {
		t.Fatalf("repositories: %v (%s)", err, raw)
	}
	return repos
}

func memberNames(members map[string][]byte) []string {
	out := make([]string, 0, len(members))
	for name := range members {
		out = append(out, name)
	}
	return out
}

// untarBytes expands an in-memory tar.
func untarBytes(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read layer tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read layer member %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = body
	}
	return out
}

// layerFileContents returns, in manifest.json order, the single file each
// layer.tar member holds.
func layerFileContents(t *testing.T, members map[string][]byte) []map[string][]byte {
	t.Helper()
	entry := readManifestMembers(t, members)
	out := make([]map[string][]byte, 0, len(entry))
	for _, e := range entry {
		for _, layerPath := range e.Layers {
			data, ok := members[layerPath]
			if !ok {
				t.Fatalf("manifest.json lists %q but the archive has %v", layerPath, memberNames(members))
			}
			out = append(out, untarBytes(t, data))
		}
	}
	return out
}

// onlyFile returns the sole entry of an expanded layer.
func onlyFile(t *testing.T, files map[string][]byte) (string, []byte) {
	t.Helper()
	if len(files) != 1 {
		t.Fatalf("layer holds %d members, want exactly 1: %v", len(files), files)
	}
	for name, body := range files {
		return name, body
	}
	return "", nil
}

// layerIDFor computes the v1 layer id the archive must use, straight from
// CONTRACT.md §3 — independently of the production chainID:
// sha256hex(parentID + "\n" + layerDigest + "\n").
func layerIDFor(parentID, layerDigest string) string {
	sum := sha256.Sum256([]byte(parentID + "\n" + layerDigest + "\n"))
	return hex.EncodeToString(sum[:])
}

// --- fake registry proxies ---------------------------------------------
//
// Several resume guarantees can only be asserted if the first pull is
// genuinely interrupted *mid-blob*. The download is therefore driven through
// a real HTTP proxy in front of the registrytest registry that (a) can slow
// one blob down and (b) counts requests. The registry itself stays the shared
// fake one — nothing about the protocol is reimplemented here.

type throttleSpec struct {
	digest string
	chunk  int
	delay  time.Duration

	mu       sync.Mutex
	disabled bool
}

func (s *throttleSpec) disable() {
	s.mu.Lock()
	s.disabled = true
	s.mu.Unlock()
}

func (s *throttleSpec) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.disabled
}

type slowBody struct {
	inner io.ReadCloser
	chunk int
	delay time.Duration
}

func (b *slowBody) Read(p []byte) (int, error) {
	if b.chunk > 0 && len(p) > b.chunk {
		p = p[:b.chunk]
	}
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	return b.inner.Read(p)
}

func (b *slowBody) Close() error { return b.inner.Close() }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type mockProxy struct {
	*httptest.Server

	mu    sync.Mutex
	paths map[string]int
}

func (p *mockProxy) Host() string { return strings.TrimPrefix(p.URL, "http://") }

// hits counts requests whose path contains substr.
func (p *mockProxy) hits(substr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int
	for path, c := range p.paths {
		if strings.Contains(path, substr) {
			n += c
		}
	}
	return n
}

// reset forgets the request counters, so a test can measure one run only.
func (p *mockProxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = map[string]int{}
}

// newMockProxy fronts upstream with a real HTTP server. When slow is
// non-nil, reads of that blob are throttled so a download can be
// interrupted deterministically.
func newMockProxy(t *testing.T, upstream *registrytest.Registry, slow *throttleSpec) *mockProxy {
	t.Helper()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	base := proxy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	proxy.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(req)
		if err != nil || slow == nil || !slow.active() {
			return resp, err
		}
		if strings.Contains(req.URL.Path, slow.digest) && resp.StatusCode == http.StatusOK {
			resp.Body = &slowBody{inner: resp.Body, chunk: slow.chunk, delay: slow.delay}
		}
		return resp, nil
	})

	p := &mockProxy{paths: map[string]int{}}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths[r.URL.Path]++
		p.mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Server.Close)
	return p
}

// --- sinks ---------------------------------------------------------------

// recordingSink keeps every state and progress callback for assertions.
type recordingSink struct {
	NopSink

	mu       sync.Mutex
	states   []State
	progress []progressSample
}

type progressSample struct {
	digest     string
	downloaded int64
}

func (s *recordingSink) OnState(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, st)
}

func (s *recordingSink) OnProgress(digest string, downloaded int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, progressSample{digest: digest, downloaded: downloaded})
}

func (s *recordingSink) States() []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]State(nil), s.states...)
}

func (s *recordingSink) Progress() []progressSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]progressSample(nil), s.progress...)
}

// cancelSink interrupts the download as soon as the watched blob has
// reported real progress — the deterministic trigger for a mid-blob
// interruption (a cancel can only fire after bytes reached disk).
type cancelSink struct {
	NopSink

	digest string
	cancel context.CancelFunc

	mu     sync.Mutex
	fired  bool
	states []State
}

func newCancelSink(digest string, cancel context.CancelFunc) *cancelSink {
	return &cancelSink{digest: digest, cancel: cancel}
}

func (s *cancelSink) OnState(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, st)
}

func (s *cancelSink) OnProgress(digest string, downloaded int64) {
	if digest != s.digest || downloaded <= 0 {
		return
	}
	s.mu.Lock()
	fire := !s.fired
	s.fired = true
	s.mu.Unlock()
	if fire {
		s.cancel()
	}
}

func (s *cancelSink) States() []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]State(nil), s.states...)
}

// --- on-disk helpers -----------------------------------------------------

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// blobFilePath mirrors the puller's blob location for a digest.
func blobFilePath(workDir, digest string) string {
	return filepath.Join(workDir, "blobs", sanitizeDigest(digest))
}

func chunkDirPath(workDir, digest string) string {
	return filepath.Join(workDir, "chunks", sanitizeDigest(digest))
}

// completedChunkIndices lists the chunks that are whole on disk.
func completedChunkIndices(dir string) []int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "chunk_") || strings.HasSuffix(name, ".part") {
			continue
		}
		var idx int
		if _, err := fmt.Sscanf(name, "chunk_%d", &idx); err != nil {
			continue
		}
		out = append(out, idx)
	}
	return out
}

type pullOutcome struct {
	res *Result
	err error
}

// --- tests ---------------------------------------------------------------

// TestResumeAfterInterruptContinuesFromOffset is the core F3 guarantee: a
// pull killed mid-blob, resumed in a fresh process against the same WorkDir,
// must send `Range: bytes=<bytes already on disk>-` instead of re-fetching
// the blob from zero, and must produce exactly the archive a clean pull
// would.
func TestResumeAfterInterruptContinuesFromOffset(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	small := testLayer{fileName: "small.txt", content: []byte("small layer")}
	big := testLayer{fileName: "big.bin", content: randomBytes(256<<10, 7)}
	img := newTestImage(t, "library/app", "latest", "amd64", small, big)
	img.register(mock)

	slow := &throttleSpec{digest: img.layers[1].digest, chunk: 16 << 10, delay: time.Millisecond}
	proxy := newMockProxy(t, mock, slow)
	ref := mockRef(proxy.Host(), "library/app", "latest")
	platform := testPlatform("amd64")

	// --- run 1: cancelled as soon as the big layer has bytes on disk.
	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := newCancelSink(img.layers[1].digest, cancel)

	first, err := Pull(ctx, Options{Ref: ref, Platform: platform, WorkDir: workDir, Sink: sink, MaxRetries: 1})
	if err == nil {
		t.Fatalf("the first pull was expected to be interrupted, got %+v", first)
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("first pull error = %v, want the cancellation", err)
	}

	partialPath := blobFilePath(workDir, img.layers[1].digest)
	fi, statErr := os.Stat(partialPath)
	if statErr != nil {
		t.Fatalf("the interrupted layer left no partial file at %s: %v", partialPath, statErr)
	}
	resumeOffset := fi.Size()
	if resumeOffset <= 0 || resumeOffset >= img.layers[1].size {
		t.Fatalf("partial size = %d, want a value strictly inside (0, %d)", resumeOffset, img.layers[1].size)
	}
	if pathExists(partialPath + ".ok") {
		t.Fatal("a partial blob must not be sealed with an .ok marker")
	}

	// --- run 2: same WorkDir, same registry, a brand-new Pull (the puller
	// builds its own client per call, so this is a process restart).
	mock.ResetRequests()
	slow.disable()

	second, err := Pull(context.Background(), Options{Ref: ref, Platform: platform, WorkDir: workDir, MaxRetries: 1})
	if err != nil {
		t.Fatalf("resumed pull: %v", err)
	}

	var resumed []registrytest.RangeRequest
	for _, r := range mock.RangeRequests() {
		if r.Digest == img.layers[1].digest {
			resumed = append(resumed, r)
		}
	}
	if len(resumed) == 0 {
		t.Fatalf("the resumed pull sent no Range request for the interrupted layer; saw %+v", mock.RangeRequests())
	}
	if resumed[0].Start != resumeOffset {
		t.Errorf("Range start = %d, want the %d bytes already on disk", resumed[0].Start, resumeOffset)
	}
	if resumed[0].End != -1 {
		t.Errorf("Range end = %d, want an open-ended range", resumed[0].End)
	}
	for _, r := range mock.RangeRequests() {
		if r.Start == 0 {
			t.Errorf("blob %s was re-requested from byte 0: %+v", r.Digest, r)
		}
	}

	// The resumed archive must carry exactly the same members with exactly
	// the same bytes as a clean one-off pull. (Raw tar bytes are not
	// comparable across runs: each member's mtime is stamped into the
	// header.)
	clean, err := Pull(context.Background(), Options{
		Ref: ref, Platform: platform, WorkDir: t.TempDir(), MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("clean pull: %v", err)
	}
	assertSameArchive(t, second.TarPath, clean.TarPath)

	members := registrytest.ReadTarMembers(t, second.TarPath)
	files := layerFileContents(t, members)
	if len(files) != 2 {
		t.Fatalf("archive has %d layers, want 2", len(files))
	}
	if name, body := onlyFile(t, files[1]); name != big.fileName || !bytes.Equal(body, big.content) {
		t.Errorf("resumed layer = %q (%d bytes), want %q (%d bytes)", name, len(body), big.fileName, len(big.content))
	}
}

// assertSameArchive compares two produced archives member by member: the
// names must match and every member's bytes must be identical.
func assertSameArchive(t *testing.T, got, want string) {
	t.Helper()
	gotMembers := registrytest.ReadTarMembers(t, got)
	wantMembers := registrytest.ReadTarMembers(t, want)

	if len(gotMembers) != len(wantMembers) {
		t.Fatalf("archive members = %v, want %v", memberNames(gotMembers), memberNames(wantMembers))
	}
	for name, wantBody := range wantMembers {
		gotBody, ok := gotMembers[name]
		if !ok {
			t.Fatalf("archive is missing member %q (has %v)", name, memberNames(gotMembers))
		}
		if !bytes.Equal(gotBody, wantBody) {
			t.Errorf("member %q differs from the clean pull (%d bytes vs %d)", name, len(gotBody), len(wantBody))
		}
	}
}

// TestServerIgnoringRangeDoesNotCorrupt is the 1ms-fork regression: when a
// mirror answers a resumed request with a full 200, the puller must throw
// the partial file away and restart — appending the whole blob onto the
// partial would produce a corrupt layer that only the checksum would catch.
func TestServerIgnoringRangeDoesNotCorrupt(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{IgnoreRange: true})
	small := testLayer{fileName: "small.txt", content: []byte("small layer")}
	big := testLayer{fileName: "big.bin", content: randomBytes(128<<10, 11)}
	img := newTestImage(t, "library/app", "latest", "amd64", small, big)
	img.register(mock)

	slow := &throttleSpec{digest: img.layers[1].digest, chunk: 16 << 10, delay: time.Millisecond}
	proxy := newMockProxy(t, mock, slow)
	ref := mockRef(proxy.Host(), "library/app", "latest")
	platform := testPlatform("amd64")

	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := newCancelSink(img.layers[1].digest, cancel)
	if _, err := Pull(ctx, Options{Ref: ref, Platform: platform, WorkDir: workDir, Sink: sink, MaxRetries: 1}); err == nil {
		t.Fatal("the first pull was expected to be interrupted")
	}

	partialPath := blobFilePath(workDir, img.layers[1].digest)
	fi, err := os.Stat(partialPath)
	if err != nil {
		t.Fatalf("no partial file to resume from: %v", err)
	}
	if fi.Size() >= img.layers[1].size {
		t.Fatalf("partial = %d bytes, want fewer than the whole %d", fi.Size(), img.layers[1].size)
	}

	mock.ResetRequests()
	proxy.reset()
	slow.disable()
	res, err := Pull(context.Background(), Options{Ref: ref, Platform: platform, WorkDir: workDir, MaxRetries: 1})
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}

	// The partial must have been discarded and the blob re-fetched exactly
	// once. Appending the whole blob onto the partial (the 1ms fork's bug)
	// only *looks* fine because the digest check then throws the file away
	// and re-downloads it — the second request is the tell.
	if got := proxy.hits(img.layers[1].digest); got != 1 {
		t.Errorf("blob requests in the resumed pull = %d, want exactly 1: the client appended the whole blob "+
			"onto the partial file instead of restarting", got)
	}

	warned := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "断点续传") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("Warnings = %v, want a 断点续传 notice for the mirror that ignored the Range header", res.Warnings)
	}

	members := registrytest.ReadTarMembers(t, res.TarPath)
	files := layerFileContents(t, members)
	if len(files) != 2 {
		t.Fatalf("archive has %d layers, want 2", len(files))
	}
	if _, body := onlyFile(t, files[1]); !bytes.Equal(body, big.content) {
		t.Fatalf("layer content = %d bytes, want the original %d bytes (the partial was appended to rather than discarded)",
			len(body), len(big.content))
	}

	// The sealed blob must be exactly the blob the registry served.
	sealed, err := os.ReadFile(partialPath)
	if err != nil {
		t.Fatalf("read the sealed blob: %v", err)
	}
	if !bytes.Equal(sealed, img.layers[1].gz) {
		t.Fatalf("sealed blob = %d bytes, want the %d-byte original", len(sealed), len(img.layers[1].gz))
	}
	if !pathExists(partialPath + ".ok") {
		t.Error("a verified blob must carry its .ok marker")
	}
}

// TestDigestMismatchIsDetectedAndRetried protects the "bad bytes are worse
// than no bytes" rule: a blob that does not hash to the digest the manifest
// advertises must never be sealed, must be discarded, and must be retried.
func TestDigestMismatchIsDetectedAndRetried(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := newTestImage(t, "library/app", "latest", "amd64", testLayer{fileName: "f.txt", content: []byte("payload")})

	// Publish the manifest but serve tampered bytes under the advertised
	// digest (same length, so only the checksum can catch it).
	img.register(mock)
	tampered := make([]byte, len(img.layers[0].gz))
	copy(tampered, img.layers[0].gz)
	tampered[len(tampered)/2] ^= 0xff
	mock.AddBlob(img.layers[0].digest, tampered)

	var (
		mu   sync.Mutex
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/") {
			mu.Lock()
			hits++
			mu.Unlock()
		}
		mock.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	workDir := t.TempDir()
	ref := mockRef(strings.TrimPrefix(srv.URL, "http://"), "library/app", "latest")
	platform := testPlatform("amd64")
	res, err := Pull(context.Background(), Options{
		Ref:        ref,
		Platform:   platform,
		WorkDir:    workDir,
		MaxRetries: 1, // one retry: the test must stay fast
	})
	if err == nil {
		t.Fatalf("Pull succeeded on a blob that does not match its digest: %+v", res)
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %q, want the digest mismatch to be named", err)
	}
	shortLayer := strings.TrimPrefix(img.layers[0].digest, "sha256:")[:12]
	if !strings.Contains(err.Error(), shortLayer) {
		t.Errorf("error = %q, want it to identify the failing layer %s", err, shortLayer)
	}

	mu.Lock()
	blobHits := hits
	mu.Unlock()
	if blobHits < 2 {
		t.Errorf("blob requests = %d, want the download to be retried", blobHits)
	}

	if pathExists(blobFilePath(workDir, img.layers[0].digest) + ".ok") {
		t.Error("the mismatching blob was sealed with an .ok marker")
	}
	if pathExists(blobFilePath(workDir, img.layers[0].digest)) {
		t.Error("the mismatching blob bytes were left behind")
	}
	if pathExists(filepath.Join(DefaultOutputDir(workDir, ref, platform), TarName(ref, platform))) {
		t.Error("a tar was produced from a failed download")
	}
}

// TestChunkedDownloadUsesRanges protects the chunked path: a blob above the
// threshold is fetched as several exact, closed Range requests and merged
// back into the original bytes.
func TestChunkedDownloadUsesRanges(t *testing.T) {
	const chunkSize = 64 << 10
	mock := registrytest.New(t, registrytest.Options{})
	big := testLayer{fileName: "data.bin", content: randomBytes(300<<10, 3)}
	img := newTestImage(t, "library/app", "latest", "amd64", big)
	img.register(mock)

	workDir := t.TempDir()
	res, err := Pull(context.Background(), Options{
		Ref:            mockRef(mock.Host(), img.repo, img.tag),
		Platform:       testPlatform("amd64"),
		WorkDir:        workDir,
		MaxRetries:     1,
		ChunkSize:      chunkSize,
		ChunkThreshold: chunkSize,
		Workers:        4,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	size := img.layers[0].size
	wantChunks := int((size + chunkSize - 1) / chunkSize)
	if wantChunks < 3 {
		t.Fatalf("the test layer only produced %d chunks; it must exercise several Range requests", wantChunks)
	}

	got := map[string]registrytest.RangeRequest{}
	for _, r := range mock.RangeRequests() {
		if r.Digest != img.layers[0].digest {
			continue
		}
		if r.Header != fmt.Sprintf("bytes=%d-%d", r.Start, r.End) {
			t.Errorf("chunk range %q is not an exact closed range", r.Header)
		}
		got[r.Header] = r
	}
	if len(got) != wantChunks {
		t.Fatalf("saw %d distinct chunk ranges, want %d: %v", len(got), wantChunks, got)
	}
	for i := 0; i < wantChunks; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if end > size-1 {
			end = size - 1
		}
		header := fmt.Sprintf("bytes=%d-%d", start, end)
		if _, ok := got[header]; !ok {
			t.Errorf("missing chunk range %s", header)
		}
	}

	// The merged blob must be the blob the registry served...
	sealed, err := os.ReadFile(blobFilePath(workDir, img.layers[0].digest))
	if err != nil {
		t.Fatalf("read merged blob: %v", err)
	}
	if !bytes.Equal(sealed, img.layers[0].gz) {
		t.Fatalf("merged blob = %d bytes, want the original %d", len(sealed), len(img.layers[0].gz))
	}
	// ...and the layer inside the tar must be the decompressed original.
	members := registrytest.ReadTarMembers(t, res.TarPath)
	files := layerFileContents(t, members)
	if name, body := onlyFile(t, files[0]); name != big.fileName || !bytes.Equal(body, big.content) {
		t.Fatalf("layer = %q (%d bytes), want %q (%d bytes)", name, len(body), big.fileName, len(big.content))
	}
	// The chunks are derived data: once merged they are cleaned up.
	if pathExists(chunkDirPath(workDir, img.layers[0].digest)) {
		t.Error("the chunk directory was left behind after a successful merge")
	}
}

// TestChunkedResumeSkipsFinishedChunks protects the sub-blob resume ledger: a
// second pull over an interrupted chunked download must request Range only
// for the chunks that are not already whole on disk.
func TestChunkedResumeSkipsFinishedChunks(t *testing.T) {
	const chunkSize = 64 << 10
	mock := registrytest.New(t, registrytest.Options{})
	big := testLayer{fileName: "data.bin", content: randomBytes(1<<20, 5)}
	img := newTestImage(t, "library/app", "latest", "amd64", big)
	img.register(mock)

	slow := &throttleSpec{digest: img.layers[0].digest, chunk: 8 << 10, delay: time.Millisecond}
	proxy := newMockProxy(t, mock, slow)
	ref := mockRef(proxy.Host(), img.repo, img.tag)
	platform := testPlatform("amd64")
	workDir := t.TempDir()
	opts := Options{
		Ref: ref, Platform: platform, WorkDir: workDir,
		MaxRetries: 1, ChunkSize: chunkSize, ChunkThreshold: chunkSize, Workers: 4,
	}

	size := img.layers[0].size
	totalChunks := int((size + chunkSize - 1) / chunkSize)
	if totalChunks < 6 {
		t.Fatalf("the test layer only produced %d chunks", totalChunks)
	}

	// Run 1: run the pull in the background and cancel it once a couple of
	// chunks are whole on disk, so the resume scan has something to skip.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcome := make(chan pullOutcome, 1)
	go func() {
		res, err := Pull(ctx, opts)
		outcome <- pullOutcome{res: res, err: err}
	}()

	chunkDir := chunkDirPath(workDir, img.layers[0].digest)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(completedChunkIndices(chunkDir)) >= 2 {
			cancel()
			break
		}
		select {
		case o := <-outcome:
			t.Fatalf("the chunked pull finished before it could be interrupted: %+v", o)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("timed out waiting for chunks to land; completed: %v", completedChunkIndices(chunkDir))
		}
		time.Sleep(time.Millisecond)
	}
	first := <-outcome
	if first.err == nil {
		t.Fatalf("the first chunked pull was expected to be interrupted: %+v", first.res)
	}

	doneBefore := completedChunkIndices(chunkDir)
	if len(doneBefore) < 2 || len(doneBefore) >= totalChunks {
		t.Fatalf("completed chunks before the resume = %v, want a proper subset of %d chunks", doneBefore, totalChunks)
	}

	// Run 2: fresh Pull, same WorkDir and registry.
	mock.ResetRequests()
	slow.disable()
	res, err := Pull(context.Background(), opts)
	if err != nil {
		t.Fatalf("resumed chunked pull: %v", err)
	}

	requested := map[int64]bool{}
	for _, r := range mock.RangeRequests() {
		if r.Digest == img.layers[0].digest {
			requested[r.Start] = true
		}
	}

	skipped := 0
	for _, idx := range doneBefore {
		start := int64(idx) * chunkSize
		if requested[start] {
			t.Errorf("chunk %d (offset %d) was already complete but was requested again", idx, start)
		} else {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatalf("no finished chunk was skipped; completed=%v requested=%v", doneBefore, requested)
	}

	// Every chunk must have been fetched exactly once across the two runs.
	for i := 0; i < totalChunks; i++ {
		start := int64(i) * chunkSize
		if !requested[start] && !containsInt(doneBefore, i) {
			t.Errorf("chunk %d (offset %d) was never downloaded", i, start)
		}
	}

	members := registrytest.ReadTarMembers(t, res.TarPath)
	files := layerFileContents(t, members)
	if _, body := onlyFile(t, files[0]); !bytes.Equal(body, big.content) {
		t.Fatalf("resumed layer = %d bytes, want the original %d", len(body), len(big.content))
	}
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TestRepullFromCacheReportsCompletedBlobs protects the .ok-marker short
// circuit (a warm WorkDir must re-export without touching a single blob) and
// the field that reports it, Result.Completed — CONTRACT.md §3 calls it
// "layers copied from a previous run".
//
// It fails on the current implementation: pass 2 of carryOver (puller.go:
// 332-340) sets *every* carried-over layer back to LayerPending, so the
// countSettled(state) taken at puller.go:201 is always 0 and the UI can
// never show "N layers already present".
func TestRepullFromCacheReportsCompletedBlobs(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	layers := []testLayer{
		{fileName: "a.txt", content: []byte("layer a")},
		{fileName: "b.txt", content: []byte("layer b")},
	}
	img := newTestImage(t, "library/app", "latest", "amd64", layers...)
	img.register(mock)

	proxy := newMockProxy(t, mock, nil)
	ref := mockRef(proxy.Host(), img.repo, img.tag)
	platform := testPlatform("amd64")
	workDir := t.TempDir()
	opts := Options{Ref: ref, Platform: platform, WorkDir: workDir, MaxRetries: 1}

	if _, err := Pull(context.Background(), opts); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	proxy.reset()
	warm, err := Pull(context.Background(), opts)
	if err != nil {
		t.Fatalf("second pull from a warm work dir: %v", err)
	}
	if warm.Completed != len(layers)+1 {
		t.Errorf("Result.Completed = %d, want %d (the blobs satisfied by their .ok markers)", warm.Completed, len(layers)+1)
	}
	for _, b := range img.layers {
		if got := proxy.hits(b.digest); got != 0 {
			t.Errorf("layer %s was re-downloaded %d times from a warm work dir", b.digest[:19], got)
		}
	}
	// The config blob is still fetched once: ResolveImage reads it to report
	// the platform a single-arch manifest actually describes. That is a
	// metadata read, not a re-download, so only the layer payloads above are
	// asserted to be untouched.
	if got := proxy.hits(img.configDigest); got > 1 {
		t.Errorf("the config blob was fetched %d times, want the single ResolveImage read", got)
	}

	// The re-export must still be the same archive.
	members := registrytest.ReadTarMembers(t, warm.TarPath)
	files := layerFileContents(t, members)
	if len(files) != len(layers) {
		t.Fatalf("re-exported archive has %d layers, want %d", len(files), len(layers))
	}
	for i, want := range layers {
		if name, body := onlyFile(t, files[i]); name != want.fileName || !bytes.Equal(body, want.content) {
			t.Errorf("re-exported layer %d = %q (%q), want %q (%q)", i, name, body, want.fileName, want.content)
		}
	}
}

// TestResumeRejectsForeignLedger protects the identity check: a
// progress.json left by a *different* image must be ignored completely, not
// carried into this download.
func TestResumeRejectsForeignLedger(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := newTestImage(t, "library/app", "latest", "amd64", testLayer{fileName: "f.txt", content: []byte("payload")})
	img.register(mock)

	ref := mockRef(mock.Host(), img.repo, img.tag)
	platform := testPlatform("amd64")
	workDir := t.TempDir()

	foreign := &State{
		Ref:      "other.registry/other/app:9.9",
		Platform: "linux/arm64",
		Status:   StatusRunning,
		Layers: []LayerState{{
			Digest:     img.layers[0].digest,
			Kind:       KindLayer,
			Name:       "foreign",
			Size:       img.layers[0].size,
			Downloaded: img.layers[0].size,
			Status:     LayerCompleted,
		}},
	}
	if err := SaveState(workDir, foreign); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if st, err := LoadState(workDir, ref, platform); err != nil || st != nil {
		t.Fatalf("LoadState(foreign) = %+v, %v; want (nil, nil)", st, err)
	}

	sink := &recordingSink{}
	res, err := Pull(context.Background(), Options{Ref: ref, Platform: platform, WorkDir: workDir, Sink: sink, MaxRetries: 1})
	if err != nil {
		t.Fatalf("Pull with a foreign ledger in the WorkDir: %v", err)
	}

	states := sink.States()
	if len(states) == 0 {
		t.Fatal("the sink saw no states")
	}
	if states[0].DownloadedBytes != 0 {
		t.Errorf("the first state reports DownloadedBytes = %d, want 0 (the foreign ledger was reused)", states[0].DownloadedBytes)
	}
	wantTotal := img.layers[0].size + int64(len(img.configBytes))
	if states[0].TotalBytes != wantTotal {
		t.Errorf("TotalBytes = %d, want %d from this image's own manifest", states[0].TotalBytes, wantTotal)
	}

	members := registrytest.ReadTarMembers(t, res.TarPath)
	files := layerFileContents(t, members)
	if _, body := onlyFile(t, files[0]); !bytes.Equal(body, []byte("payload")) {
		t.Fatalf("layer content = %q, want the real payload", body)
	}
}

// TestCorruptLedgerIsIgnored protects the "always safe to start fresh" rule:
// a corrupt progress.json yields (nil, nil) and the pull still succeeds.
func TestCorruptLedgerIsIgnored(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := newTestImage(t, "library/app", "latest", "amd64", testLayer{fileName: "f.txt", content: []byte("payload")})
	img.register(mock)

	ref := mockRef(mock.Host(), img.repo, img.tag)
	platform := testPlatform("amd64")
	workDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(workDir, ProgressFileName), []byte("{{{ not json at all"), 0o644); err != nil {
		t.Fatalf("write corrupt ledger: %v", err)
	}
	st, err := LoadState(workDir, ref, platform)
	if err != nil {
		t.Fatalf("LoadState on a corrupt ledger returned an error: %v", err)
	}
	if st != nil {
		t.Fatalf("LoadState on a corrupt ledger = %+v, want nil", st)
	}

	res, err := Pull(context.Background(), Options{Ref: ref, Platform: platform, WorkDir: workDir, MaxRetries: 1})
	if err != nil {
		t.Fatalf("Pull with a corrupt ledger: %v", err)
	}
	if !pathExists(res.TarPath) {
		t.Fatalf("no archive at %s", res.TarPath)
	}

	// A successful pull consumes its ledger, and a missing ledger is equally
	// benign.
	if st, err := LoadState(workDir, ref, platform); err != nil || st != nil {
		t.Fatalf("LoadState with no ledger = %+v, %v; want (nil, nil)", st, err)
	}
}

// TestRetryFromScratchCleansWorkDir protects "retry from scratch": the
// work directory (partials, ledger and blob cache together) is removed.
func TestRetryFromScratchCleansWorkDir(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := newTestImage(t, "library/app", "latest", "amd64", testLayer{fileName: "f.txt", content: []byte("payload")})
	img.register(mock)

	workDir := t.TempDir()
	if _, err := Pull(context.Background(), Options{
		Ref: mockRef(mock.Host(), img.repo, img.tag), Platform: testPlatform("amd64"), WorkDir: workDir, MaxRetries: 1,
	}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !pathExists(workDir) {
		t.Fatal("the work directory vanished during a successful pull")
	}
	if err := CleanWorkDir(workDir); err != nil {
		t.Fatalf("CleanWorkDir: %v", err)
	}
	if pathExists(workDir) {
		t.Fatalf("%s still exists after CleanWorkDir", workDir)
	}
	if err := CleanWorkDir(""); err != nil {
		t.Errorf("CleanWorkDir(\"\") = %v, want nil", err)
	}
}

// TestConcurrentLayersAreAllDownloaded protects the worker pool: with four
// layers and four workers, every layer must land, in manifest order.
func TestConcurrentLayersAreAllDownloaded(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	layers := []testLayer{
		{fileName: "a.txt", content: []byte("layer a")},
		{fileName: "b.txt", content: []byte("layer b")},
		{fileName: "c.txt", content: []byte("layer c")},
		{fileName: "d.txt", content: []byte("layer d")},
	}
	img := newTestImage(t, "library/app", "latest", "amd64", layers...)
	img.register(mock)

	res, err := Pull(context.Background(), Options{
		Ref:      mockRef(mock.Host(), img.repo, img.tag),
		Platform: testPlatform("amd64"),
		WorkDir:  t.TempDir(),
		Workers:  4,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Layers != 4 {
		t.Errorf("Result.Layers = %d, want 4", res.Layers)
	}

	members := registrytest.ReadTarMembers(t, res.TarPath)
	files := layerFileContents(t, members)
	if len(files) != 4 {
		t.Fatalf("archive has %d layers, want 4", len(files))
	}
	for i, want := range layers {
		name, body := onlyFile(t, files[i])
		if name != want.fileName || !bytes.Equal(body, want.content) {
			t.Errorf("layer %d = %q (%q), want %q (%q)", i, name, body, want.fileName, want.content)
		}
	}

	// The chain must follow the manifest order: every layer's parent is the
	// previous layer's id, and repositories points at the last one.
	entry := readManifestMembers(t, members)[0]
	repos := readRepositories(t, members)
	lastID := strings.Split(entry.Layers[3], "/")[0]
	if got := repos[img.repo][img.tag]; got != lastID {
		t.Errorf("repositories[%s][%s] = %q, want the last layer id %q", img.repo, img.tag, got, lastID)
	}

	parent := ""
	for i, b := range img.layers {
		wantID := layerIDFor(parent, b.digest)
		if wantID != strings.Split(entry.Layers[i], "/")[0] {
			t.Errorf("layer %d id = %q, want %q", i, strings.Split(entry.Layers[i], "/")[0], wantID)
		}
		raw, ok := members[wantID+"/json"]
		if !ok {
			t.Fatalf("layer %d metadata member %s/json is missing", i, wantID)
		}
		var meta v1LayerJSON
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("layer %d json: %v", i, err)
		}
		if meta.ID != wantID {
			t.Errorf("layer %d json id = %q, want %q", i, meta.ID, wantID)
		}
		if i == 0 {
			if meta.Parent != nil {
				t.Errorf("first layer parent = %q, want null", *meta.Parent)
			}
		} else if meta.Parent == nil || *meta.Parent != parent {
			t.Errorf("layer %d parent = %v, want %q", i, meta.Parent, parent)
		}
		parent = wantID
	}
}
