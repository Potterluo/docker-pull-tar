package registry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// testIndexJSON hand-builds a manifest index so the tests can exercise
// shapes registrytest.BuildIndex does not produce (annotation-only entries,
// windows entries, media-type lies).
func testIndexJSON(t *testing.T, mediaType string, entries ...map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"schemaVersion": 2,
		"manifests":     entries,
	}
	if mediaType != "" {
		doc["mediaType"] = mediaType
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	return raw
}

// TestParseManifestImageManifest protects the single-arch shape the puller
// resolves before it downloads anything.
func TestParseManifestImageManifest(t *testing.T) {
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "index.html", "hello")

	m, err := parseManifest(img.ManifestBytes, "")
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	if m.IsIndex {
		t.Fatal("an image manifest was reported as an index")
	}
	if m.MediaType != MediaTypeDockerManifest {
		t.Errorf("MediaType = %q, want %q", m.MediaType, MediaTypeDockerManifest)
	}
	if m.Config.Digest != img.ConfigDigest {
		t.Errorf("Config.Digest = %q, want %q", m.Config.Digest, img.ConfigDigest)
	}
	if m.Config.Size != int64(len(img.ConfigBytes)) {
		t.Errorf("Config.Size = %d, want %d", m.Config.Size, len(img.ConfigBytes))
	}
	if len(m.Layers) != 1 {
		t.Fatalf("len(Layers) = %d, want 1", len(m.Layers))
	}
	if m.Layers[0].Digest != img.LayerDigest {
		t.Errorf("Layers[0].Digest = %q, want %q", m.Layers[0].Digest, img.LayerDigest)
	}
	if got := m.Platforms(); got != nil {
		t.Errorf("Platforms() = %+v, want nil for an image manifest (the config blob carries it)", got)
	}
	if m.Digest != ComputeDigest(img.ManifestBytes) {
		t.Errorf("Digest = %q, want the computed digest %q", m.Digest, ComputeDigest(img.ManifestBytes))
	}
	if len(m.Raw) != len(img.ManifestBytes) {
		t.Errorf("Raw was not preserved (%d bytes, want %d)", len(m.Raw), len(img.ManifestBytes))
	}
	if m.TotalLayerBytes() != int64(len(img.LayerGz)) {
		t.Errorf("TotalLayerBytes() = %d, want %d", m.TotalLayerBytes(), len(img.LayerGz))
	}

	t.Run("a registry-supplied digest wins", func(t *testing.T) {
		m, err := parseManifest(img.ManifestBytes, "sha256:header")
		if err != nil {
			t.Fatalf("parseManifest: %v", err)
		}
		if m.Digest != "sha256:header" {
			t.Errorf("Digest = %q, want the Docker-Content-Digest header value", m.Digest)
		}
	})
}

// TestParseManifestIndex protects multi-arch detection and the platform list
// the arch picker renders: linux entries only, in manifest order, deduped.
func TestParseManifestIndex(t *testing.T) {
	amd64 := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "f", "amd64")
	arm64 := registrytest.MustBuildImage("library/nginx", "latest", "arm64", "f", "arm64")
	arm64Again := registrytest.MustBuildImage("library/nginx", "latest", "arm64", "g", "arm64-again")

	raw, err := registrytest.BuildIndex("library/nginx", "latest", amd64, arm64, arm64Again)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	m, err := parseManifest(raw, "")
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	if !m.IsIndex {
		t.Fatal("an index was reported as a single image manifest")
	}
	if m.MediaType != registrytest.MediaTypeIndex {
		t.Errorf("MediaType = %q, want %q", m.MediaType, registrytest.MediaTypeIndex)
	}
	if len(m.Manifests) != 3 {
		t.Fatalf("len(Manifests) = %d, want 3", len(m.Manifests))
	}

	got := m.Platforms()
	if len(got) != 2 {
		t.Fatalf("Platforms() = %+v, want the deduped linux pair", got)
	}
	if got[0].Architecture != "amd64" || got[1].Architecture != "arm64" {
		t.Errorf("Platforms() = %+v, want amd64 then arm64 (manifest order)", got)
	}

	t.Run("windows entries are reported, not hidden", func(t *testing.T) {
		// This subtest used to assert the opposite ("hidden from the picker").
		// That filtering was the bug: an index whose ONLY entry is windows
		// (hello-world:nanoserver1709) reported no platforms at all, so the
		// picker offered nothing, --list-arch claimed "single-arch image", and
		// the failure read "available architectures:" followed by nothing.
		// Refusing a windows entry is ResolveArch's job, not the reporter's.
		raw := testIndexJSON(t, MediaTypeDockerManifestList,
			map[string]any{"mediaType": MediaTypeDockerManifest, "digest": "sha256:1", "size": 1,
				"platform": map[string]any{"os": "windows", "architecture": "amd64"}},
			map[string]any{"mediaType": MediaTypeDockerManifest, "digest": "sha256:2", "size": 1,
				"platform": map[string]any{"os": "linux", "architecture": "amd64"}},
		)
		m, err := parseManifest(raw, "")
		if err != nil {
			t.Fatalf("parseManifest: %v", err)
		}
		got := m.Platforms()
		if len(got) != 2 {
			t.Fatalf("Platforms() = %+v, want BOTH entries so the list is truthful", got)
		}
		if got[0].OS != "windows" || got[1].OS != "linux" {
			t.Errorf("Platforms() = %+v, want windows then linux (manifest order)", got)
		}
		// The safety half lives in the resolver: a linux/amd64 request must
		// still refuse, even though a windows/amd64 entry is on the list.
		if p, ok := ResolveArch("linux/amd64", got); !ok || p.OS != "linux" {
			t.Errorf("ResolveArch(linux/amd64) = %+v,%v; want the linux entry", p, ok)
		}
	})

	t.Run("an index with only windows entries still lists them", func(t *testing.T) {
		raw := testIndexJSON(t, MediaTypeDockerManifestList,
			map[string]any{"mediaType": MediaTypeDockerManifest, "digest": "sha256:1", "size": 1,
				"platform": map[string]any{"os": "windows", "architecture": "amd64"}},
		)
		m, err := parseManifest(raw, "")
		if err != nil {
			t.Fatalf("parseManifest: %v", err)
		}
		got := m.Platforms()
		if len(got) != 1 || got[0].String() != "windows/amd64" {
			t.Fatalf("Platforms() = %+v, want exactly windows/amd64", got)
		}
		// Explicitly asking for it works (and is the only way to pull it).
		if p, ok := ResolveArch("windows/amd64", got); !ok || p.String() != "windows/amd64" {
			t.Errorf("ResolveArch(windows/amd64) = %+v,%v; want a match", p, ok)
		}
		// A linux request must NOT be served the windows image.
		if p, ok := ResolveArch("linux/amd64", got); ok {
			t.Errorf("ResolveArch(linux/amd64) matched %+v; a cross-OS match produces an unusable tar", p)
		}
	})

	t.Run("entries without a platform object are skipped", func(t *testing.T) {
		raw := testIndexJSON(t, MediaTypeDockerManifestList,
			map[string]any{"mediaType": MediaTypeDockerManifest, "digest": "sha256:1", "size": 1},
			map[string]any{"mediaType": MediaTypeDockerManifest, "digest": "sha256:2", "size": 1,
				"platform": map[string]any{"os": "linux", "architecture": "amd64"}},
		)
		m, err := parseManifest(raw, "")
		if err != nil {
			t.Fatalf("parseManifest: %v", err)
		}
		if got := m.Platforms(); len(got) != 1 || got[0].Architecture != "amd64" {
			t.Fatalf("Platforms() = %+v, want only the entry with a platform", got)
		}
	})
}

// TestParseManifestIndexUnderWrongContentType is the misleading-media-type
// regression: mirrors serve an image-index body while advertising the v2
// image-manifest content type, and the legacy tool then treated the index as
// a single image (and shipped the index bytes as a layer).
func TestParseManifestIndexUnderWrongContentType(t *testing.T) {
	amd64 := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "f", "amd64")
	arm64 := registrytest.MustBuildImage("library/nginx", "latest", "arm64", "f", "arm64")
	realIndex, err := registrytest.BuildIndex("library/nginx", "latest", amd64, arm64)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "body claims to be an image manifest",
			raw: testIndexJSON(t, MediaTypeDockerManifest,
				map[string]any{"mediaType": MediaTypeDockerManifest, "digest": amd64.ManifestDigest, "size": len(amd64.ManifestBytes),
					"platform": map[string]any{"os": "linux", "architecture": "amd64"}},
				map[string]any{"mediaType": MediaTypeDockerManifest, "digest": arm64.ManifestDigest, "size": len(arm64.ManifestBytes),
					"platform": map[string]any{"os": "linux", "architecture": "arm64"}},
			),
		},
		{
			name: "body carries no media type at all",
			raw: testIndexJSON(t, "",
				map[string]any{"mediaType": MediaTypeDockerManifest, "digest": amd64.ManifestDigest, "size": len(amd64.ManifestBytes),
					"platform": map[string]any{"os": "linux", "architecture": "amd64"}},
				map[string]any{"mediaType": MediaTypeDockerManifest, "digest": arm64.ManifestDigest, "size": len(arm64.ManifestBytes),
					"platform": map[string]any{"os": "linux", "architecture": "arm64"}},
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseManifest(tc.raw, "")
			if err != nil {
				t.Fatalf("parseManifest: %v", err)
			}
			if !m.IsIndex {
				t.Fatalf("IsIndex = false; an index body must be detected from its manifests list, not its media type")
			}
			if got := m.Platforms(); len(got) != 2 {
				t.Fatalf("Platforms() = %+v, want both architectures", got)
			}
		})
	}

	// The reverse lie: image-manifest content under the index media type.
	t.Run("index media type without manifests is rejected", func(t *testing.T) {
		if _, err := parseManifest(realIndex[:0], ""); err == nil {
			t.Fatal("an empty body was accepted as a manifest")
		}
		raw := testIndexJSON(t, MediaTypeOCIIndex)
		if _, err := parseManifest(raw, ""); err == nil {
			t.Fatal("an index with no manifests was accepted")
		} else if !strings.Contains(err.Error(), "索引") {
			t.Errorf("error = %q, want it to explain the empty index", err)
		}
	})

	// registrytest's real index body must still be recognised.
	if m, err := parseManifest(realIndex, ""); err != nil || !m.IsIndex {
		t.Fatalf("registrytest.BuildIndex output was not recognised: %v", err)
	}
}

// TestManifestEntryFor protects the arch -> index-entry mapping the puller
// uses to pick a sub-manifest, including the annotation-only official-image
// entries Docker Hub publishes.
func TestManifestEntryFor(t *testing.T) {
	amd64 := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "f", "amd64")
	arm64 := registrytest.MustBuildImage("library/nginx", "latest", "arm64", "f", "arm64")
	raw, err := registrytest.BuildIndex("library/nginx", "latest", amd64, arm64)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	m, err := parseManifest(raw, "")
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}

	t.Run("amd64", func(t *testing.T) {
		d, ok := m.EntryFor(Platform{OS: "linux", Architecture: "amd64"})
		if !ok {
			t.Fatal("EntryFor(linux/amd64) found nothing")
		}
		if d.Digest != amd64.ManifestDigest {
			t.Errorf("EntryFor(linux/amd64).Digest = %q, want %q", d.Digest, amd64.ManifestDigest)
		}
	})

	t.Run("arm64", func(t *testing.T) {
		d, ok := m.EntryFor(Platform{OS: "linux", Architecture: "arm64"})
		if !ok {
			t.Fatal("EntryFor(linux/arm64) found nothing")
		}
		if d.Digest != arm64.ManifestDigest {
			t.Errorf("EntryFor(linux/arm64).Digest = %q, want %q", d.Digest, arm64.ManifestDigest)
		}
	})

	t.Run("unknown architecture", func(t *testing.T) {
		if d, ok := m.EntryFor(Platform{OS: "linux", Architecture: "ppc64le"}); ok {
			t.Fatalf("EntryFor(linux/ppc64le) = %+v, want no entry", d)
		}
	})

	t.Run("bashbrew annotation stands in for the platform object", func(t *testing.T) {
		raw := testIndexJSON(t, MediaTypeDockerManifestList,
			map[string]any{
				"mediaType": MediaTypeDockerManifest, "digest": amd64.ManifestDigest, "size": len(amd64.ManifestBytes),
				"annotations": map[string]string{"com.docker.official-images.bashbrew.arch": "amd64"},
			},
			map[string]any{
				"mediaType": MediaTypeDockerManifest, "digest": arm64.ManifestDigest, "size": len(arm64.ManifestBytes),
				"annotations": map[string]string{"com.docker.official-images.bashbrew.arch": "arm64v8"},
			},
		)
		idx, err := parseManifest(raw, "")
		if err != nil {
			t.Fatalf("parseManifest: %v", err)
		}
		if got := idx.Platforms(); len(got) != 2 || got[0].Architecture != "amd64" || got[1].Architecture != "arm64v8" {
			t.Fatalf("Platforms() = %+v, want the annotation-derived pair", got)
		}
		if d, ok := idx.EntryFor(Platform{OS: "linux", Architecture: "amd64"}); !ok || d.Digest != amd64.ManifestDigest {
			t.Errorf("EntryFor(amd64) = %+v, %v", d, ok)
		}
		// arm64 must resolve to the arm64v8-spelled entry.
		if d, ok := idx.EntryFor(Platform{OS: "linux", Architecture: "arm64"}); !ok || d.Digest != arm64.ManifestDigest {
			t.Errorf("EntryFor(arm64) = %+v, %v; want the arm64v8 entry", d, ok)
		}
	})

	t.Run("an image manifest has no entries", func(t *testing.T) {
		single, err := parseManifest(amd64.ManifestBytes, "")
		if err != nil {
			t.Fatalf("parseManifest: %v", err)
		}
		if d, ok := single.EntryFor(Platform{OS: "linux", Architecture: "amd64"}); ok {
			t.Fatalf("EntryFor on an image manifest = %+v, want false", d)
		}
	})
}

// TestParseManifestIncompleteBodies protects the early failures: an
// unusable manifest must be rejected before any layer is scheduled.
func TestParseManifestIncompleteBodies(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{
			name:    "not JSON at all",
			raw:     "{not json",
			wantSub: "解析清单失败",
		},
		{
			name:    "no config field",
			raw:     `{"schemaVersion":2,"mediaType":"` + MediaTypeDockerManifest + `","layers":[{"digest":"sha256:x","size":1}]}`,
			wantSub: "缺少 config",
		},
		{
			name:    "no layers",
			raw:     `{"schemaVersion":2,"mediaType":"` + MediaTypeDockerManifest + `","config":{"digest":"sha256:c","size":1}}`,
			wantSub: "没有层",
		},
		{
			name:    "index with no entries",
			raw:     `{"schemaVersion":2,"mediaType":"` + MediaTypeOCIIndex + `","manifests":[]}`,
			wantSub: "索引",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseManifest([]byte(tc.raw), "")
			if err == nil {
				t.Fatalf("parseManifest accepted an unusable body: %+v", m)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// TestDescriptorShortName protects the 12-character layer label the legacy
// UI showed and the puller still writes into progress/error messages.
func TestDescriptorShortName(t *testing.T) {
	long := "sha256:" + testDigestHex
	tests := []struct {
		name string
		desc Descriptor
		want string
	}{
		{"long digest", Descriptor{Digest: long}, testDigestHex[:12]},
		{"short digest", Descriptor{Digest: "sha256:abc"}, "abc"},
		{"no algorithm prefix", Descriptor{Digest: testDigestHex}, testDigestHex[:12]},
		{"missing digest falls back to the media type", Descriptor{MediaType: MediaTypeDockerConfig}, MediaTypeDockerConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.desc.ShortName(); got != tc.want {
				t.Errorf("ShortName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestComputeDigestStability protects the fallback digest used when a
// registry omits Docker-Content-Digest: it must be the sha256 of the exact
// bytes and nothing else.
func TestComputeDigestStability(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	first := ComputeDigest(body)
	if first != ComputeDigest(body) {
		t.Fatal("ComputeDigest is not deterministic")
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("ComputeDigest = %q, want sha256:<64 hex>", first)
	}
	if first == ComputeDigest([]byte(`{"schemaVersion":3}`)) {
		t.Fatal("ComputeDigest collided for different bodies")
	}
	// Must match the registry-side helper the fake registry uses.
	if first != registrytest.Digest(body) {
		t.Errorf("ComputeDigest = %q, registrytest.Digest = %q", first, registrytest.Digest(body))
	}
}

// TestParseImageConfig protects the config-blob reader the single-arch
// platform label depends on.
func TestParseImageConfig(t *testing.T) {
	raw := []byte(`{
		"architecture":"arm64v8",
		"os":"linux",
		"variant":"v8",
		"config":{"Labels":{"maintainer":"someone"}},
		"rootfs":{"type":"layers","diff_ids":["sha256:aaa"]}
	}`)
	cfg, err := ParseImageConfig(raw)
	if err != nil {
		t.Fatalf("ParseImageConfig: %v", err)
	}
	want := Platform{OS: "linux", Architecture: "arm64v8", Variant: "v8"}
	if got := cfg.Platform(); got != want {
		t.Errorf("Platform() = %+v, want %+v", got, want)
	}
	if cfg.Config.Labels["maintainer"] != "someone" {
		t.Errorf("labels not decoded: %+v", cfg.Config.Labels)
	}
	if len(cfg.RootFS.DiffIDs) != 1 || cfg.RootFS.DiffIDs[0] != "sha256:aaa" {
		t.Errorf("diff_ids not decoded: %+v", cfg.RootFS.DiffIDs)
	}

	if _, err := ParseImageConfig([]byte("not json")); err == nil {
		t.Error("ParseImageConfig accepted garbage")
	}

	// An empty config is still a config: Platform() reports the zero value
	// and the caller decides what to do with it.
	cfg, err = ParseImageConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParseImageConfig({}): %v", err)
	}
	if !cfg.Platform().IsZero() {
		t.Errorf("Platform() = %+v, want the zero platform", cfg.Platform())
	}
}
