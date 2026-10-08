package puller

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// TestChainID pins the legacy v1 layer id to the documented formula,
// sha256hex(parentID + "\n" + blobDigest + "\n"), against hand-computed
// vectors. `docker load` rebuilds the layer graph from these, so an extra or
// missing newline silently breaks the imported image.
func TestChainID(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		digest string
		want   string
	}{
		{
			name:   "first layer",
			parent: "",
			digest: "sha256:abc",
			want:   "01ebe1de0c4b1f0805f5cc87b446290ebdf98474ff1fb87fb218f29de4fc9a5f",
		},
		{
			name:   "child layer",
			parent: "parent",
			digest: "sha256:abc",
			want:   "9e80f2b30b91a2641d3e1723d92d96ca4db0c4971af3ceac5b991bcf5cbb1a40",
		},
		{
			name:   "long parent",
			parent: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			digest: "sha256:abc",
			want:   "32df47e5673f6bb45d33358a75d577b6dbc94bceee559f033ebc6f5c82e73ed3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chainID(tc.parent, tc.digest)
			if got != tc.want {
				t.Fatalf("chainID(%q, %q) = %q, want %q", tc.parent, tc.digest, got, tc.want)
			}
			// The formula implemented independently of the production code.
			if want := layerIDFor(tc.parent, tc.digest); got != want {
				t.Fatalf("chainID = %q, independent computation = %q", got, want)
			}
			if len(got) != 64 || strings.HasPrefix(got, "sha256:") {
				t.Errorf("chainID = %q, want bare 64-char hex (it is used as a path segment)", got)
			}
		})
	}

	// The id must depend on the parent as well as the blob.
	if chainID("", "sha256:abc") == chainID("other", "sha256:abc") {
		t.Error("chainID ignored the parent id")
	}
}

// TestTarNameSanitises protects the artifact file name: the reference and
// the platform are both user-controlled strings that end up in a path.
func TestTarNameSanitises(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		platform registry.Platform
		want     string
	}{
		{
			name:     "Docker Hub official image",
			image:    "nginx",
			platform: registry.Platform{OS: "linux", Architecture: "amd64"},
			want:     "nginx_latest_amd64.tar",
		},
		{
			name:     "tag is kept",
			image:    "nginx:1.26",
			platform: registry.Platform{OS: "linux", Architecture: "amd64"},
			want:     "nginx_1.26_amd64.tar",
		},
		{
			name:     "variant joins the architecture",
			image:    "nginx:1.26",
			platform: registry.Platform{OS: "linux", Architecture: "arm", Variant: "v7"},
			want:     "nginx_1.26_arm_v7.tar",
		},
		{
			name:     "repository separators become underscores",
			image:    "user/app:1.0",
			platform: registry.Platform{OS: "linux", Architecture: "amd64"},
			want:     "user_app_1.0_amd64.tar",
		},
		{
			name:     "host is not part of the name",
			image:    "harbor.abc.com/a/b/nginx:1.26",
			platform: registry.Platform{OS: "linux", Architecture: "amd64"},
			want:     "a_b_nginx_1.26_amd64.tar",
		},
		{
			name:     "digest-pinned reference uses the tag placeholder",
			image:    "nginx@sha256:" + strings.Repeat("a", 64),
			platform: registry.Platform{OS: "linux", Architecture: "amd64"},
			want:     "nginx_latest_amd64.tar",
		},
		{
			name:     "no architecture",
			image:    "nginx:latest",
			platform: registry.Platform{},
			want:     "nginx_latest.tar",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := registry.Parse(tc.image, "")
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.image, err)
			}
			got := TarName(ref, tc.platform)
			if got != tc.want {
				t.Errorf("TarName(%q, %+v) = %q, want %q", tc.image, tc.platform, got, tc.want)
			}
			if strings.ContainsAny(got, `/\:`) {
				t.Errorf("TarName = %q contains a path separator or drive colon", got)
			}
		})
	}

	t.Run("DefaultOutputDir", func(t *testing.T) {
		ref, err := registry.Parse("nginx:1.26", "")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		platform := registry.Platform{OS: "linux", Architecture: "amd64"}
		if got, want := DefaultOutputDir("base", ref, platform), filepath.Join("base", "nginx_1.26_amd64"); got != want {
			t.Errorf("DefaultOutputDir = %q, want %q", got, want)
		}
		// An empty base means the current directory, not an absolute path.
		if got, want := DefaultOutputDir("", ref, platform), filepath.Join(".", "nginx_1.26_amd64"); got != want {
			t.Errorf("DefaultOutputDir(\"\") = %q, want %q", got, want)
		}
		outDir := DefaultOutputDir("base", ref, platform)
		if want := filepath.Dir(filepath.Join(outDir, TarName(ref, platform))); outDir != want {
			t.Errorf("the artifact does not land in DefaultOutputDir: %q vs %q", outDir, want)
		}
	})
}

// TestSanitizeFilePart is the path-traversal guard the legacy GUI's delete
// path lacked: a repository, tag or platform coming from a user string must
// never be able to escape its output directory.
func TestSanitizeFilePart(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"linux/amd64", "linux_amd64"},
		{"a:b", "a_b"},
		{`..\evil`, "_evil"},
		{"../../etc/passwd", "_.._etc_passwd"},
		{`a*b?c"d<e>f|g`, "a_b_c_d_e_f_g"},
		{"\x00nul", "nul"},
		{"name.", "name"},
		{"  spaced  ", "spaced"},
		{"..", ""},
		{".", ""},
		{"", ""},
		{"nginx", "nginx"},
		{"1.26", "1.26"},
		{"CON", "CON"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := sanitizeFilePart(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeFilePart(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertContained(t, tc.in, got)
		})
	}

	// The invariant, stated directly for the hostile inputs.
	for _, in := range []string{"../", "..", "/", `\`, ":", "a/../../b", "..\\..\\..\\windows\\system32"} {
		got := sanitizeFilePart(in)
		t.Run("contained/"+in, func(t *testing.T) {
			assertContained(t, in, got)
		})
	}
}

// assertContained checks that a sanitised part cannot move a path out of its
// directory or name a device/drive.
func assertContained(t *testing.T, in, out string) {
	t.Helper()
	if strings.ContainsAny(out, `/\:`) {
		t.Errorf("sanitizeFilePart(%q) = %q still contains a separator", in, out)
	}
	if out == "." || out == ".." {
		t.Errorf("sanitizeFilePart(%q) = %q is a directory reference", in, out)
	}
	if strings.HasPrefix(out, "..") {
		t.Errorf("sanitizeFilePart(%q) = %q can still traverse upwards", in, out)
	}
	base := filepath.Join("stage", out)
	if out != "" && filepath.Dir(base) != "stage" {
		t.Errorf("sanitizeFilePart(%q) = %q escapes the output directory (%q)", in, out, base)
	}
}

// TestFormatBytes protects the human-readable sizes shown in the UI and in
// the logged layer labels (1024-based, legacy "1.0MB" style).
func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1024 * 1024, "1.0MB"},
		{2621440, "2.5MB"},
		{1024 * 1024 * 1024, "1.0GB"},
		{1024 * 1024 * 1024 * 1024, "1.0TB"},
	}
	for _, tc := range tests {
		if got := FormatBytes(tc.in); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatDuration protects the elapsed-time line ("30秒", "1分30秒",
// "1小时1分") the summary prints.
func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0秒"},
		{999 * time.Millisecond, "0秒"},
		{time.Second, "1秒"},
		{30 * time.Second, "30秒"},
		{59 * time.Second, "59秒"},
		{60 * time.Second, "1分0秒"},
		{90 * time.Second, "1分30秒"},
		{3599 * time.Second, "59分59秒"},
		{time.Hour, "1小时0分"},
		{time.Hour + time.Second, "1小时0分"},
		{3661 * time.Second, "1小时1分"},
		{2*time.Hour + 3*time.Minute, "2小时3分"},
	}
	for _, tc := range tests {
		if got := FormatDuration(tc.in); got != tc.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAssembleTarLayerChain protects the multi-layer archive: the chain ids,
// their parent links, the order manifest.json lists the layers in, and that
// repositories points at the LAST layer (the one `docker load` starts from).
func TestAssembleTarLayerChain(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	layers := []testLayer{
		{fileName: "first.txt", content: []byte("first layer")},
		{fileName: "second.txt", content: []byte("second layer")},
		{fileName: "third.txt", content: []byte("third layer")},
	}
	img := newTestImage(t, "library/app", "3.0", "amd64", layers...)
	img.register(mock)

	res, err := Pull(context.Background(), Options{
		Ref: mockRef(mock.Host(), img.repo, img.tag), Platform: testPlatform("amd64"),
		WorkDir: t.TempDir(), Workers: 3, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	members := registrytest.ReadTarMembers(t, res.TarPath)
	entry := readManifestMembers(t, members)[0]

	// The chain ids, computed independently from CONTRACT.md §3.
	wantIDs := make([]string, 0, len(layers))
	parent := ""
	for _, b := range img.layers {
		id := layerIDFor(parent, b.digest)
		wantIDs = append(wantIDs, id)
		parent = id
	}

	if len(entry.Layers) != len(layers) {
		t.Fatalf("manifest Layers = %v, want %d entries", entry.Layers, len(layers))
	}
	for i, id := range wantIDs {
		if entry.Layers[i] != id+"/layer.tar" {
			t.Errorf("manifest Layers[%d] = %q, want %q (manifest order)", i, entry.Layers[i], id+"/layer.tar")
		}

		raw, ok := members[id+"/json"]
		if !ok {
			t.Fatalf("layer %d metadata %s/json is missing; members: %v", i, id, memberNames(members))
		}
		var meta v1LayerJSON
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("layer %d json: %v", i, err)
		}
		if meta.ID != id {
			t.Errorf("layer %d json id = %q, want %q", i, meta.ID, id)
		}
		switch {
		case i == 0 && meta.Parent != nil:
			t.Errorf("first layer json parent = %q, want null", *meta.Parent)
		case i > 0 && (meta.Parent == nil || *meta.Parent != wantIDs[i-1]):
			t.Errorf("layer %d json parent = %v, want %q", i, meta.Parent, wantIDs[i-1])
		}

		body, ok := members[id+"/layer.tar"]
		if !ok {
			t.Fatalf("layer %d payload %s/layer.tar is missing", i, id)
		}
		files := untarBytes(t, body)
		name, content := onlyFile(t, files)
		if name != layers[i].fileName || !bytes.Equal(content, layers[i].content) {
			t.Errorf("layer %d content = %q (%q), want %q (%q)", i, name, content, layers[i].fileName, layers[i].content)
		}
	}

	repos := readRepositories(t, members)
	if got, want := repos[img.repo][img.tag], wantIDs[len(wantIDs)-1]; got != want {
		t.Errorf("repositories[%s][%s] = %q, want the last layer id %q", img.repo, img.tag, got, want)
	}

	// The config member is the only non-layer json in the archive.
	configMember := strings.TrimPrefix(img.configDigest, "sha256:") + ".json"
	if !bytes.Equal(members[configMember], img.configBytes) {
		t.Errorf("config member %s does not match the config blob", configMember)
	}
	if len(members) != 3*len(layers)+3 { // config, manifest.json, repositories + (dir, json, layer.tar) per layer
		t.Errorf("archive has %d members, want %d: %v", len(members), 3*len(layers)+3, memberNames(members))
	}
}
