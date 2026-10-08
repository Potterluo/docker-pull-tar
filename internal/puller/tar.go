package puller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
)

// gzipMagic is the two-byte gzip header.
var gzipMagic = []byte{0x1f, 0x8b}

// v1LayerJSON is the legacy per-layer metadata file `docker load` reads
// alongside each layer.
type v1LayerJSON struct {
	ID     string  `json:"id"`
	Parent *string `json:"parent"`
}

// saveManifestEntry is one element of the tar's manifest.json.
type saveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// LayerChain is one layer's assigned identity in the exported tar.
type LayerChain struct {
	Descriptor registry.Descriptor
	ID         string // fake layer id (the v1 chain id)
	Parent     string // previous layer's id, "" for the first
	Dir        string // "<id>" relative to the tar root
}

// chainID computes the legacy v1 layer id:
// sha256("<parentID>\n<blobDigest>\n"). `docker load` uses this to rebuild
// the layer graph, so it must be computed exactly this way.
func chainID(parentID, blobDigest string) string {
	sum := sha256.Sum256([]byte(parentID + "\n" + blobDigest + "\n"))
	return hex.EncodeToString(sum[:])
}

// buildChain assigns every layer its id and parent.
func buildChain(layers []registry.Descriptor) []LayerChain {
	out := make([]LayerChain, 0, len(layers))
	parent := ""
	for _, d := range layers {
		id := chainID(parent, d.Digest)
		out = append(out, LayerChain{
			Descriptor: d,
			ID:         id,
			Parent:     parent,
			Dir:        id,
		})
		parent = id
	}
	return out
}

// assembleTar builds the `docker load` archive.
//
// Layout (root-relative members, matching `tar -C <stage> -cf out.tar .`):
//
//	<confighex>.json
//	<layerid>/layer.tar        uncompressed layer filesystem
//	<layerid>/json             {"id","parent"}
//	manifest.json
//	repositories
func (j *job) assembleTar(manifest *registry.Manifest, configDigest string, outputPath string) (int64, error) {
	if len(manifest.Layers) == 0 {
		return 0, fmt.Errorf("清单中没有层")
	}
	if configDigest == "" {
		return 0, fmt.Errorf("清单中没有配置信息")
	}

	stage := j.stageDir
	if err := os.RemoveAll(stage); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return 0, err
	}

	// 1. The image config blob, named by its digest hex — this is the
	// "Config" file manifest.json points at.
	configName := strings.TrimPrefix(configDigest, "sha256:") + ".json"
	configBytes, err := os.ReadFile(j.blobPath(configDigest))
	if err != nil {
		return 0, fmt.Errorf("读取镜像配置失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, configName), configBytes, 0o644); err != nil {
		return 0, err
	}

	// 2. Each layer, decompressed into its own directory.
	chain := buildChain(manifest.Layers)
	for _, lc := range chain {
		dir := filepath.Join(stage, lc.Dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
		if err := decompressBlob(j.blobPath(lc.Descriptor.Digest), filepath.Join(dir, "layer.tar")); err != nil {
			return 0, fmt.Errorf("解压层 %s 失败: %w", lc.Descriptor.ShortName(), err)
		}
		meta := v1LayerJSON{ID: lc.ID}
		if lc.Parent != "" {
			parent := lc.Parent
			meta.Parent = &parent
		}
		raw, err := json.Marshal(&meta)
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(dir, "json"), raw, 0o644); err != nil {
			return 0, err
		}
	}

	// 3. manifest.json + repositories.
	layerPaths := make([]string, 0, len(chain))
	for _, lc := range chain {
		layerPaths = append(layerPaths, lc.Dir+"/layer.tar")
	}
	entry := saveManifestEntry{
		Config:   configName,
		RepoTags: []string{j.ref.RepoTag()},
		Layers:   layerPaths,
	}
	manifestJSON, err := json.Marshal([]saveManifestEntry{entry})
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), manifestJSON, 0o644); err != nil {
		return 0, err
	}

	// The `repositories` map is the pre-manifest fallback loader's index. Its
	// key must be the SAME repository string manifest.json's RepoTags uses, or
	// a legacy loader would tag the image differently from a modern one
	// (`library/nginx` vs `nginx`).
	reposKey := j.ref.Repository
	if registry.NeedsLibraryPrefix(j.ref.Registry) {
		reposKey = strings.TrimPrefix(reposKey, "library/")
	}
	repos := map[string]map[string]string{
		reposKey: {j.ref.TagOrDefault(): chain[len(chain)-1].ID},
	}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(stage, "repositories"), reposJSON, 0o644); err != nil {
		return 0, err
	}

	// 4. Pack it. Members are written root-relative so `docker load` (and
	// the legacy tool's output) see identical archives.
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return 0, err
	}
	size, err := writeTar(outputPath, stage)
	if err != nil {
		return 0, err
	}

	// The stage directory is pure derived data — the blobs it was built
	// from stay behind so a re-export needs no network.
	_ = os.RemoveAll(stage)
	return size, nil
}

// writeTar archives dir's contents with root-relative member names.
func writeTar(outputPath, dir string) (int64, error) {
	tmp := outputPath + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer func() {
		// If anything below failed, do not leave a half-written tar behind
		// that looks loadable.
		if f != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	tw := tar.NewWriter(f)
	// Deterministic order: config file, then each layer dir, then the two
	// index files. Sorted walking would interleave layers, which is legal
	// but harder to eyeball.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	for _, name := range names {
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil {
			return 0, err
		}
		if info.IsDir() {
			if err := addDirTree(tw, full, name); err != nil {
				return 0, err
			}
			continue
		}
		if err := addFile(tw, full, name, info); err != nil {
			return 0, err
		}
	}

	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	f = nil

	fi, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	// Publish atomically: a reader never sees a partial archive.
	if err := os.Rename(tmp, outputPath); err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func addDirTree(tw *tar.Writer, fullDir, name string) error {
	info, err := os.Stat(fullDir)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name + "/"
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := filepath.Join(fullDir, e.Name())
		ei, err := os.Stat(full)
		if err != nil {
			return err
		}
		if ei.IsDir() {
			if err := addDirTree(tw, full, name+"/"+e.Name()); err != nil {
				return err
			}
			continue
		}
		if err := addFile(tw, full, name+"/"+e.Name(), ei); err != nil {
			return err
		}
	}
	return nil
}

func addFile(tw *tar.Writer, full, name string, info os.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	hdr.ModTime = info.ModTime()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	return err
}

// decompressBlob writes src's contents to dst, gunzipping when src is
// gzip-compressed. Registries normally serve gzipped layers, but a
// manifest may reference an uncompressed one; `docker load` requires
// layer.tar to be a plain tar, so both cases end up uncompressed.
func decompressBlob(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	head := make([]byte, 2)
	n, err := io.ReadFull(in, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	var reader io.Reader = in
	if n == 2 && bytes.Equal(head, gzipMagic) {
		gz, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	if _, err := io.Copy(out, reader); err != nil {
		return err
	}
	return out.Sync()
}

// TarName is the artifact file name:
// "<repository with / replaced>_<tag>_<arch>.tar", sanitised so it is valid
// on Windows and safe to join with a directory.
func TarName(ref registry.Ref, platform registry.Platform) string {
	repo := ref.Repository
	if registry.NeedsLibraryPrefix(ref.Registry) {
		repo = strings.TrimPrefix(repo, "library/")
	}
	parts := []string{sanitizeFilePart(repo), sanitizeFilePart(ref.TagOrDefault())}
	if arch := sanitizeFilePart(platform.Architecture); arch != "" {
		if v := sanitizeFilePart(platform.Variant); v != "" {
			arch += "_" + v
		}
		parts = append(parts, arch)
	}
	return strings.Join(parts, "_") + ".tar"
}

// DefaultOutputDir is "<base>/<repo>_<tag>_<arch>", the legacy layout
// (each image gets its own directory next to its progress file's siblings).
func DefaultOutputDir(base string, ref registry.Ref, platform registry.Platform) string {
	name := strings.TrimSuffix(TarName(ref, platform), ".tar")
	if strings.TrimSpace(base) == "" {
		base = "."
	}
	return filepath.Join(base, name)
}

// sanitizeFilePart removes path separators and Windows-hostile characters
// so a reference can never escape its output directory. This is the guard
// the legacy GUI's delete path lacked.
func sanitizeFilePart(s string) string {
	if s == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", "\x00", "",
	)
	out := replacer.Replace(s)
	out = strings.Trim(out, " .")
	if out == "." || out == ".." {
		return "_"
	}
	return out
}
