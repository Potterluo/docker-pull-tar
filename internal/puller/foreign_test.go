package puller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// TestPullFetchesForeignLayerFromItsURLs covers the Windows base-image case.
//
// A foreign (non-distributable) layer is NOT stored in the registry — a Windows
// image declares its base layers with a `urls` array pointing at Microsoft's
// CDN. Asking /v2/<repo>/blobs/<digest> for one answers 403 AccessDenied, which
// is exactly how `hello-world:nanoserver1709` failed on layer b7914a074279
// after layer 407ada6e90de happened to be cached. The descriptor's `urls` used
// to be dropped at JSON-parse time, so the pull had nowhere else to look.
//
// The registry here deliberately does NOT have the layer blob: if the pull
// succeeds, the bytes can only have come from the URL.
func TestPullFetchesForeignLayerFromItsURLs(t *testing.T) {
	const (
		repo    = "library/hello-world"
		tag     = "nanoserver1709"
		content = "windows base layer bytes"
	)

	img := registrytest.MustBuildImage(repo, tag, "amd64", "kernel32.dll", content)

	// A stand-in for the Microsoft CDN. http.ServeContent honours Range, so the
	// chunked path is exercised too when the size crosses the threshold.
	var hits atomic.Int64
	var servedAuth atomic.Value
	servedAuth.Store("")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Record whether a registry credential was leaked to the CDN.
		servedAuth.Store(r.Header.Get("Authorization"))
		http.ServeContent(w, r, "layer.tar.gz", time.Time{}, bytes.NewReader(img.LayerGz))
	}))
	defer cdn.Close()

	// Rewrite the manifest so its only layer is foreign and lives at the CDN.
	var manifest map[string]any
	if err := json.Unmarshal(img.ManifestBytes, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	manifest["layers"] = []map[string]any{{
		"mediaType": "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
		"digest":    img.LayerDigest,
		"size":      len(img.LayerGz),
		"urls":      []string{cdn.URL + "/fwlink/860906"},
	}}
	rewritten, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	mock := registrytest.New(t, registrytest.Options{})
	// Config + manifest only. The layer blob is intentionally absent.
	mock.AddBlob(img.ConfigDigest, img.ConfigBytes)
	mock.AddManifest(repo, tag, registrytest.MediaTypeManifest, rewritten)

	workDir := t.TempDir()
	res, err := Pull(context.Background(), Options{
		Ref:        mockRef(mock.Host(), repo, tag),
		Platform:   testPlatform("amd64"),
		WorkDir:    workDir,
		MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if got := hits.Load(); got == 0 {
		t.Fatal("the CDN was never contacted; the foreign layer was not fetched from its urls")
	}
	if got, _ := servedAuth.Load().(string); got != "" {
		t.Errorf("sent Authorization=%q to the CDN; a registry credential must never leave for a third-party host", got)
	}

	// The bytes must be the real layer, not just something that downloaded.
	blobPath := blobFilePath(workDir, img.LayerDigest)
	sealed, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read sealed blob: %v", err)
	}
	if !bytes.Equal(sealed, img.LayerGz) {
		t.Errorf("sealed foreign layer = %d bytes, want the %d-byte original", len(sealed), len(img.LayerGz))
	}

	// And it must reach the archive, so the offline machine gets a loadable set.
	members := registrytest.ReadTarMembers(t, res.TarPath)
	layerID := layerIDFor("", img.LayerDigest)
	data, ok := members[layerID+"/layer.tar"]
	if !ok {
		t.Fatalf("archive has no %s/layer.tar (members: %v); the foreign layer was downloaded but not packed", layerID, memberNames(members))
	}
	// layer.tar is the UNCOMPRESSED filesystem the gzip blob held.
	if !strings.Contains(string(data), content) {
		t.Errorf("layer.tar does not contain the foreign layer's payload (%d bytes)", len(data))
	}
	if strings.Contains(string(data), "404") && !strings.Contains(string(data), content) {
		t.Errorf("layer.tar looks like an error page rather than the layer")
	}
}

// TestForeignLayerDetection pins both spellings of "not in the registry" and
// the refusal to treat an ordinary layer as foreign (which would send a normal
// pull to an empty URL list).
func TestForeignLayerDetection(t *testing.T) {
	for _, tc := range []struct {
		mediaType string
		urls      []string
		foreign   bool
	}{
		{"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip", []string{"https://example.test/l"}, true},
		{"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip", []string{"https://example.test/l"}, true},
		{"application/vnd.docker.image.rootfs.diff.tar.gzip", nil, false},
		{"application/vnd.oci.image.layer.v1.tar+gzip", nil, false},
		{"", nil, false},
		// A URL list on an ordinary layer is meaningless and must not divert it.
		{"application/vnd.docker.image.rootfs.diff.tar.gzip", []string{"https://example.test/l"}, false},
	} {
		desc := registry.Descriptor{MediaType: tc.mediaType, URLs: tc.urls}
		if got := desc.IsForeign(); got != tc.foreign {
			t.Errorf("IsForeign(%q) = %v, want %v", tc.mediaType, got, tc.foreign)
		}
		if got := desc.ForeignURLs(); (len(got) > 0) != (tc.foreign && len(tc.urls) > 0) {
			t.Errorf("ForeignURLs(%q) = %v, want %v for a foreign layer", tc.mediaType, got, tc.urls)
		}
	}
	// Blank entries are dropped rather than attempted.
	desc := registry.Descriptor{
		MediaType: "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
		URLs:      []string{"  ", "", "https://example.test/real"},
	}
	if got := desc.ForeignURLs(); len(got) != 1 || got[0] != "https://example.test/real" {
		t.Errorf("ForeignURLs() = %v, want just the real one", got)
	}
}

// TestListTagsDoesNotNeedTheTagToExist pins a bug my own end-to-end check found:
// `dockerpull tags -i 127.0.0.1:5000/nginx` failed with
//
//	获取清单 latest 失败 (404): manifest unknown: unknown tag=latest
//
// because the tag listing went through InspectImage, which resolves the ref's
// manifest. A tag listing is about the REPOSITORY, so a repository whose
// `latest` is absent must still list the tags it has.
func TestListTagsDoesNotNeedTheTagToExist(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	// Registered under "alpine" only: `latest` genuinely does not exist.
	img := registrytest.MustBuildImage("library/nginx", "alpine", "amd64", "index.html", "no-latest-tag")
	mock.RegisterImage(img)
	mock.SetTags("library/nginx", []string{"alpine"})

	ref := registry.Ref{
		Registry: mock.Host(), Repository: "library/nginx", Tag: "latest", UseHTTP: true,
	}
	tags, from, err := ListTags(context.Background(), Options{Ref: ref})
	if err != nil {
		t.Fatalf("ListTags: %v — listing tags must not require the ref's tag to exist", err)
	}
	if len(tags) != 1 || tags[0] != "alpine" {
		t.Errorf("ListTags = %v, want [alpine]", tags)
	}
	if from != mock.Host() {
		t.Errorf("tagsFrom = %q, want %q", from, mock.Host())
	}
}
