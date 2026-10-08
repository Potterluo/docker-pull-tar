package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Media types used by the registry API v2.
const (
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerConfig       = "application/vnd.docker.container.image.v1+json"
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIConfig          = "application/vnd.oci.image.config.v1+json"
)

// AcceptManifestTypes is the Accept header sent on manifest requests. The
// order matters to some registries and mirrors: image indexes are listed
// before image manifests so a multi-arch tag yields the index.
const AcceptManifestTypes = MediaTypeOCIIndex + ", " +
	MediaTypeDockerManifestList + ", " +
	MediaTypeOCIManifest + ", " +
	MediaTypeDockerManifest

// Descriptor is one entry of a manifest: the config blob, one layer, or (in
// an index) one platform-specific manifest.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// URLs carries the download locations of a FOREIGN (non-distributable)
	// layer. Registries do not store those blobs, so a pull that asks
	// /v2/<repo>/blobs/<digest> for one gets 403/404 — the bytes live at these
	// URLs instead (Microsoft's CDN for a Windows base image).
	URLs []string `json:"urls,omitempty"`
}

// IsForeign reports whether the layer is a non-distributable "foreign" layer,
// which must be fetched from its own URLs rather than from the registry.
//
// Two spellings mean the same thing: Docker's
// "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip" and OCI's
// "application/vnd.oci.image.layer.nondistributable.*".
func (d Descriptor) IsForeign() bool {
	mt := strings.ToLower(d.MediaType)
	return strings.Contains(mt, "foreign") || strings.Contains(mt, "nondistributable")
}

// ForeignURLs returns the declared locations for a foreign layer, or nil.
func (d Descriptor) ForeignURLs() []string {
	if !d.IsForeign() {
		return nil
	}
	out := make([]string, 0, len(d.URLs))
	for _, u := range d.URLs {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// ShortName is the digest prefix used for display, matching the legacy
// tool's 12-character layer labels.
func (d Descriptor) ShortName() string {
	s := strings.TrimPrefix(d.Digest, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return d.MediaType
	}
	return s
}

// Manifest is either an image index (multi-arch) or an image manifest.
type Manifest struct {
	MediaType string
	Digest    string
	IsIndex   bool
	Config    Descriptor
	Layers    []Descriptor
	Manifests []Descriptor

	// Raw is the exact response body. Its sha256 is the manifest digest
	// when the registry did not supply a Docker-Content-Digest header.
	Raw []byte
}

// PlatformName resolves a manifest entry's platform, preferring the
// bashbrew annotation Docker Hub uses on official images (which is set even
// when `platform` is absent) over the platform object itself.
func (d Descriptor) PlatformName() Platform {
	if d.Annotations != nil {
		if arch := d.Annotations["com.docker.official-images.bashbrew.arch"]; arch != "" {
			p := ParsePlatform(arch)
			return p
		}
	}
	if d.Platform != nil {
		return *d.Platform
	}
	return Platform{}
}

// Platforms lists everything an index actually offers: every entry that
// declares an OS, deduplicated, original order preserved. An image manifest has
// no platforms (its config blob carries the single one — see ConfigPlatform).
//
// It deliberately does NOT filter to Linux. It used to, and that made the tool
// lie about the one case where the list matters most: `hello-world:nanoserver1709`
// is an index with a single windows/amd64 entry, so the picker showed nothing,
// `--list-arch` claimed "single-arch image", and a failed pull reported
// "available architectures:" followed by NOTHING. Filtering belongs in
// ResolveArch, which must refuse a cross-OS match — reporting must be truthful.
func (m *Manifest) Platforms() []Platform {
	if !m.IsIndex {
		return nil
	}
	out := make([]Platform, 0, len(m.Manifests))
	for _, d := range m.Manifests {
		p := d.PlatformName()
		if p.OS == "" {
			continue
		}
		out = append(out, p)
	}
	return DedupePlatforms(out)
}

// EntryFor finds the index entry serving platform.
func (m *Manifest) EntryFor(platform Platform) (Descriptor, bool) {
	if !m.IsIndex {
		return Descriptor{}, false
	}
	available := m.Platforms()
	chosen, ok := ResolveArch(platform.String(), available)
	if !ok {
		return Descriptor{}, false
	}
	for _, d := range m.Manifests {
		if d.PlatformName().Equal(chosen) {
			return d, true
		}
	}
	// Alias spelling difference (arm64v8 vs arm64): fall back to arch-only.
	for _, d := range m.Manifests {
		if CanonicalArch(d.PlatformName().Architecture) == CanonicalArch(chosen.Architecture) &&
			strings.EqualFold(d.PlatformName().OS, chosen.OS) {
			return d, true
		}
	}
	return Descriptor{}, false
}

// TotalLayerBytes sums the layer sizes. Registries must set `size` on each
// descriptor, so this is normally exact; it is 0 when they do not.
func (m *Manifest) TotalLayerBytes() int64 {
	var total int64
	for _, l := range m.Layers {
		total += l.Size
	}
	return total
}

// rawManifest mirrors the on-the-wire JSON. One struct covers index and
// image manifest because the fields are disjoint.
type rawManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        *Descriptor  `json:"config"`
	Layers        []Descriptor `json:"layers"`
	Manifests     []Descriptor `json:"manifests"`
}

// ComputeDigest returns the sha256 digest of a manifest body.
func ComputeDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// parseManifest decodes a manifest body. headerDigest, when non-empty, is
// the registry's Docker-Content-Digest; otherwise the digest is computed
// from the bytes.
//
// Index detection uses BOTH the mediaType and the presence of `manifests`:
// some mirrors return an image-index body under the v2 image-manifest
// content type, which the legacy tool mis-handled.
func parseManifest(raw []byte, headerDigest string) (*Manifest, error) {
	var rm rawManifest
	if err := json.Unmarshal(raw, &rm); err != nil {
		return nil, fmt.Errorf("解析清单失败: %w", err)
	}

	m := &Manifest{
		MediaType: rm.MediaType,
		Raw:       raw,
		Digest:    headerDigest,
		Layers:    rm.Layers,
		Manifests: rm.Manifests,
	}
	if m.Digest == "" {
		m.Digest = ComputeDigest(raw)
	}

	isIndexType := rm.MediaType == MediaTypeOCIIndex || rm.MediaType == MediaTypeDockerManifestList
	m.IsIndex = len(rm.Manifests) > 0 || isIndexType

	if rm.Config != nil {
		m.Config = *rm.Config
	}

	if !m.IsIndex {
		if rm.Config == nil {
			return nil, fmt.Errorf("清单格式不完整：缺少 config 字段")
		}
		if len(rm.Layers) == 0 {
			return nil, fmt.Errorf("清单中没有层")
		}
	} else if len(rm.Manifests) == 0 {
		return nil, fmt.Errorf("清单索引中没有可用的镜像")
	}
	return m, nil
}

// ImageConfig is the subset of the image config blob this tool needs.
type ImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// Platform returns the platform the config blob describes.
func (c *ImageConfig) Platform() Platform {
	return Platform{OS: c.OS, Architecture: c.Architecture, Variant: c.Variant}
}

// ParseImageConfig decodes an image config blob.
func ParseImageConfig(raw []byte) (*ImageConfig, error) {
	var c ImageConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("解析镜像配置失败: %w", err)
	}
	return &c, nil
}
