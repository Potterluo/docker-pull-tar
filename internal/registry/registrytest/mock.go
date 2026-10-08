// Package registrytest provides an in-process fake Docker Registry v2 used
// by the registry, puller and server test suites.
//
// It deliberately implements the parts of the protocol this tool depends
// on — and the parts that broke the previous implementation: Basic *and*
// Bearer auth, Range requests with real 206/416 semantics, and a mode that
// ignores Range entirely (which must make the client restart, never
// append).
//
// It is a normal package rather than a _test.go file so more than one
// package's tests can use it. Nothing in the production binary imports it,
// so it is not linked into the shipped executable.
package registrytest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Media types the fake registry advertises.
const (
	MediaTypeManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeConfig   = "application/vnd.docker.container.image.v1+json"
)

// Options configures the fake registry's behaviour.
type Options struct {
	// BasicUser/BasicPass, when set, require HTTP Basic credentials on
	// every /v2/* route.
	BasicUser string
	BasicPass string

	// Bearer, when true, advertises a Bearer challenge and serves a token
	// endpoint that issues a fixed token.
	Bearer bool

	// IgnoreRange makes every blob response a full 200 regardless of the
	// Range header — the "my mirror does not support resume" case.
	IgnoreRange bool

	// DenyBlobs makes blob requests fail with 500, for retry tests.
	DenyBlobs bool

	// NoRangesAdvertised suppresses 206 support without ignoring Range:
	// the server answers 200 only when no Range was asked for and 416
	// otherwise (a pathological registry).
	RejectRanges bool
}

// RangeRequest records one saw Range header, for assertions.
type RangeRequest struct {
	Digest string
	Start  int64
	End    int64 // -1 for open-ended
	Header string
}

type manifestEntry struct {
	mediaType string

	tags      map[string][]string
	tagsCalls int
	body      []byte
}

// Registry is a running fake registry.
type Registry struct {
	*httptest.Server

	opts Options

	mu         sync.Mutex
	blobs      map[string][]byte
	manifests  map[string]manifestEntry
	requests   []RangeRequest
	authCalls  int
	tokenCalls int
	tags       map[string][]string
	tagsCalls  int
}

// New starts a fake registry; it is shut down when the test finishes.
func New(t *testing.T, opts Options) *Registry {
	t.Helper()
	r := &Registry{
		opts:      opts,
		blobs:     make(map[string][]byte),
		manifests: make(map[string]manifestEntry),
		tags:      make(map[string][]string),
	}
	r.Server = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.Server.Close)
	return r
}

// Host returns "127.0.0.1:port" — usable as a registry host in a Ref.
func (r *Registry) Host() string {
	return strings.TrimPrefix(r.Server.URL, "http://")
}

// AddBlob registers blob content under digest.
func (r *Registry) AddBlob(digest string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blobs[digest] = data
}

// AddManifest registers a manifest body under (repo, ref).
func (r *Registry) AddManifest(repo, ref, mediaType string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests[manifestKey(repo, ref)] = manifestEntry{mediaType: mediaType, body: body}
}

// RangeRequests returns a copy of every Range request seen.
func (r *Registry) RangeRequests() []RangeRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RangeRequest(nil), r.requests...)
}

// ResetRequests clears the recorded requests and counters.
func (r *Registry) ResetRequests() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = nil
	r.authCalls = 0
	r.tokenCalls = 0
}

// AuthCalls reports how many times the /v2/ ping endpoint was hit.
func (r *Registry) AuthCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authCalls
}

// TokenCalls reports how many times the token endpoint was hit.
func (r *Registry) TokenCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokenCalls
}

func manifestKey(repo, ref string) string { return repo + "@" + ref }

func (r *Registry) handle(w http.ResponseWriter, req *http.Request) {
	if !r.authorized(w, req) {
		return
	}

	path := req.URL.Path

	// Token endpoint for the Bearer flow.
	if r.opts.Bearer && strings.HasSuffix(path, "/token") {
		r.mu.Lock()
		r.tokenCalls++
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"` + TestBearerToken + `","expires_in":300}`))
		return
	}

	if path == "/v2" || path == "/v2/" {
		r.mu.Lock()
		r.authCalls++
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}

	rest := strings.TrimPrefix(path, "/v2/")
	if repo, ref, ok := splitAround(rest, "/manifests/"); ok {
		r.serveManifest(w, req, repo, ref)
		return
	}
	if repo, digest, ok := splitAround(rest, "/blobs/"); ok {
		r.serveBlob(w, req, repo, digest)
		return
	}
	if repo, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		r.serveTags(w, repo)
		return
	}
	http.NotFound(w, req)
}

// SetTags declares what /v2/<repo>/tags/list reports. Without it the endpoint
// 404s, which is how a registry that does not implement it behaves — that path
// is worth testing too, so this is opt-in rather than derived from the
// registered manifests.
func (r *Registry) SetTags(repo string, tags []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tags == nil {
		r.tags = map[string][]string{}
	}
	r.tags[repo] = append([]string(nil), tags...)
}

// TagsCalls reports how many times tags/list was requested.
func (r *Registry) TagsCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tagsCalls
}

func (r *Registry) serveTags(w http.ResponseWriter, repo string) {
	r.mu.Lock()
	tags, ok := r.tags[repo]
	r.tagsCalls++
	r.mu.Unlock()

	if !ok {
		// Real registries answer an unknown repository with 404 (or 401);
		// either way ListTags must surface an error so the caller falls back.
		http.NotFound(w, nil)
		return
	}
	if tags == nil {
		tags = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
}

// TestBearerToken is the token the fake registry issues and expects.
const TestBearerToken = "registrytest-token"

// authorized enforces the configured auth mode. It writes the challenge and
// returns false when the request is not authorised.
func (r *Registry) authorized(w http.ResponseWriter, req *http.Request) bool {
	if r.opts.Bearer && strings.HasSuffix(req.URL.Path, "/token") {
		return true
	}
	switch {
	case r.opts.Bearer:
		want := "Bearer " + TestBearerToken
		if req.Header.Get("Authorization") == want {
			return true
		}
		realm := r.Server.URL + "/token"
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s",service="registrytest",scope="repository:test:pull"`, realm))
		w.WriteHeader(http.StatusUnauthorized)
		return false

	case r.opts.BasicUser != "":
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(r.opts.BasicUser+":"+r.opts.BasicPass))
		if req.Header.Get("Authorization") == want {
			return true
		}
		// A Basic challenge with no realm parameters is exactly the shape
		// that used to IndexError in the legacy parser.
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
		return false

	default:
		return true
	}
}

func splitAround(s, sep string) (before, after string, ok bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	r.mu.Lock()
	entry, ok := r.manifests[manifestKey(repo, ref)]
	r.mu.Unlock()
	if !ok {
		http.Error(w, "manifest unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", entry.mediaType)
	w.Header().Set("Docker-Content-Digest", Digest(entry.body))
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.body)))
	if req.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(entry.body)
}

func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, repo, digest string) {
	if r.opts.DenyBlobs {
		http.Error(w, "blob unavailable", http.StatusInternalServerError)
		return
	}
	r.mu.Lock()
	blob, ok := r.blobs[digest]
	r.mu.Unlock()
	if !ok {
		http.Error(w, "blob unknown", http.StatusNotFound)
		return
	}
	size := int64(len(blob))

	// HEAD: report the length, no body.
	if req.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	rangeHeader := req.Header.Get("Range")
	if rangeHeader != "" {
		start, end, err := parseRange(rangeHeader, size)
		r.mu.Lock()
		r.requests = append(r.requests, RangeRequest{Digest: digest, Start: start, End: end, Header: rangeHeader})
		r.mu.Unlock()

		switch {
		case err != nil || start >= size:
			// Range unsatisfiable: the client is already at the end.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		case r.opts.IgnoreRange:
			// Pretend we never saw the Range header. A correct client must
			// restart from zero rather than append.
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(blob)
			return
		case r.opts.RejectRanges:
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		default:
			if end < 0 || end >= size {
				end = size - 1
			}
			chunk := blob[start : end+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(chunk)
			return
		}
	}

	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// parseRange parses "bytes=start-" / "bytes=start-end". end is -1 when the
// request was open-ended.
func parseRange(header string, size int64) (int64, int64, error) {
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if spec == "" {
		return 0, -1, fmt.Errorf("empty range")
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, -1, fmt.Errorf("bad range %q", header)
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return 0, -1, err
	}
	if strings.TrimSpace(parts[1]) == "" {
		return start, -1, nil
	}
	end, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return start, -1, err
	}
	if end > size-1 {
		end = size - 1
	}
	return start, end, nil
}

// --- synthetic images ---------------------------------------------------

// Image is a synthetic single-architecture image: one gzipped layer
// containing one file, a config blob and a manifest.
type Image struct {
	Repo        string
	Tag         string
	Arch        string
	OS          string
	FileName    string
	FileContent string

	LayerRawTar    []byte
	LayerGz        []byte
	LayerDigest    string
	ConfigBytes    []byte
	ConfigDigest   string
	ManifestBytes  []byte
	ManifestDigest string
}

// BuildImage synthesises a valid single-layer, single-arch image.
//
// The layer blob is a gzip of a real tar, so the puller's gunzip + repack
// path is exercised for real and the resulting archive can be inspected
// member by member.
func BuildImage(repo, tag, arch, fileName, content string) (*Image, error) {
	img := &Image{
		Repo:        repo,
		Tag:         tag,
		Arch:        arch,
		OS:          "linux",
		FileName:    fileName,
		FileContent: content,
	}

	// A minimal but real tar.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	payload := []byte(content)
	if err := tw.WriteHeader(&tar.Header{
		Name:     fileName,
		Mode:     0o644,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(payload); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	img.LayerRawTar = tarBuf.Bytes()

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(img.LayerRawTar); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	img.LayerGz = gzBuf.Bytes()
	img.LayerDigest = Digest(img.LayerGz)

	config := map[string]any{
		"architecture": arch,
		"os":           img.OS,
		"config":       map[string]any{"Labels": map[string]string{"test.image": repo}},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{Digest(img.LayerRawTar)},
		},
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	img.ConfigBytes = configBytes
	img.ConfigDigest = Digest(configBytes)

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     MediaTypeManifest,
		"config": map[string]any{
			"mediaType": MediaTypeConfig,
			"digest":    img.ConfigDigest,
			"size":      len(img.ConfigBytes),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"digest":    img.LayerDigest,
			"size":      len(img.LayerGz),
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	img.ManifestBytes = manifestBytes
	img.ManifestDigest = Digest(manifestBytes)
	return img, nil
}

// MustBuildImage is BuildImage that panics on error, for test tables.
func MustBuildImage(repo, tag, arch, fileName, content string) *Image {
	img, err := BuildImage(repo, tag, arch, fileName, content)
	if err != nil {
		panic(err)
	}
	return img
}

// RegisterImage adds the image's blobs and its tag manifest.
func (r *Registry) RegisterImage(img *Image) {
	r.AddBlob(img.LayerDigest, img.LayerGz)
	r.AddBlob(img.ConfigDigest, img.ConfigBytes)
	r.AddManifest(img.Repo, img.Tag, MediaTypeManifest, img.ManifestBytes)
}

// RegisterImageByDigest additionally serves the manifest under its own
// digest, which is how an index refers to a sub-manifest.
func (r *Registry) RegisterImageByDigest(img *Image) {
	r.RegisterImage(img)
	r.AddManifest(img.Repo, img.ManifestDigest, MediaTypeManifest, img.ManifestBytes)
}

// BuildIndex builds a multi-arch image index over imgs.
func BuildIndex(repo, tag string, imgs ...*Image) ([]byte, error) {
	entries := make([]map[string]any, 0, len(imgs))
	for _, img := range imgs {
		entries = append(entries, map[string]any{
			"mediaType": MediaTypeManifest,
			"digest":    img.ManifestDigest,
			"size":      len(img.ManifestBytes),
			"platform": map[string]any{
				"architecture": img.Arch,
				"os":           img.OS,
			},
		})
	}
	return json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     MediaTypeIndex,
		"manifests":     entries,
	})
}

// RegisterMultiArch registers several architectures behind one tag.
func (r *Registry) RegisterMultiArch(repo, tag string, imgs ...*Image) ([]byte, error) {
	for _, img := range imgs {
		r.RegisterImageByDigest(img)
	}
	index, err := BuildIndex(repo, tag, imgs...)
	if err != nil {
		return nil, err
	}
	r.AddManifest(repo, tag, MediaTypeIndex, index)
	return index, nil
}

// Digest returns the sha256 digest of b in "sha256:<hex>" form.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ReadTarMembers lists an archive's member names and sizes.
func ReadTarMembers(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	out := make(map[string][]byte)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar %s: %v", path, err)
		}
		if hdr.Typeflag == tar.TypeDir {
			out[hdr.Name] = nil
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read member %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = data
	}
	return out
}
