package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// The fixture below is a trimmed copy of a REAL quay.io response, captured with
//
//	GET https://quay.io/api/v1/find/repositories?query=prometheus&page=1
//
// Shape verified before the parser was written: results[].namespace is an
// OBJECT (not a string), and the endpoint ignores `limit` (asking for 3 returns
// 10). Both facts are load-bearing, and a hand-invented fixture would have got
// them wrong.
const quaySearchFixture = `{
  "results": [
    {"kind":"repository","title":"prometheus","namespace":{"name":"prometheus"},
     "name":"prometheus","description":"The Prometheus monitoring system",
     "is_public":true,"score":42.5,"href":"/prometheus/prometheus"},
    {"kind":"repository","title":"node-exporter","namespace":{"name":"prometheus"},
     "name":"node-exporter","description":"","is_public":true,"score":20,"href":"/x"},
    {"kind":"repository","title":"bad","namespace":{"name":""},
     "name":"nonsense","is_public":true,"score":1,"href":"/y"}
  ],
  "has_additional": true,
  "page": 1,
  "page_size": 10
}`

func TestQuaySearchParsesNamespaceAndBuildsPullReferences(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quaySearchFixture))
	}))
	defer srv.Close()

	s := NewQuaySearcher(srv.Client(), srv.URL)
	page, err := s.Search(context.Background(), "prometheus", 1, 25)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/api/v1/find/repositories" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "prometheus" {
		t.Errorf("query = %q", gotQuery)
	}
	// The namespace-less result must be dropped: it cannot be turned into a
	// pull reference, so surfacing it would produce a row that 404s.
	if len(page.Results) != 2 {
		t.Fatalf("results = %d, want 2 (the namespace-less entry is unusable)", len(page.Results))
	}
	// `namespace.name` + "/" + `name` is the reference a user then pulls.
	if got, want := page.Results[0].Repository, "prometheus/prometheus"; got != want {
		t.Errorf("Repository = %q, want %q", got, want)
	}
	if got, want := page.Results[0].Image, "prometheus"; got != want {
		t.Errorf("Image = %q, want %q", got, want)
	}
	if got, want := page.Results[1].Repository, "prometheus/node-exporter"; got != want {
		t.Errorf("Repository = %q, want %q", got, want)
	}
	if page.Results[0].Source != "quay" {
		t.Errorf("Source = %q, want quay", page.Results[0].Source)
	}
	if page.Results[0].Official {
		t.Error("a non-library namespace must not be marked official")
	}
	// has_additional means there is at least another page; the UI needs a
	// non-zero total to enable "next" without us inventing an exact count.
	if page.Total <= 0 {
		t.Errorf("Total = %d, want > 0 when has_additional is true", page.Total)
	}
}

func TestQuaySearchRejectsAnEmptyKeyword(t *testing.T) {
	s := NewQuaySearcher(nil, "")
	if _, err := s.Search(context.Background(), "   ", 1, 10); err == nil {
		t.Error("Search with a blank keyword returned nil, want an error")
	}
}

const quayTagsFixture = `{
  "tags": [
    {"name":"latest","size":1234,"last_modified":"2024-05-06T07:08:09Z","is_manifest_list":true},
    {"name":"v2.53.0","size":1200,"last_modified":"2024-04-01T00:00:00Z","is_manifest_list":false},
    {"name":"","size":0}
  ],
  "page": 1,
  "has_additional": false
}`

func TestQuayTagsParsesNamesAndDates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v2/repository/prometheus/prometheus/tag/") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quayTagsFixture))
	}))
	defer srv.Close()

	tags, err := NewQuaySearcher(srv.Client(), srv.URL).Tags(context.Background(), "prometheus/prometheus")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags = %d, want 2 (an unnamed entry is dropped)", len(tags))
	}
	if tags[0].Name != "latest" || tags[0].Updated != "2024-05-06" {
		t.Errorf("first tag = %+v, want latest / 2024-05-06", tags[0])
	}
}

// TestQuayTagsRequiresANamespace pins a real difference from Docker Hub: Quay
// has no implicit "library" namespace, so a bare repository name must be
// rejected with an explanation rather than turned into a URL that 404s.
func TestQuayTagsRequiresANamespace(t *testing.T) {
	_, err := NewQuaySearcher(nil, "https://quay.io").Tags(context.Background(), "nginx")
	if err == nil {
		t.Fatal("Tags(\"nginx\") returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "命名空间") {
		t.Errorf("error %q should explain the namespace requirement", err)
	}
}

func TestQuaySearchErrorsSurfaceTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := NewQuaySearcher(srv.Client(), srv.URL).Search(context.Background(), "x", 1, 10); err == nil {
		t.Fatal("a 503 returned nil, want an error")
	}
}

// TestBuiltinRegistriesAreWellFormed guards the catalog itself: a duplicate id
// or host would make RegistryByID/RegistryByHost silently return the first
// match, and a SearchID with no implementation behind it would offer the user
// a search that always fails.
func TestBuiltinRegistriesAreWellFormed(t *testing.T) {
	built := SearchSources(nil)
	seenID := map[string]bool{}
	seenHost := map[string]bool{}

	for _, r := range BuiltinRegistries {
		if r.ID == "" || r.Name == "" || r.Host == "" {
			t.Errorf("incomplete entry: %+v", r)
		}
		if seenID[r.ID] {
			t.Errorf("duplicate registry id %q", r.ID)
		}
		seenID[r.ID] = true

		host := NormalizeAuthHost(r.Host)
		if seenHost[host] {
			t.Errorf("duplicate registry host %q (as %q)", r.Host, host)
		}
		seenHost[host] = true

		// The host must survive a parse round trip as a registry: this is what
		// makes `ghcr.io/owner/repo` pullable with no configuration.
		ref, err := Parse(r.Host+"/owner/repo", "")
		if err != nil {
			t.Errorf("Parse(%s/owner/repo): %v", r.Host, err)
			continue
		}
		if NormalizeAuthHost(ref.Registry) != host {
			t.Errorf("%s parsed to registry %q, want %q", r.Host, ref.Registry, host)
		}
		// A non-Docker-Hub registry must NOT get the "library/" prefix: on
		// quay.io "nginx" is not "library/nginx", and prefixing would turn a
		// valid single-segment namespace into a nonexistent repository.
		if !IsDockerHub(ref.Registry) && NeedsLibraryPrefix(ref.Registry) && !dockerHubProxies[strings.ToLower(ref.Registry)] {
			t.Errorf("%s is treated as a Docker Hub proxy (library/ prefix)", r.Host)
		}

		if r.SearchID != "" {
			if _, ok := built[r.SearchID]; !ok {
				t.Errorf("registry %s advertises SearchID %q but no Searcher provides it", r.ID, r.SearchID)
			}
		}
	}
}

func TestRegistryLookups(t *testing.T) {
	if r, ok := RegistryByID("ghcr"); !ok || r.Host != "ghcr.io" {
		t.Errorf("RegistryByID(ghcr) = %+v, %v", r, ok)
	}
	if _, ok := RegistryByID("nope"); ok {
		t.Error("RegistryByID accepted an unknown id")
	}
	// The Docker Hub aliases must all resolve to the same catalog entry, or a
	// reference written as `docker.io/x` would not be recognised as a
	// built-in source.
	for _, alias := range []string{"docker.io", "index.docker.io", "registry-1.docker.io", "", "https://index.docker.io/v1/"} {
		if _, ok := RegistryByHost(alias); !ok {
			t.Errorf("RegistryByHost(%q) found nothing", alias)
		}
	}
	if r, ok := RegistryByHost("quay.io"); !ok || r.ID != "quay" {
		t.Errorf("RegistryByHost(quay.io) = %+v, %v", r, ok)
	}
	if _, ok := RegistryByHost("harbor.internal:5000"); ok {
		t.Error("a private registry must not be reported as a built-in")
	}
}

func TestNormalizeAuthHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "docker.io"},
		{"   ", "docker.io"},
		{"DOCKER.IO", "docker.io"},
		{"registry-1.docker.io", "docker.io"},
		{"index.docker.io", "docker.io"},
		{"https://index.docker.io/v1/", "docker.io"},
		{"ghcr.io", "ghcr.io"},
		{"https://ghcr.io", "ghcr.io"},
		{"ghcr.io/", "ghcr.io"},
		{"ghcr.io/owner/repo", "ghcr.io"},
		{"127.0.0.1:5000", "127.0.0.1:5000"}, // a port is part of the host
		{"harbor.internal:5000/x/y", "harbor.internal:5000"},
		{"HTTPS://Quay.Io", "quay.io"},
	} {
		if got := NormalizeAuthHost(tc.in); got != tc.want {
			t.Errorf("NormalizeAuthHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCatalogJSONShapeIsStable pins the wire shape the UI consumes, so a
// rename cannot silently break /api/registries' client.
func TestCatalogJSONShapeIsStable(t *testing.T) {
	body, err := json.Marshal(BuiltinRegistries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != len(BuiltinRegistries) {
		t.Fatalf("round trip changed the count")
	}
	for _, key := range []string{"id", "name", "host"} {
		if _, ok := decoded[0][key]; !ok {
			t.Errorf("catalog entries must carry %q", key)
		}
	}
}

// TestListTagsPutsLatestFirst: tags/list has no defined order and is
// lexicographic in practice, so library/nginx reports "1" first. A picker that
// defaults to tags[0] would then offer "1" to someone who asked for nginx.
func TestListTagsPutsLatestFirst(t *testing.T) {
	mock := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "index.html", "tag-order")
	mock.RegisterImage(img)
	mock.SetTags("library/nginx", []string{"1", "1-alpine", "latest", "mainline"})

	client := NewClient(ClientOptions{})
	got, err := client.ListTags(t.Context(), Ref{
		Registry: mock.Host(), Repository: "library/nginx", Tag: "latest", UseHTTP: true,
	})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("ListTags = %v, want 4", got)
	}
	if got[0] != "latest" {
		t.Errorf("tags[0] = %q, want latest first (the picker's default)", got[0])
	}
	// The rest keep the registry's order; no invented ranking.
	if got[1] != "1" || got[2] != "1-alpine" || got[3] != "mainline" {
		t.Errorf("tags = %v, want the registry's order after latest", got)
	}

	// A page that omits latest, for a repository that really HAS it, gains it:
	// the manifest probe decides, not the page. library/nginx is exactly this
	// case — thousands of tags, so no 200-entry lexicographic window contains
	// latest.
	mock.SetTags("library/nginx", []string{"v2", "v1"})
	if got, err := client.ListTags(t.Context(), Ref{
		Registry: mock.Host(), Repository: "library/nginx", Tag: "latest", UseHTTP: true,
	}); err != nil || len(got) != 3 || got[0] != "latest" {
		t.Errorf("ListTags = %v (%v), want latest prepended (the repository has one)", got, err)
	}

	// A repository with NO latest must not have one invented for it, and the tag
	// the caller asked for goes first when it exists.
	plain := registrytest.MustBuildImage("library/plain", "v2", "amd64", "f.txt", "no-latest")
	mock.RegisterImage(plain)
	mock.SetTags("library/plain", []string{"v9", "v2"})
	got, err = client.ListTags(t.Context(), Ref{
		Registry: mock.Host(), Repository: "library/plain", Tag: "v2", UseHTTP: true,
	})
	if err != nil {
		t.Fatalf("ListTags(library/plain): %v", err)
	}
	if len(got) != 2 || got[0] != "v2" || got[1] != "v9" {
		t.Errorf("ListTags(library/plain) = %v, want [v2 v9]: the asked-for tag first, no invented latest", got)
	}

	// An unknown repository must be an ERROR, not "no tags", so the caller can
	// fall back to a searcher instead of claiming the image has no tags.
	if _, err := client.ListTags(t.Context(), Ref{
		Registry: mock.Host(), Repository: "library/absent", UseHTTP: true,
	}); err == nil {
		t.Error("ListTags of an unknown repository returned no error")
	}
}
