package registry

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// TestParseWWWAuthenticate covers every challenge shape a registry hands
// back. The "Basic with no parameters" case is the regression the legacy
// Python parser hit: it indexed split('"')[3] unconditionally and raised
// IndexError (a crash, not an auth error) on `Basic realm="registry"` /
// bare `Basic`.
func TestParseWWWAuthenticate(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   AuthChallenge
	}{
		{
			name:   "bearer with all parameters",
			header: `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`,
			want: AuthChallenge{
				Scheme:  "bearer",
				Realm:   "https://auth.docker.io/token",
				Service: "registry.docker.io",
				Scope:   "repository:library/nginx:pull",
			},
		},
		{
			// The legacy IndexError regression: no parameters at all.
			name:   "basic with no parameters must not panic",
			header: "Basic",
			want:   AuthChallenge{Scheme: "basic"},
		},
		{
			name:   "basic with a realm",
			header: `Basic realm="registry"`,
			want:   AuthChallenge{Scheme: "basic", Realm: "registry"},
		},
		{
			name:   "basic with an unquoted realm",
			header: "Basic realm=registry.example.com",
			want:   AuthChallenge{Scheme: "basic", Realm: "registry.example.com"},
		},
		{
			name:   "empty header means no challenge",
			header: "",
			want:   AuthChallenge{Scheme: "none"},
		},
		{
			name:   "whitespace-only header means no challenge",
			header: "   \t ",
			want:   AuthChallenge{Scheme: "none"},
		},
		{
			name:   "unsupported scheme",
			header: "Negotiate",
			want:   AuthChallenge{Scheme: "unknown", Realm: "Negotiate"},
		},
		{
			name:   "unsupported scheme with parameters keeps the name",
			header: `Digest realm="x", nonce="y"`,
			want:   AuthChallenge{Scheme: "unknown", Realm: "Digest"},
		},
		{
			name:   "case-insensitive scheme",
			header: `bearer realm="https://auth.example.com/token"`,
			want:   AuthChallenge{Scheme: "bearer", Realm: "https://auth.example.com/token"},
		},
		{
			name:   "unquoted parameter values also parse",
			header: "Bearer realm=https://auth.example.com/token,service=svc,scope=repository:a/b:pull",
			want: AuthChallenge{
				Scheme:  "bearer",
				Realm:   "https://auth.example.com/token",
				Service: "svc",
				Scope:   "repository:a/b:pull",
			},
		},
		{
			name:   "realm with a query string survives",
			header: `Bearer realm="https://gitlab.example.com/jwt/auth?client_id=docker",service="container_registry"`,
			want:   AuthChallenge{Scheme: "bearer", Realm: "https://gitlab.example.com/jwt/auth?client_id=docker", Service: "container_registry"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A panic here is the regression: report it as a failure with
			// the offending header rather than crashing the test binary.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseWWWAuthenticate(%q) panicked: %v", tc.header, r)
				}
			}()
			got := ParseWWWAuthenticate(tc.header)
			if got != tc.want {
				t.Errorf("ParseWWWAuthenticate(%q)\n got %+v\nwant %+v", tc.header, got, tc.want)
			}
		})
	}
}

// TestCredentials protects the header encoding and the two display helpers:
// Redacted must never leak the password into a log or a task row.
func TestCredentials(t *testing.T) {
	t.Run("BasicHeader", func(t *testing.T) {
		tests := []struct {
			creds Credentials
			want  string
		}{
			{Credentials{Username: "user", Password: "pass"}, "Basic dXNlcjpwYXNz"},
			{Credentials{Username: "u", Password: "p"}, "Basic dTpw"},
			{Credentials{Username: "a:b", Password: "c:d"}, "Basic YTpiOmM6ZA=="},
		}
		for _, tc := range tests {
			if got := tc.creds.BasicHeader(); got != tc.want {
				t.Errorf("%+v.BasicHeader() = %q, want %q", tc.creds, got, tc.want)
			}
			raw := strings.TrimPrefix(tc.creds.BasicHeader(), "Basic ")
			dec, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				t.Fatalf("BasicHeader is not valid base64: %v", err)
			}
			if string(dec) != tc.creds.Username+":"+tc.creds.Password {
				t.Errorf("BasicHeader decodes to %q, want user:pass", dec)
			}
		}
	})

	t.Run("Empty treats a half-filled pair as unusable", func(t *testing.T) {
		tests := []struct {
			creds Credentials
			want  bool
		}{
			{Credentials{}, true},
			{Credentials{Username: "user"}, true},
			{Credentials{Password: "pass"}, true},
			{Credentials{Username: "user", Password: "pass"}, false},
		}
		for _, tc := range tests {
			if got := tc.creds.Empty(); got != tc.want {
				t.Errorf("%+v.Empty() = %v, want %v", tc.creds, got, tc.want)
			}
		}
	})

	t.Run("Redacted never contains the password", func(t *testing.T) {
		creds := Credentials{Username: "user", Password: "sup3r-s3cret"}
		got := creds.Redacted()
		if strings.Contains(got, creds.Password) {
			t.Fatalf("Redacted() = %q leaks the password", got)
		}
		if !strings.Contains(got, "user") {
			t.Errorf("Redacted() = %q, want the username to survive", got)
		}
		if got := (Credentials{}).Redacted(); got == "" || strings.Contains(got, ":") {
			t.Errorf("Redacted() for empty credentials = %q", got)
		}
	})
}

// newTestRef points a Ref at the fake registry over plain HTTP.
func newTestRef(host, repo, tag string) Ref {
	return Ref{Registry: host, Repository: repo, Tag: tag, UseHTTP: true}
}

// TestAuthHeaderAgainstFakeRegistry exercises the whole /v2/ ping dance
// against the in-process registry: anonymous, Bearer and Basic.
func TestAuthHeaderAgainstFakeRegistry(t *testing.T) {
	ctx := context.Background()

	t.Run("public registry needs no Authorization", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{})
		img := registrytest.MustBuildImage("test", "latest", "amd64", "hello.txt", "hi")
		mock.RegisterImage(img)

		client := NewClient(ClientOptions{ProxyURL: "-"})
		ref := newTestRef(mock.Host(), "test", "latest")

		hdr, err := client.AuthHeader(ctx, ref)
		if err != nil {
			t.Fatalf("AuthHeader: %v", err)
		}
		if hdr != "" {
			t.Errorf("AuthHeader = %q, want \"\" on a public registry", hdr)
		}
		if mock.AuthCalls() == 0 {
			t.Errorf("the /v2/ ping was never issued")
		}
		if mock.TokenCalls() != 0 {
			t.Errorf("a public registry must not be asked for a token (%d calls)", mock.TokenCalls())
		}

		// The cached value must be reused rather than re-pinged per call.
		before := mock.AuthCalls()
		if _, err := client.AuthHeader(ctx, ref); err != nil {
			t.Fatalf("second AuthHeader: %v", err)
		}
		if mock.AuthCalls() != before {
			t.Errorf("AuthHeader re-negotiated despite a cache hit (%d -> %d pings)", before, mock.AuthCalls())
		}

		m, err := client.FetchManifest(ctx, ref, "")
		if err != nil {
			t.Fatalf("FetchManifest on a public registry: %v", err)
		}
		if m.IsIndex {
			t.Errorf("image manifest reported as an index")
		}
	})

	t.Run("bearer challenge performs the token exchange", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{Bearer: true})
		img := registrytest.MustBuildImage("test", "latest", "amd64", "hello.txt", "hi")
		mock.RegisterImage(img)

		client := NewClient(ClientOptions{ProxyURL: "-"})
		ref := newTestRef(mock.Host(), "test", "latest")

		hdr, err := client.AuthHeader(ctx, ref)
		if err != nil {
			t.Fatalf("AuthHeader: %v", err)
		}
		if !strings.HasPrefix(hdr, "Bearer ") {
			t.Fatalf("AuthHeader = %q, want a Bearer token", hdr)
		}
		if !strings.Contains(hdr, registrytest.TestBearerToken) {
			t.Errorf("AuthHeader = %q, want the token the token endpoint issued", hdr)
		}
		if mock.TokenCalls() == 0 {
			t.Errorf("the token endpoint was never called")
		}

		m, err := client.FetchManifest(ctx, ref, "")
		if err != nil {
			t.Fatalf("FetchManifest with a Bearer token: %v", err)
		}
		if m.MediaType == "" {
			t.Errorf("manifest has no media type")
		}
	})

	// This is the observable form of the Basic-credentials regression:
	// negotiateAuth sends the credentials on the /v2/ ping, the registry
	// answers 200 (it authenticated us), and client.go:182-184 then throws
	// them away as "public", so every later request goes out anonymously
	// and 401s.
	t.Run("basic credentials are used", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{BasicUser: "user", BasicPass: "pass"})
		img := registrytest.MustBuildImage("test", "latest", "amd64", "hello.txt", "hi")
		mock.RegisterImage(img)

		client := NewClient(ClientOptions{
			Credentials: Credentials{Username: "user", Password: "pass"},
			ProxyURL:    "-",
		})
		ref := newTestRef(mock.Host(), "test", "latest")

		hdr, err := client.AuthHeader(ctx, ref)
		if err != nil {
			t.Fatalf("AuthHeader: %v", err)
		}
		if !strings.HasPrefix(hdr, "Basic ") {
			t.Errorf("AuthHeader = %q, want a Basic header (the credentials were configured; "+
				"client.go:182-184 discards them when the ping answers 200)", hdr)
		}

		if _, err := client.FetchManifest(ctx, ref, ""); err != nil {
			t.Errorf("FetchManifest with Basic credentials: %v (client.go:182-184 does not reuse the "+
				"credentials it sent on the ping)", err)
		}
	})

	t.Run("wrong basic credentials surface an error", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{BasicUser: "user", BasicPass: "pass"})
		img := registrytest.MustBuildImage("test", "latest", "amd64", "hello.txt", "hi")
		mock.RegisterImage(img)

		client := NewClient(ClientOptions{
			Credentials: Credentials{Username: "user", Password: "wrong"},
			ProxyURL:    "-",
		})
		ref := newTestRef(mock.Host(), "test", "latest")

		if _, err := client.FetchManifest(ctx, ref, ""); err == nil {
			t.Fatal("FetchManifest with wrong credentials succeeded, want an error")
		} else if !strings.Contains(err.Error(), "401") {
			t.Errorf("error = %q, want the registry's 401 status to be surfaced", err)
		}
		// The credentials never authenticated, so the registry's own
		// success counter must still be zero.
		if mock.AuthCalls() != 0 {
			t.Errorf("AuthCalls = %d, want 0 (the ping was rejected)", mock.AuthCalls())
		}
	})

	t.Run("basic challenge with no credentials explains what to do", func(t *testing.T) {
		mock := registrytest.New(t, registrytest.Options{BasicUser: "user", BasicPass: "pass"})
		client := NewClient(ClientOptions{ProxyURL: "-"})
		ref := newTestRef(mock.Host(), "test", "latest")

		_, err := client.AuthHeader(ctx, ref)
		if err == nil {
			t.Fatal("AuthHeader without credentials succeeded against a Basic registry")
		}
		if !strings.Contains(err.Error(), "Basic") {
			t.Errorf("error = %q, want it to mention Basic authentication", err)
		}
	})

}

// TestTokenScopeNamesTheRepository pins the fix for a GHCR-only failure.
//
// ghcr.io answers the plain /v2/ ping with a TEMPLATE scope:
//
//	Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:user/image:pull"
//
// Using that verbatim asked for a token to the repository "user/image", and
// GitHub answers 403 DENIED — not 401, so the failure did not even look like an
// auth problem: `ghcr.io/linuxcontainers/alpine` failed in this tool while
// `docker manifest inspect` fetched it fine, and the tag list silently fell back
// to Docker Hub's tags for the same name (42 of them, where ghcr has 38).
// Docker Hub and Quay send no scope on the ping, which is why only GHCR broke.
func TestTokenScopeNamesTheRepository(t *testing.T) {
	const placeholder = "repository:user/image:pull"

	for _, tc := range []struct {
		challenge string
		repo      string
		want      string
		why       string
	}{
		{placeholder, "linuxcontainers/alpine", "repository:linuxcontainers/alpine:pull",
			"the repository being fetched must win over the challenge's template"},
		{placeholder, "oras-project/oras", "repository:oras-project/oras:pull",
			"likewise for any other repository"},
		{"", "library/nginx", "repository:library/nginx:pull",
			"Docker Hub and Quay send no scope, so it is derived"},
		{placeholder, "  spaced/repo  ", "repository:spaced/repo:pull",
			"a padded repository is trimmed, not encoded with spaces"},
		{placeholder, "", placeholder,
			"with no repository to name (a bare probe) the challenge's scope is all we have"},
		{"", "", "",
			"nothing to ask for"},
	} {
		if got := tokenScope(tc.challenge, tc.repo); got != tc.want {
			t.Errorf("tokenScope(%q, %q) = %q, want %q — %s", tc.challenge, tc.repo, got, tc.want, tc.why)
		}
	}
}
