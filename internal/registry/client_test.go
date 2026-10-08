package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

const testRepo = "library/nginx"

// clientForMock builds a client that talks to the fake registry and never
// consults the environment's proxy settings.
func clientForMock(opts ClientOptions) *Client {
	opts.ProxyURL = "-"
	return NewClient(opts)
}

func refForMock(host, repo, tag string) Ref {
	return Ref{Registry: host, Repository: repo, Tag: tag, UseHTTP: true}
}

// TestScanPlatforms protects the arch picker: a multi-arch tag must report
// the index's entries, a single-arch tag the architecture its config blob
// actually declares.
func TestScanPlatforms(t *testing.T) {
	ctx := context.Background()

	t.Run("multi-arch index", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		amd64 := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "amd64")
		arm64 := registrytest.MustBuildImage(testRepo, "latest", "arm64", "f", "arm64")
		if _, err := mock.RegisterMultiArch(testRepo, "latest", amd64, arm64); err != nil {
			t.Fatalf("RegisterMultiArch: %v", err)
		}

		client := clientForMock(ClientOptions{})
		ref := refForMock(mock.Host(), testRepo, "latest")

		platforms, root, err := client.ScanPlatforms(ctx, ref)
		if err != nil {
			t.Fatalf("ScanPlatforms: %v", err)
		}
		if root == nil || !root.IsIndex {
			t.Fatalf("root manifest = %+v, want the index", root)
		}
		if len(platforms) != 2 {
			t.Fatalf("ScanPlatforms = %+v, want amd64 and arm64", platforms)
		}
		if platforms[0].Architecture != "amd64" || platforms[1].Architecture != "arm64" {
			t.Errorf("ScanPlatforms = %+v, want manifest order", platforms)
		}
	})

	t.Run("single-arch image reads the config blob", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		img := registrytest.MustBuildImage(testRepo, "1.26", "arm64", "f", "arm64")
		mock.RegisterImage(img)

		client := clientForMock(ClientOptions{})
		ref := refForMock(mock.Host(), testRepo, "1.26")

		platforms, root, err := client.ScanPlatforms(ctx, ref)
		if err != nil {
			t.Fatalf("ScanPlatforms: %v", err)
		}
		if root == nil || root.IsIndex {
			t.Fatalf("root manifest = %+v, want the image manifest", root)
		}
		if len(platforms) != 1 {
			t.Fatalf("ScanPlatforms = %+v, want the config blob's single platform", platforms)
		}
		if platforms[0].Architecture != "arm64" || platforms[0].OS != "linux" {
			t.Errorf("ScanPlatforms = %+v, want linux/arm64 from the config blob", platforms)
		}
	})

	t.Run("missing manifest is an error", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		client := clientForMock(ClientOptions{})
		if _, _, err := client.ScanPlatforms(ctx, refForMock(mock.Host(), testRepo, "nope")); err == nil {
			t.Fatal("ScanPlatforms succeeded for an unknown tag")
		}
	})
}

// TestResolveImage protects the index -> sub-manifest hop and, for a
// single-arch image, that the reported platform comes from the image itself
// rather than from what the caller asked for.
func TestResolveImage(t *testing.T) {
	ctx := context.Background()

	t.Run("index resolves to the requested sub-manifest", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		amd64 := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "amd64")
		arm64 := registrytest.MustBuildImage(testRepo, "latest", "arm64", "f", "arm64")
		if _, err := mock.RegisterMultiArch(testRepo, "latest", amd64, arm64); err != nil {
			t.Fatalf("RegisterMultiArch: %v", err)
		}

		client := clientForMock(ClientOptions{})
		ref := refForMock(mock.Host(), testRepo, "latest")

		m, platform, err := client.ResolveImage(ctx, ref, Platform{OS: "linux", Architecture: "arm64"})
		if err != nil {
			t.Fatalf("ResolveImage: %v", err)
		}
		if m.IsIndex {
			t.Fatal("ResolveImage returned the index instead of the sub-manifest")
		}
		if m.Config.Digest != arm64.ConfigDigest {
			t.Errorf("config digest = %q, want the arm64 image's %q", m.Config.Digest, arm64.ConfigDigest)
		}
		if platform.Architecture != "arm64" || platform.OS != "linux" {
			t.Errorf("platform = %+v, want the index entry's linux/arm64", platform)
		}
	})

	t.Run("unknown platform lists what is available", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		amd64 := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "amd64")
		arm64 := registrytest.MustBuildImage(testRepo, "latest", "arm64", "f", "arm64")
		if _, err := mock.RegisterMultiArch(testRepo, "latest", amd64, arm64); err != nil {
			t.Fatalf("RegisterMultiArch: %v", err)
		}

		client := clientForMock(ClientOptions{})
		ref := refForMock(mock.Host(), testRepo, "latest")
		_, _, err := client.ResolveImage(ctx, ref, Platform{OS: "linux", Architecture: "ppc64le"})
		if err == nil {
			t.Fatal("ResolveImage succeeded for an architecture the index does not publish")
		}
		for _, want := range []string{"ppc64le", "linux/amd64", "linux/arm64"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to mention %q", err, want)
			}
		}
	})

	t.Run("single-arch image reports its own platform", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		img := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "amd64")
		mock.RegisterImage(img)

		client := clientForMock(ClientOptions{})
		ref := refForMock(mock.Host(), testRepo, "latest")

		// Ask for arm64: the answer must still be amd64, because that is
		// what the image actually is.
		m, platform, err := client.ResolveImage(ctx, ref, Platform{OS: "linux", Architecture: "arm64"})
		if err != nil {
			t.Fatalf("ResolveImage: %v", err)
		}
		if m.IsIndex {
			t.Fatal("single-arch image reported as an index")
		}
		if platform.Architecture != "amd64" {
			t.Errorf("platform = %+v, want the config's linux/amd64, not the requested arch", platform)
		}
	})

	t.Run("index pointing at another index is refused", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		inner := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "amd64")
		innerIndex, err := registrytest.BuildIndex(testRepo, "latest", inner)
		if err != nil {
			t.Fatalf("BuildIndex: %v", err)
		}
		// A sub-manifest that is itself an index.
		outer := map[string]any{
			"schemaVersion": 2,
			"mediaType":     registrytest.MediaTypeIndex,
			"manifests": []map[string]any{{
				"mediaType": registrytest.MediaTypeIndex,
				"digest":    registrytest.Digest(innerIndex),
				"size":      len(innerIndex),
				"platform":  map[string]any{"os": "linux", "architecture": "amd64"},
			}},
		}
		outerBytes, err := json.Marshal(outer)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		mock.AddManifest(testRepo, "latest", registrytest.MediaTypeIndex, outerBytes)
		mock.AddManifest(testRepo, registrytest.Digest(innerIndex), registrytest.MediaTypeIndex, innerIndex)

		client := clientForMock(ClientOptions{})
		ref := refForMock(mock.Host(), testRepo, "latest")
		if _, _, err := client.ResolveImage(ctx, ref, Platform{OS: "linux", Architecture: "amd64"}); err == nil {
			t.Fatal("ResolveImage followed a nested index")
		} else if !strings.Contains(err.Error(), "索引") {
			t.Errorf("error = %q, want it to explain the nested index", err)
		}
	})
}

// TestFetchBlobBytes protects the config-blob read: the bytes must be
// exactly what the registry served, because the tar's <config>.json member
// is this blob verbatim.
func TestFetchBlobBytes(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "x")
	mock.RegisterImage(img)

	client := clientForMock(ClientOptions{})
	ref := refForMock(mock.Host(), testRepo, "latest")

	got, err := client.FetchBlobBytes(context.Background(), ref, img.ConfigDigest, 4<<20)
	if err != nil {
		t.Fatalf("FetchBlobBytes: %v", err)
	}
	if string(got) != string(img.ConfigBytes) {
		t.Fatalf("FetchBlobBytes returned %d bytes that differ from the registered config blob", len(got))
	}

	cfg, err := client.FetchConfig(context.Background(), ref, img.ConfigDigest)
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if cfg.Architecture != "amd64" || cfg.OS != "linux" {
		t.Errorf("FetchConfig = %+v, want linux/amd64", cfg.Platform())
	}

	if _, err := client.FetchConfig(context.Background(), ref, "sha256:0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("FetchConfig succeeded for an unknown digest")
	}
}

// TestHeadBlob protects the "size unknown is recoverable" contract: a
// registry that will not answer HEAD must not break the pull, and a blob
// that exists must report its stored (compressed) length.
func TestHeadBlob(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "x")
	mock.RegisterImage(img)

	client := clientForMock(ClientOptions{})
	ref := refForMock(mock.Host(), testRepo, "latest")

	n, err := client.HeadBlob(context.Background(), ref, img.LayerDigest)
	if err != nil {
		t.Fatalf("HeadBlob: %v", err)
	}
	if n != int64(len(img.LayerGz)) {
		t.Errorf("HeadBlob = %d, want the gzipped layer length %d", n, len(img.LayerGz))
	}

	// Unknown digest: REPORTED as "no size", not as a hard failure.
	n, err = client.HeadBlob(context.Background(), ref, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("HeadBlob on an unknown digest returned a hard error: %v", err)
	}
	if n != 0 {
		t.Errorf("HeadBlob on an unknown digest = %d, want 0", n)
	}

	// The GET path, by contrast, must surface the 404.
	if _, err := client.OpenBlob(context.Background(), ref, "sha256:1111111111111111111111111111111111111111111111111111111111111111", 0); err == nil {
		t.Fatal("OpenBlob on an unknown digest succeeded")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want the 404 status to be surfaced", err)
	}
}

// TestOpenBlobRanges protects the byte-range contract the resume path is
// built on: 200 for a full body, 206 for a partial one, 416 when the offset
// is already at or past the end.
func TestOpenBlobRanges(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "some file content")
	mock.RegisterImage(img)
	size := int64(len(img.LayerGz))

	client := clientForMock(ClientOptions{})
	ref := refForMock(mock.Host(), testRepo, "latest")
	ctx := context.Background()

	readAll := func(t *testing.T, br *BlobReader) []byte {
		t.Helper()
		defer func() { _ = br.Body.Close() }()
		b, err := io.ReadAll(br.Body)
		if err != nil {
			t.Fatalf("read blob body: %v", err)
		}
		return b
	}

	t.Run("offset 0 is a full 200", func(t *testing.T) {
		br, err := client.OpenBlob(ctx, ref, img.LayerDigest, 0)
		if err != nil {
			t.Fatalf("OpenBlob: %v", err)
		}
		if br.StatusCode != http.StatusOK {
			t.Errorf("StatusCode = %d, want 200", br.StatusCode)
		}
		if br.TotalSize != size {
			t.Errorf("TotalSize = %d, want %d", br.TotalSize, size)
		}
		if got := readAll(t, br); string(got) != string(img.LayerGz) {
			t.Fatalf("body was %d bytes, want the full %d", len(got), size)
		}
	})

	t.Run("offset yields 206 and the tail", func(t *testing.T) {
		const offset = 17
		br, err := client.OpenBlob(ctx, ref, img.LayerDigest, offset)
		if err != nil {
			t.Fatalf("OpenBlob: %v", err)
		}
		if br.StatusCode != http.StatusPartialContent {
			t.Errorf("StatusCode = %d, want 206", br.StatusCode)
		}
		if br.TotalSize != size {
			t.Errorf("TotalSize = %d, want %d from Content-Range", br.TotalSize, size)
		}
		got := readAll(t, br)
		if string(got) != string(img.LayerGz[offset:]) {
			t.Fatalf("body = %d bytes, want the %d-byte tail from offset %d", len(got), size-offset, offset)
		}

		reqs := mock.RangeRequests()
		if len(reqs) != 1 {
			t.Fatalf("RangeRequests = %+v, want exactly one", reqs)
		}
		if reqs[0].Header != "bytes=17-" {
			t.Errorf("Range header = %q, want bytes=17-", reqs[0].Header)
		}
	})

	t.Run("closed range", func(t *testing.T) {
		mock.ResetRequests()
		br, err := client.OpenBlobRange(ctx, ref, img.LayerDigest, 5, 9)
		if err != nil {
			t.Fatalf("OpenBlobRange: %v", err)
		}
		if br.StatusCode != http.StatusPartialContent {
			t.Errorf("StatusCode = %d, want 206", br.StatusCode)
		}
		if got := readAll(t, br); string(got) != string(img.LayerGz[5:10]) {
			t.Fatalf("body = %d bytes, want bytes 5-9", len(got))
		}
		if reqs := mock.RangeRequests(); len(reqs) != 1 || reqs[0].Header != "bytes=5-9" {
			t.Errorf("RangeRequests = %+v, want bytes=5-9", reqs)
		}
	})

	t.Run("offset at the end is 416", func(t *testing.T) {
		br, err := client.OpenBlob(ctx, ref, img.LayerDigest, size)
		if err != nil {
			t.Fatalf("OpenBlob at EOF: %v", err)
		}
		defer func() { _ = br.Body.Close() }()
		if br.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("StatusCode = %d, want 416", br.StatusCode)
		}
	})

	t.Run("offset past the end is 416", func(t *testing.T) {
		br, err := client.OpenBlob(ctx, ref, img.LayerDigest, size+100)
		if err != nil {
			t.Fatalf("OpenBlob past EOF: %v", err)
		}
		defer func() { _ = br.Body.Close() }()
		if br.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("StatusCode = %d, want 416", br.StatusCode)
		}
	})
}

// TestOpenBlobServerIgnoresRange is the guard the 1ms fork lacked: a mirror
// that answers a Range request with a full 200 must be reported as 200, so
// the puller truncates its partial file instead of splicing the whole blob
// onto it.
func TestOpenBlobServerIgnoresRange(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{IgnoreRange: true})
	img := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "content")
	mock.RegisterImage(img)

	client := clientForMock(ClientOptions{})
	ref := refForMock(mock.Host(), testRepo, "latest")

	br, err := client.OpenBlob(context.Background(), ref, img.LayerDigest, 10)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer func() { _ = br.Body.Close() }()
	if br.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200 so the caller knows to restart from zero", br.StatusCode)
	}
	if len(mock.RangeRequests()) != 1 {
		t.Fatalf("the Range header was never sent: %+v", mock.RangeRequests())
	}
	body, err := io.ReadAll(br.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != len(img.LayerGz) {
		t.Fatalf("body = %d bytes, want the whole %d-byte blob", len(body), len(img.LayerGz))
	}
}

// TestOpenBlobRejectRangesReturns416 covers the pathological registry that
// answers every Range with 416: fetchChunk treats that as "already there",
// and OpenBlob must not turn it into a transport error.
func TestOpenBlobRejectRangesReturns416(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{RejectRanges: true})
	img := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "content")
	mock.RegisterImage(img)

	client := clientForMock(ClientOptions{})
	ref := refForMock(mock.Host(), testRepo, "latest")

	br, err := client.OpenBlobRange(context.Background(), ref, img.LayerDigest, 5, 9)
	if err != nil {
		t.Fatalf("OpenBlobRange: %v", err)
	}
	defer func() { _ = br.Body.Close() }()
	if br.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("StatusCode = %d, want 416", br.StatusCode)
	}
	if br.TotalSize != int64(len(img.LayerGz)) {
		t.Errorf("TotalSize = %d, want %d from the 416 Content-Range", br.TotalSize, len(img.LayerGz))
	}
}

// TestDoAuthedRetriesOnceOn401 protects the stale-token recovery: the first
// authenticated attempt is rejected with 401, the client drops its cached
// credential, renegotiates and succeeds on the second try.
func TestDoAuthedRetriesOnceOn401(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{Bearer: true})
	img := registrytest.MustBuildImage("test", "latest", "amd64", "f", "x")
	mock.RegisterImage(img)

	registryHandler := mock.Config.Handler
	var (
		mu           sync.Mutex
		manifestHits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			mu.Lock()
			manifestHits++
			hit := manifestHits
			mu.Unlock()
			if hit == 1 {
				// A stale cached token: reject once, challenge again.
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(
					`Bearer realm="%s/token",service="registrytest",scope="repository:test:pull"`, mock.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		registryHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	client := clientForMock(ClientOptions{})
	ref := refForMock(strings.TrimPrefix(srv.URL, "http://"), "test", "latest")

	m, err := client.FetchManifest(context.Background(), ref, "")
	if err != nil {
		t.Fatalf("FetchManifest did not recover from the 401: %v", err)
	}
	if m.IsIndex {
		t.Errorf("manifest = %+v, want the image manifest", m)
	}

	mu.Lock()
	hits := manifestHits
	mu.Unlock()
	if hits != 2 {
		t.Errorf("manifest requests = %d, want exactly 2 (one rejected, one retried)", hits)
	}
	if mock.TokenCalls() < 2 {
		t.Errorf("token calls = %d, want the credential to be renegotiated", mock.TokenCalls())
	}

	// A second call must hit the (now valid) cache and not renegotiate.
	mu.Lock()
	hits = manifestHits
	mu.Unlock()
	if _, err := client.FetchManifest(context.Background(), ref, ""); err != nil {
		t.Fatalf("second FetchManifest: %v", err)
	}
	mu.Lock()
	if manifestHits != hits+1 {
		t.Errorf("second FetchManifest issued %d requests, want 1", manifestHits-hits)
	}
	mu.Unlock()
}

// TestInsecureTLS protects the verify_tls setting: verification is ON by
// default and only skipped when the caller explicitly opts out (the legacy
// tool hard-disabled it with no way back).
func TestInsecureTLS(t *testing.T) {
	img := registrytest.MustBuildImage(testRepo, "latest", "amd64", "f", "x")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2" || r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", MediaTypeDockerManifest)
			w.Header().Set("Docker-Content-Digest", registrytest.Digest(img.ManifestBytes))
			_, _ = w.Write(img.ManifestBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ref := Ref{Registry: strings.TrimPrefix(srv.URL, "https://"), Repository: testRepo, Tag: "latest"}
	ctx := context.Background()

	t.Run("verification is on by default", func(t *testing.T) {
		client := clientForMock(ClientOptions{})
		if _, err := client.FetchManifest(ctx, ref, ""); err == nil {
			t.Fatal("FetchManifest against a self-signed TLS registry succeeded without InsecureTLS")
		} else if !strings.Contains(err.Error(), "证书") && !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
			t.Errorf("error = %q, want a TLS verification failure", err)
		}
	})

	t.Run("InsecureTLS skips verification", func(t *testing.T) {
		client := clientForMock(ClientOptions{InsecureTLS: true})
		m, err := client.FetchManifest(ctx, ref, "")
		if err != nil {
			t.Fatalf("FetchManifest with InsecureTLS: %v", err)
		}
		if m.IsIndex || len(m.Layers) != 1 {
			t.Errorf("manifest = %+v, want the single image manifest", m)
		}
	})
}
