package registry

import (
	"strings"
	"testing"
)

// testDigest is a syntactically valid sha256 digest used by the tables.
const (
	testDigestHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testDigest    = "sha256:" + testDigestHex
)

// TestParse pins the reference-parsing contract (CONTRACT.md §2): the
// Docker Hub "library/" rule, the registry detection heuristic, digest
// pinning, and — the part the legacy tool got wrong — that an explicit host
// typed by the user always beats the -r / mirror override.
func TestParse(t *testing.T) {
	tests := []struct {
		name           string
		in             string
		customRegistry string
		want           Ref
		wantErr        bool
	}{
		{
			name: "bare name becomes library/nginx on Docker Hub",
			in:   "nginx",
			want: Ref{Registry: "registry-1.docker.io", Repository: "library/nginx", Tag: "latest"},
		},
		{
			name: "bare name with tag",
			in:   "nginx:1.26",
			want: Ref{Registry: "registry-1.docker.io", Repository: "library/nginx", Tag: "1.26"},
		},
		{
			name: "explicit library/ prefix is not doubled",
			in:   "library/nginx",
			want: Ref{Registry: "registry-1.docker.io", Repository: "library/nginx", Tag: "latest"},
		},
		{
			name: "user/app keeps its namespace (no library/ injection)",
			in:   "user/app:1.0",
			want: Ref{Registry: "registry-1.docker.io", Repository: "user/app", Tag: "1.0"},
		},
		{
			name: "host with dot and nested path",
			in:   "harbor.abc.com/a/b/nginx:1.26",
			want: Ref{Registry: "harbor.abc.com", Repository: "a/b/nginx", Tag: "1.26"},
		},
		{
			name: "host with port",
			in:   "harbor.abc.com:5000/a/b/nginx",
			want: Ref{Registry: "harbor.abc.com:5000", Repository: "a/b/nginx", Tag: "latest"},
		},
		{
			name: "localhost is a registry",
			in:   "localhost/nginx",
			want: Ref{Registry: "localhost", Repository: "nginx", Tag: "latest"},
		},
		{
			name: "localhost with port is a registry",
			in:   "localhost:5000/repo:1.0",
			want: Ref{Registry: "localhost:5000", Repository: "repo", Tag: "1.0"},
		},
		{
			name: "ghcr.io",
			in:   "ghcr.io/owner/repo:1.0",
			want: Ref{Registry: "ghcr.io", Repository: "owner/repo", Tag: "1.0"},
		},
		{
			name: "mcr.microsoft.com",
			in:   "mcr.microsoft.com/dotnet/aspnet:9.0",
			want: Ref{Registry: "mcr.microsoft.com", Repository: "dotnet/aspnet", Tag: "9.0"},
		},
		{
			name: "digest only",
			in:   "nginx@" + testDigest,
			want: Ref{Registry: "registry-1.docker.io", Repository: "library/nginx", Tag: "latest", Digest: testDigest},
		},
		{
			name: "tag and digest together",
			in:   "harbor.abc.com/a/b/nginx:1.26@" + testDigest,
			want: Ref{Registry: "harbor.abc.com", Repository: "a/b/nginx", Tag: "1.26", Digest: testDigest},
		},
		{
			name:           "custom registry applies to a bare name",
			in:             "nginx",
			customRegistry: "docker.1ms.run",
			want:           Ref{Registry: "docker.1ms.run", Repository: "library/nginx", Tag: "latest"},
		},
		{
			// Regression: the legacy tool let -r override a host the user
			// had typed out. An explicit host always wins.
			name:           "explicit host beats the custom registry",
			in:             "harbor.abc.com/x/y",
			customRegistry: "docker.1ms.run",
			want:           Ref{Registry: "harbor.abc.com", Repository: "x/y", Tag: "latest"},
		},
		{
			name:           "explicit host beats -r for ghcr too",
			in:             "ghcr.io/owner/repo:2",
			customRegistry: "docker.nju.edu.cn",
			want:           Ref{Registry: "ghcr.io", Repository: "owner/repo", Tag: "2"},
		},
		{
			name:           "Docker Hub mirror gets the library prefix",
			in:             "nginx",
			customRegistry: "registry-1.docker.io",
			want:           Ref{Registry: "registry-1.docker.io", Repository: "library/nginx", Tag: "latest"},
		},
		{
			name:           "nju mirror gets the library prefix",
			in:             "nginx",
			customRegistry: "docker.nju.edu.cn",
			want:           Ref{Registry: "docker.nju.edu.cn", Repository: "library/nginx", Tag: "latest"},
		},
		{
			// A private registry is not a Docker Hub mirror: a single
			// segment is the whole repository name there.
			name:           "non-Hub custom registry gets no library prefix",
			in:             "nginx",
			customRegistry: "my.internal.registry",
			want:           Ref{Registry: "my.internal.registry", Repository: "nginx", Tag: "latest"},
		},
		{
			name:           "trailing slash on the custom registry is trimmed",
			in:             "nginx",
			customRegistry: "docker.1ms.run/",
			want:           Ref{Registry: "docker.1ms.run", Repository: "library/nginx", Tag: "latest"},
		},
		{
			name:    "empty input",
			in:      "",
			wantErr: true,
		},
		{
			name:    "whitespace input",
			in:      "   ",
			wantErr: true,
		},
		{
			name:    "empty digest",
			in:      "nginx@",
			wantErr: true,
		},
		{
			name:    "digest without an algorithm separator",
			in:      "nginx@sha256",
			wantErr: true,
		},
		{
			name:    "host with an empty repository",
			in:      "harbor.abc.com/",
			wantErr: true,
		},
		{
			name:    "tag with an inner space is rejected",
			in:      "nginx:1 26",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in, tc.customRegistry)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q, %q) = %+v, want an error", tc.in, tc.customRegistry, got)
				}
				if !got.IsZero() {
					t.Errorf("Parse(%q, %q) returned a non-zero Ref %+v alongside the error", tc.in, tc.customRegistry, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q, %q) = %v, want no error", tc.in, tc.customRegistry, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q, %q)\n got %+v\nwant %+v", tc.in, tc.customRegistry, got, tc.want)
			}
		})
	}
}

// TestRefAccessors protects the derived forms used by the URL builder and by
// the tar's RepoTags/manifest members.
func TestRefAccessors(t *testing.T) {
	t.Run("digest reference prefers the digest", func(t *testing.T) {
		ref, err := Parse("nginx@"+testDigest, "")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if ref.Reference() != testDigest {
			t.Errorf("Reference() = %q, want the digest %q", ref.Reference(), testDigest)
		}
		if ref.TagOrDefault() != "latest" {
			t.Errorf("TagOrDefault() = %q, want latest", ref.TagOrDefault())
		}
		if ref.RepoTag() != "nginx:latest" {
			t.Errorf("RepoTag() = %q, want nginx:latest", ref.RepoTag())
		}
		if ref.Host() != "registry-1.docker.io" {
			t.Errorf("Host() = %q", ref.Host())
		}
	})

	t.Run("tag wins when a digest is absent", func(t *testing.T) {
		ref, err := Parse("harbor.abc.com/a/b/nginx:1.26@"+testDigest, "")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if ref.Tag != "1.26" || ref.Digest != testDigest {
			t.Fatalf("Parse kept %+v, want both tag and digest", ref)
		}
		if ref.Reference() != testDigest {
			t.Errorf("Reference() = %q, want the digest to win", ref.Reference())
		}
		if ref.String() != "harbor.abc.com/a/b/nginx@"+testDigest {
			t.Errorf("String() = %q, want the digest form", ref.String())
		}
	})

	t.Run("RepoTag strips library only for Docker Hub and its mirrors", func(t *testing.T) {
		tests := []struct {
			in   string
			want string
		}{
			{"nginx", "nginx:latest"},
			{"nginx:1.26", "nginx:1.26"},
			{"library/nginx:1.26", "nginx:1.26"},
			{"user/app:1.0", "user/app:1.0"},
			{"my.registry/team/app:1.0", "team/app:1.0"},
			{"ghcr.io/library/app:1.0", "app:1.0"},
			{"harbor.abc.com/a/b/nginx:1.26", "a/b/nginx:1.26"},
		}
		for _, tc := range tests {
			ref, err := Parse(tc.in, "")
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			// ghcr.io is not a Docker Hub host, so its library/ survives;
			// that case is asserted below instead of in this table.
			if tc.in == "ghcr.io/library/app:1.0" {
				if got := ref.RepoTag(); got != "library/app:1.0" {
					t.Errorf("RepoTag(%q) = %q, want library/app:1.0", tc.in, got)
				}
				continue
			}
			if got := ref.RepoTag(); got != tc.want {
				t.Errorf("RepoTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("scheme and base URL", func(t *testing.T) {
		https, err := Parse("nginx", "")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if https.Scheme() != "https" {
			t.Errorf("Scheme() = %q, want https", https.Scheme())
		}
		if https.BaseURL() != "https://registry-1.docker.io/v2" {
			t.Errorf("BaseURL() = %q", https.BaseURL())
		}
		if https.String() != "registry-1.docker.io/library/nginx:latest" {
			t.Errorf("String() = %q", https.String())
		}

		plain, err := Parse("localhost:5000/nginx", "")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plain.UseHTTP = true
		if plain.Scheme() != "http" {
			t.Errorf("Scheme() = %q, want http", plain.Scheme())
		}
		if plain.BaseURL() != "http://localhost:5000/v2" {
			t.Errorf("BaseURL() = %q", plain.BaseURL())
		}
	})

	t.Run("WithRegistry keeps repository and tag", func(t *testing.T) {
		ref, err := Parse("nginx:1.26", "")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		moved := ref.WithRegistry("docker.1ms.run", false)
		if moved.Registry != "docker.1ms.run" || moved.Repository != "library/nginx" || moved.Tag != "1.26" {
			t.Errorf("WithRegistry = %+v", moved)
		}
		if ref.Registry != "registry-1.docker.io" {
			t.Errorf("WithRegistry mutated the receiver: %+v", ref)
		}
	})
}

// TestIsDockerHub covers every spelling of Docker Hub the tool accepts and a
// sampling of hosts that must not be treated as Hub.
func TestIsDockerHub(t *testing.T) {
	hub := []string{"docker.io", "index.docker.io", "registry-1.docker.io", "registry.docker.io", "hub.docker.com", "DOCKER.IO"}
	for _, h := range hub {
		if !IsDockerHub(h) {
			t.Errorf("IsDockerHub(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"ghcr.io", "mcr.microsoft.com", "docker.1ms.run", "127.0.0.1:5000", ""} {
		if IsDockerHub(h) {
			t.Errorf("IsDockerHub(%q) = true, want false", h)
		}
	}
}

// TestNeedsLibraryPrefix is the rule behind parse-time "library/" injection
// and RepoTag stripping: Hub and its pull-through mirrors need it, a private
// registry does not.
func TestNeedsLibraryPrefix(t *testing.T) {
	for _, h := range []string{"registry-1.docker.io", "docker.1ms.run", "docker.nju.edu.cn", "docker.xuanyuan.me", "docker.m.daocloud.io"} {
		if !NeedsLibraryPrefix(h) {
			t.Errorf("NeedsLibraryPrefix(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"my.internal.registry", "ghcr.io", "localhost:5000", "k8s.m.daocloud.io"} {
		if NeedsLibraryPrefix(h) {
			t.Errorf("NeedsLibraryPrefix(%q) = true, want false", h)
		}
	}
}

// TestMirrors protects the shipped mirror list the store seeds and the
// default that an unqualified reference resolves to.
func TestMirrors(t *testing.T) {
	def := DefaultMirror()
	if def.Host != "registry-1.docker.io" {
		t.Errorf("DefaultMirror().Host = %q, want registry-1.docker.io", def.Host)
	}
	if !def.IsDefault || def.ID != "dockerhub" {
		t.Errorf("DefaultMirror() = %+v", def)
	}

	tests := []struct {
		id       string
		wantHost string
		wantOK   bool
	}{
		{"dockerhub", "registry-1.docker.io", true},
		{"1ms", "docker.1ms.run", true},
		{"nju", "docker.nju.edu.cn", true},
		{"nope", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		m, ok := MirrorByID(tc.id)
		if ok != tc.wantOK {
			t.Fatalf("MirrorByID(%q) ok = %v, want %v", tc.id, ok, tc.wantOK)
		}
		if ok && m.Host != tc.wantHost {
			t.Errorf("MirrorByID(%q).Host = %q, want %q", tc.id, m.Host, tc.wantHost)
		}
	}

	byHost, ok := MirrorByHost("docker.nju.edu.cn")
	if !ok || byHost.ID != "nju" {
		t.Errorf("MirrorByHost(docker.nju.edu.cn) = %+v, %v", byHost, ok)
	}
	// Host lookup is case-insensitive: users paste whatever they copied.
	if m, ok := MirrorByHost("DOCKER.1MS.RUN"); !ok || m.ID != "1ms" {
		t.Errorf("MirrorByHost(DOCKER.1MS.RUN) = %+v, %v", m, ok)
	}
	if _, ok := MirrorByHost("not.a.mirror"); ok {
		t.Errorf("MirrorByHost(not.a.mirror) reported a hit")
	}

	for _, m := range BuiltinMirrors {
		if m.ID == "" || m.Host == "" || m.Name == "" {
			t.Errorf("incomplete builtin mirror %+v", m)
		}
		if strings.ContainsAny(m.Host, " /") {
			t.Errorf("builtin mirror host %q must be a bare host[:port]", m.Host)
		}
	}
	if len(BuiltinMirrors) == 0 {
		t.Fatal("BuiltinMirrors is empty: the store would seed nothing")
	}
}
