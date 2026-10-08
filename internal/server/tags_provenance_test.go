package server

import (
	"net/http"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// TestInspectTagsComeFromTheInspectedRegistry pins the fix for a provenance bug
// where the tag dropdown could describe a different image than the download.
//
// The handler used to ask a SEARCHER for tags first. A searcher is frequently a
// different registry's API — Docker Hub's, while the pull goes to ghcr.io or
// quay.io — so a same-looking `owner/repo` name returned another publisher's
// tags, and the user could pick a tag that does not exist where they are
// downloading from. The registry being inspected is the authority because it is
// the host the pull will use.
func TestInspectTagsComeFromTheInspectedRegistry(t *testing.T) {
	ts := newTestServer(t)

	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("owner/repo", "latest", "amd64", "f.txt", "tags-provenance")
	reg.RegisterImage(img)
	// The registry says these; a searcher would say something else entirely.
	reg.SetTags("owner/repo", []string{"v1.2.3", "v1.2.2", "latest"})

	code, env := ts.envelope(http.MethodPost, "/api/images/inspect", map[string]any{
		"image":    "owner/repo:latest",
		"registry": reg.Host(),
		"insecure": true,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/images/inspect = %d (%v)", code, env)
	}

	tags, _ := env["tags"].([]any)
	got := make([]string, 0, len(tags))
	for _, tag := range tags {
		s, _ := tag.(string)
		got = append(got, s)
	}
	if len(got) != 3 {
		t.Fatalf("tags = %v, want the registry's three", got)
	}
	// `latest` leads because the reference asked for it (and it exists): the
	// picker's default must be a tag the user can actually pull.
	if got[0] != "latest" {
		t.Errorf("tags[0] = %q, want latest (the requested tag) first", got[0])
	}
	for _, want := range []string{"v1.2.3", "v1.2.2"} {
		found := false
		for _, have := range got {
			if have == want {
				found = true
			}
		}
		if !found {
			t.Errorf("tags = %v, missing the registry's %q", got, want)
		}
	}

	// And the response must say where they came from, so the UI does not have to
	// pretend a fallback list is authoritative.
	if from, _ := env["tagsFrom"].(string); from != reg.Host() {
		t.Errorf("tagsFrom = %q, want the inspected registry %q", from, reg.Host())
	}
	if calls := reg.TagsCalls(); calls == 0 {
		t.Error("the registry's tags/list was never requested")
	}
}

// TestInspectFallsBackWhenTheRegistryCannotListTags keeps the other half honest:
// a registry with no tags/list must not turn into "this image has no tags", and
// the fallback must be labelled as such.
func TestInspectFallsBackWhenTheRegistryCannotListTags(t *testing.T) {
	ts := newTestServer(t)

	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "index.html", "no-tags-endpoint")
	reg.RegisterImage(img)
	// Deliberately do NOT call SetTags: /v2/library/nginx/tags/list 404s, which
	// is what a registry without the endpoint does.

	code, env := ts.envelope(http.MethodPost, "/api/images/inspect", map[string]any{
		"image":    "library/nginx:latest",
		"registry": reg.Host(),
		"insecure": true,
		// A source that resolves to no searcher, so the fallback does not reach
		// the network: this test is about the registry half of the rule.
		"source": "no-such-source",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/images/inspect = %d (%v)", code, env)
	}

	// tagsFrom must NOT claim the registry, because it did not answer.
	if from, _ := env["tagsFrom"].(string); from == reg.Host() {
		t.Errorf("tagsFrom = %q, but the registry has no tags/list", from)
	}
	// The arch list is the point of inspect and must survive regardless.
	platforms, _ := env["platforms"].([]any)
	if len(platforms) == 0 {
		t.Error("platforms is empty; a missing tag list must not break the arch picker")
	}
}
