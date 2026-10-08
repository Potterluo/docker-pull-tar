package puller

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// TestPullSingleArchProducesLoadableTar is the end-to-end happy path: pull
// one amd64 image out of a real HTTP registry and assert the produced
// archive is a `docker load`-compatible v1 image — the right members, the
// right indices, and a layer.tar that really holds the *uncompressed*
// filesystem the registry shipped as a gzip blob.
func TestPullSingleArchProducesLoadableTar(t *testing.T) {
	const (
		repo    = "library/nginx"
		tag     = "latest"
		content = "<h1>hello from the layer</h1>"
	)
	mock := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage(repo, tag, "amd64", "index.html", content)
	mock.RegisterImage(img)

	workDir := t.TempDir()
	ref := mockRef(mock.Host(), repo, tag)
	res, err := Pull(context.Background(), Options{
		Ref:        ref,
		Platform:   testPlatform("amd64"),
		WorkDir:    workDir,
		MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// --- the artifact itself
	fi, err := os.Stat(res.TarPath)
	if err != nil {
		t.Fatalf("the reported TarPath does not exist: %v", err)
	}
	if res.Size != fi.Size() {
		t.Errorf("Result.Size = %d, want the file's %d bytes", res.Size, fi.Size())
	}
	if res.Size == 0 {
		t.Error("Result.Size = 0")
	}
	if res.Layers != 1 {
		t.Errorf("Result.Layers = %d, want 1", res.Layers)
	}
	if res.Platform != "linux/amd64" {
		t.Errorf("Result.Platform = %q, want linux/amd64", res.Platform)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Result.Warnings = %v, want none", res.Warnings)
	}
	if got, want := res.Bytes, int64(len(img.ConfigBytes)+len(img.LayerGz)); got != want {
		t.Errorf("Result.Bytes = %d, want %d (config + layer)", got, want)
	}

	members := registrytest.ReadTarMembers(t, res.TarPath)

	configMember := strings.TrimPrefix(img.ConfigDigest, "sha256:") + ".json"
	layerID := layerIDFor("", img.LayerDigest)

	wantMembers := []string{
		configMember,
		"manifest.json",
		"repositories",
		layerID + "/",
		layerID + "/json",
		layerID + "/layer.tar",
	}
	if len(members) != len(wantMembers) {
		t.Fatalf("archive members = %v, want exactly %v", memberNames(members), wantMembers)
	}
	for _, name := range wantMembers {
		if _, ok := members[name]; !ok {
			t.Errorf("archive is missing member %q (has %v)", name, memberNames(members))
		}
	}
	if len(layerID) != 64 {
		t.Errorf("layer id %q is %d chars, want a 64-hex sha256", layerID, len(layerID))
	}
	if len(strings.TrimSuffix(configMember, ".json")) != 64 {
		t.Errorf("config member %q is not a 64-hex digest name", configMember)
	}

	// --- manifest.json
	entries := readManifestMembers(t, members)
	if len(entries) != 1 {
		t.Fatalf("manifest.json has %d entries, want exactly 1", len(entries))
	}
	entry := entries[0]
	if entry.Config != configMember {
		t.Errorf("manifest Config = %q, want %q", entry.Config, configMember)
	}
	// The fake registry's host is not a Docker Hub host, so the implicit
	// library/ namespace is *kept* — exactly what ref.RepoTag() says.
	if len(entry.RepoTags) != 1 || entry.RepoTags[0] != ref.RepoTag() {
		t.Errorf("RepoTags = %v, want the reference's own RepoTag %q", entry.RepoTags, ref.RepoTag())
	}
	if len(entry.RepoTags) != 1 || entry.RepoTags[0] != "library/nginx:latest" {
		t.Errorf("RepoTags = %v, want [library/nginx:latest]", entry.RepoTags)
	}
	if len(entry.Layers) != 1 || entry.Layers[0] != layerID+"/layer.tar" {
		t.Errorf("manifest Layers = %v, want [%s/layer.tar]", entry.Layers, layerID)
	}

	// --- repositories
	repos := readRepositories(t, members)
	if len(repos) != 1 {
		t.Fatalf("repositories = %v, want one repository", repos)
	}
	tags, ok := repos[repo]
	if !ok {
		t.Fatalf("repositories = %v, want the key %q", repos, repo)
	}
	if tags[tag] != layerID {
		t.Errorf("repositories[%s][%s] = %q, want the manifest's layer id %q", repo, tag, tags[tag], layerID)
	}

	// --- the layer really was gunzipped and repacked
	raw, ok := members[layerID+"/layer.tar"]
	if !ok {
		t.Fatalf("layer.tar is missing")
	}
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		t.Fatal("layer.tar is still gzip-compressed; `docker load` needs a plain tar")
	}
	files := untarBytes(t, raw)
	if len(files) != 1 {
		t.Fatalf("layer.tar holds %d members, want 1: %v", len(files), files)
	}
	if got := string(files["index.html"]); got != content {
		t.Fatalf("layer file content = %q, want the original %q", got, content)
	}

	// --- v1 layer metadata
	var meta v1LayerJSON
	if err := json.Unmarshal(members[layerID+"/json"], &meta); err != nil {
		t.Fatalf("layer json: %v", err)
	}
	if meta.ID != layerID {
		t.Errorf("layer json id = %q, want %q", meta.ID, layerID)
	}
	if meta.Parent != nil {
		t.Errorf("layer json parent = %q, want null for a single-layer image", *meta.Parent)
	}

	// --- the config member is the config blob, byte for byte
	if !bytes.Equal(members[configMember], img.ConfigBytes) {
		t.Error("the config member differs from the config blob the registry served")
	}
}

// TestPullMultiArchSelectsPlatform protects the regression the legacy e2e
// test covered: pulling arm64 out of a multi-arch tag must export the arm64
// filesystem, not the amd64 one that happens to be listed first.
func TestPullMultiArchSelectsPlatform(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	amd64 := registrytest.MustBuildImage("library/app", "latest", "amd64", "arch.txt", "this is the amd64 layer")
	arm64 := registrytest.MustBuildImage("library/app", "latest", "arm64", "arch.txt", "this is the arm64 layer")
	if _, err := mock.RegisterMultiArch("library/app", "latest", amd64, arm64); err != nil {
		t.Fatalf("RegisterMultiArch: %v", err)
	}

	ref := mockRef(mock.Host(), "library/app", "latest")
	res, err := Pull(context.Background(), Options{
		Ref: ref, Platform: testPlatform("arm64"), WorkDir: t.TempDir(), MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Pull(arm64): %v", err)
	}
	if res.Platform != "linux/arm64" {
		t.Errorf("Platform = %q, want linux/arm64", res.Platform)
	}

	members := registrytest.ReadTarMembers(t, res.TarPath)
	files := layerFileContents(t, members)
	if len(files) != 1 {
		t.Fatalf("archive has %d layers, want 1", len(files))
	}
	if _, body := onlyFile(t, files[0]); string(body) != arm64.FileContent {
		t.Fatalf("layer content = %q, want the arm64 content %q (the amd64 layer was exported instead)",
			body, arm64.FileContent)
	}

	// The amd64 layer must not be in the archive at all.
	if _, ok := members[layerIDFor("", amd64.LayerDigest)+"/layer.tar"]; ok {
		t.Error("the amd64 layer ended up in an arm64 export")
	}
}

// TestPullUnknownArchFails protects the failure mode: asking for an
// architecture the image does not publish must fail loudly and say which
// ones exist, never fall back to another architecture.
func TestPullUnknownArchFails(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	amd64 := registrytest.MustBuildImage("library/app", "latest", "amd64", "f", "amd64")
	arm64 := registrytest.MustBuildImage("library/app", "latest", "arm64", "f", "arm64")
	if _, err := mock.RegisterMultiArch("library/app", "latest", amd64, arm64); err != nil {
		t.Fatalf("RegisterMultiArch: %v", err)
	}

	workDir := t.TempDir()
	res, err := Pull(context.Background(), Options{
		Ref: mockRef(mock.Host(), "library/app", "latest"), Platform: testPlatform("ppc64le"), WorkDir: workDir, MaxRetries: 1,
	})
	if err == nil {
		t.Fatalf("Pull(ppc64le) succeeded: %+v", res)
	}
	for _, want := range []string{"ppc64le", "linux/amd64", "linux/arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	// Nothing may have been written for a job that never started.
	if entries, rerr := os.ReadDir(workDir); rerr == nil && len(entries) > 0 {
		t.Errorf("the failed pull left %d entries in the work directory", len(entries))
	}
}

// TestPullRecordsProgressStates protects the progress stream the UI renders:
// the job reaches running then completed, each layer passes through
// downloading before completed, and the final byte counters add up.
func TestPullRecordsProgressStates(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/app", "latest", "amd64", "f", "payload")
	mock.RegisterImage(img)

	sink := &recordingSink{}
	res, err := Pull(context.Background(), Options{
		Ref: mockRef(mock.Host(), "library/app", "latest"), Platform: testPlatform("amd64"),
		WorkDir: t.TempDir(), Sink: sink, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.TarPath == "" {
		t.Fatal("Result.TarPath is empty")
	}

	states := sink.States()
	if len(states) < 2 {
		t.Fatalf("the sink saw %d states, want at least the running and completed ones", len(states))
	}
	if states[0].Status != StatusRunning {
		t.Errorf("first state status = %q, want %q", states[0].Status, StatusRunning)
	}
	last := states[len(states)-1]
	if last.Status != StatusCompleted {
		t.Errorf("last state status = %q, want %q", last.Status, StatusCompleted)
	}
	if last.Error != "" {
		t.Errorf("completed state carries error %q", last.Error)
	}
	if last.TotalBytes <= 0 {
		t.Errorf("TotalBytes = %d, want a positive total", last.TotalBytes)
	}
	if last.DownloadedBytes != last.TotalBytes {
		t.Errorf("DownloadedBytes = %d, want it to equal TotalBytes %d", last.DownloadedBytes, last.TotalBytes)
	}

	// Every blob must have been observed as downloading and then completed.
	seen := map[string][]string{}
	for _, st := range states {
		for _, l := range st.Layers {
			seen[l.Digest] = append(seen[l.Digest], l.Status)
		}
	}
	for _, digest := range []string{img.ConfigDigest, img.LayerDigest} {
		statuses := seen[digest]
		if !containsString(statuses, LayerDownloading) {
			t.Errorf("blob %s never reported %q: %v", digest[:19], LayerDownloading, statuses)
		}
		if !containsString(statuses, LayerCompleted) {
			t.Errorf("blob %s never reported %q: %v", digest[:19], LayerCompleted, statuses)
		}
	}
	if len(sink.Progress()) == 0 {
		t.Error("the sink never received an OnProgress callback")
	}
	for _, p := range sink.Progress() {
		if p.downloaded < 0 {
			t.Errorf("negative progress reported for %s: %d", p.digest, p.downloaded)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TestPullConfigBlobIsStoredAsConfigMember protects the Config member of
// manifest.json: the registry's config blob must be written verbatim under
// its own digest hex, because `docker load` re-reads it as the image config.
func TestPullConfigBlobIsStoredAsConfigMember(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/app", "1.0", "arm64", "f", "payload")
	mock.RegisterImage(img)

	res, err := Pull(context.Background(), Options{
		Ref: mockRef(mock.Host(), "library/app", "1.0"), Platform: testPlatform("arm64"),
		WorkDir: t.TempDir(), MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	members := registrytest.ReadTarMembers(t, res.TarPath)
	configMember := strings.TrimPrefix(img.ConfigDigest, "sha256:") + ".json"
	body, ok := members[configMember]
	if !ok {
		t.Fatalf("archive has no %s member: %v", configMember, memberNames(members))
	}
	if !bytes.Equal(body, img.ConfigBytes) {
		t.Fatalf("config member = %d bytes, want the %d-byte config blob verbatim", len(body), len(img.ConfigBytes))
	}
	if entries := readManifestMembers(t, members); entries[0].Config != configMember {
		t.Errorf("manifest Config = %q, want %q", entries[0].Config, configMember)
	}

	// The config blob must be the file the archive's own config member was
	// built from: it still parses as an image config for the right platform.
	cfg, err := registry.ParseImageConfig(body)
	if err != nil {
		t.Fatalf("the config member is not parseable: %v", err)
	}
	if cfg.Architecture != "arm64" || cfg.OS != "linux" {
		t.Errorf("config member describes %+v, want linux/arm64", cfg.Platform())
	}
}

// TestPullValidation protects the argument checks: an incomplete job must be
// rejected before any network traffic.
func TestPullValidation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "no reference",
			opts: Options{WorkDir: "x"},
			want: "镜像信息不完整",
		},
		{
			name: "no work dir",
			opts: Options{Ref: registry.Ref{Registry: "h", Repository: "r", Tag: "t"}},
			want: "WorkDir",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Pull(context.Background(), tc.opts)
			if err == nil {
				t.Fatalf("Pull = %+v, want an error", res)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	t.Run("missing tag", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		_, err := Pull(context.Background(), Options{
			Ref: mockRef(mock.Host(), "library/app", "nope"), Platform: testPlatform("amd64"),
			WorkDir: t.TempDir(), MaxRetries: 1,
		})
		if err == nil {
			t.Fatal("Pull succeeded for an unknown tag")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("error = %q, want the registry status surfaced", err)
		}
	})
}
