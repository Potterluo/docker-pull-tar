package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
)

// TestSearchSourcesAdvertiseTheirPullHost protects the fix for the bug where
// searching the 1ms accelerator downloaded from registry-1.docker.io.
//
// A search result is a repository NAME, not a location, so the client has to be
// told which host will serve it. That hint is computed server-side (the row
// stores a random "src_…" id, so only the server can map it back to a built-in).
func TestSearchSourcesAdvertiseTheirPullHost(t *testing.T) {
	ts := newTestServer(t)

	code, env := ts.envelope(http.MethodGet, "/api/sources?kind=search", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/sources?kind=search = %d", code)
	}
	rows, ok := env["sources"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("sources = %#v", env["sources"])
	}

	byName := map[string]map[string]any{}
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("row is %T", row)
		}
		name, _ := m["name"].(string)
		byName[name] = m
	}

	// 1ms must point at its own host: that is the whole bug.
	accel, present := byName["1ms 加速源"]
	if !present {
		t.Fatalf("the 1ms accelerator is not among the seeded search sources: %v", byName)
	}
	if got, _ := accel["pullHost"].(string); got != "docker.1ms.run" {
		t.Errorf("1ms pullHost = %q, want docker.1ms.run — searching 1ms would download from elsewhere", got)
	}

	// Quay serves its own namespace.
	if quay, present := byName["Quay.io (Red Hat)"]; present {
		if got, _ := quay["pullHost"].(string); got != "quay.io" {
			t.Errorf("quay pullHost = %q, want quay.io", got)
		}
	}

	// Docker Hub deliberately has NO hint, so the operator's configured default
	// mirror still wins.
	if hub, present := byName["Docker Hub 官方"]; present {
		if got, _ := hub["pullHost"].(string); got != "" {
			t.Errorf("dockerhub pullHost = %q, want empty (defer to the default mirror)", got)
		}
	}

	// And every advertised host must be usable as a registry, or the client
	// would send something the pull path rejects.
	for name, row := range byName {
		host, _ := row["pullHost"].(string)
		if host == "" {
			continue
		}
		if _, err := registry.Parse("library/nginx:latest", host); err != nil {
			t.Errorf("%s advertises pullHost %q, which is not usable as a registry: %v", name, host, err)
		}
	}
}

// TestMirrorSourcesCarryNoPullHost keeps the two kinds distinct: a MIRROR row is
// itself the download target (its `host` is the answer), so a pullHost field
// there would be a second, contradictory answer.
func TestMirrorSourcesCarryNoPullHost(t *testing.T) {
	ts := newTestServer(t)
	_, env := ts.envelope(http.MethodGet, "/api/sources?kind=mirror", nil)
	rows, _ := env["sources"].([]any)
	if len(rows) == 0 {
		t.Fatal("no mirror sources were seeded")
	}
	for _, row := range rows {
		m := row.(map[string]any)
		if host, _ := m["pullHost"].(string); host != "" {
			t.Errorf("mirror %v carries pullHost=%q; its own host field is the answer", m["name"], host)
		}
		if host, _ := m["host"].(string); host == "" {
			t.Errorf("mirror %v has no host", m["name"])
		}
	}
}

// TestCreateTaskHonoursAnExplicitRegistry proves the hint actually ROUTES the
// download. The in-process fake registry stands in for the accelerator: a pull
// that ignores `registry` would try the default host and fail to resolve.
func TestCreateTaskHonoursAnExplicitRegistry(t *testing.T) {
	ts := newTestServer(t)

	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "f.txt", "routed")
	reg.RegisterImage(img)

	// The image is named WITHOUT a host, exactly as a search result is, and the
	// registry is supplied separately — the shape the UI now sends.
	code, env := ts.envelope(http.MethodPost, "/api/tasks", map[string]any{
		"image":    "library/nginx:latest",
		"registry": reg.Host(),
		"platform": "linux/amd64",
		"insecure": true,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/tasks = %d (%v)", code, env)
	}
	task, _ := env["task"].(map[string]any)
	ref, _ := task["ref"].(string)
	if !strings.Contains(ref, reg.Host()) {
		t.Errorf("task ref = %q, want it to name the requested registry %q", ref, reg.Host())
	}
	gotRegistry, _ := task["registry"].(string)
	if gotRegistry != reg.Host() {
		t.Errorf("task registry = %q, want %q", gotRegistry, reg.Host())
	}
}

// TestCreateTaskAcceptsAMirrorID pins the other accepted spelling: the UI may
// send a mirror id ("1ms") instead of a host, and the manager resolves it.
func TestCreateTaskAcceptsAMirrorID(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodPost, "/api/tasks", map[string]any{
		"image":    "library/nginx:latest",
		"registry": "1ms",
		"platform": "linux/amd64",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/tasks with registry=1ms = %d (%v)", code, env)
	}
	task, _ := env["task"].(map[string]any)
	if got, _ := task["registry"].(string); got != "docker.1ms.run" {
		t.Errorf("task registry = %q, want docker.1ms.run (the mirror id must resolve to its host)", got)
	}
}

// TestDefaultPullHostFollowsTheSetting covers the "the initial source is not
// necessarily docker.io" requirement.
//
// The UI needs the effective default so it can preselect it, and only the
// server can resolve it: `default_mirror` may hold a built-in accelerator id
// ("1ms", "nju"), a user-added mirror's host, or nothing. Guessing client-side
// is how docker.io gets assumed for an operator who deliberately configured an
// accelerator.
func TestDefaultPullHostFollowsTheSetting(t *testing.T) {
	ts := newTestServer(t)

	readDefault := func() string {
		ts.t.Helper()
		code, env := ts.envelope(http.MethodGet, "/api/registries", nil)
		if code != http.StatusOK {
			ts.t.Fatalf("GET /api/registries = %d", code)
		}
		got, _ := env["defaultPullHost"].(string)
		return got
	}

	// The shipped default is Docker Hub.
	if got := readDefault(); got != "registry-1.docker.io" {
		t.Errorf("initial defaultPullHost = %q, want registry-1.docker.io", got)
	}

	for _, tc := range []struct {
		setting string
		want    string
		why     string
	}{
		{"1ms", "docker.1ms.run", "a built-in accelerator id must resolve to its host"},
		{"nju", "docker.nju.edu.cn", "likewise for another built-in"},
		{"docker.nju.edu.cn", "docker.nju.edu.cn", "an explicit host is used as-is"},
	} {
		if code, env := ts.envelope(http.MethodPut, "/api/settings", map[string]string{
			"default_mirror": tc.setting,
		}); code != http.StatusOK {
			t.Fatalf("PUT default_mirror=%s = %d (%v)", tc.setting, code, env)
		}
		if got := readDefault(); got != tc.want {
			t.Errorf("default_mirror=%q gave defaultPullHost %q, want %q — %s",
				tc.setting, got, tc.want, tc.why)
		}
	}
}

// TestDefaultPullHostMatchesWhatAPullActuallyUses is the honesty check: the host
// the UI preselects must be the host a bare reference really resolves to, or the
// UI would promise one thing and the pull do another.
func TestDefaultPullHostMatchesWhatAPullActuallyUses(t *testing.T) {
	ts := newTestServer(t)

	if code, env := ts.envelope(http.MethodPut, "/api/settings", map[string]string{
		"default_mirror": "1ms",
	}); code != http.StatusOK {
		t.Fatalf("PUT default_mirror = %d (%v)", code, env)
	}
	_, env := ts.envelope(http.MethodGet, "/api/registries", nil)
	advertised, _ := env["defaultPullHost"].(string)

	// A bare name with no explicit registry: where does it land?
	code, taskEnv := ts.envelope(http.MethodPost, "/api/tasks", map[string]any{
		"image":    "library/nginx:latest",
		"platform": "linux/amd64",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/tasks = %d (%v)", code, taskEnv)
	}
	task, _ := taskEnv["task"].(map[string]any)
	used, _ := task["registry"].(string)
	if used != advertised {
		t.Errorf("the UI would preselect %q but a bare reference resolves to %q", advertised, used)
	}
}

// TestSearchSourcesCarryTheirBuiltinID pins the field that makes a deep link work.
//
// The stored row id is random, so GET /api/registries' `searchId` ("mcr") could
// never match it: /settings links to /search?source=<searchId> and the page
// silently fell back to the DEFAULT source — the picker showed one backend while
// the results came from another.
func TestSearchSourcesCarryTheirBuiltinID(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodGet, "/api/sources?kind=search", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/sources?kind=search = %d", code)
	}
	rows, _ := env["sources"].([]any)
	if len(rows) == 0 {
		t.Fatal("no search sources")
	}

	byBuiltin := map[string]map[string]any{}
	for _, row := range rows {
		m, _ := row.(map[string]any)
		id, _ := m["searchId"].(string)
		if id == "" {
			t.Errorf("source %v has no searchId, so a catalog deep link cannot name it", m["name"])
			continue
		}
		// The stored id must stay distinct from it — they are two identities.
		if stored, _ := m["id"].(string); stored == id {
			t.Errorf("source %v: id and searchId are both %q", m["name"], id)
		}
		byBuiltin[id] = m
	}

	// Every searchable built-in must be reachable by the id the catalog reports.
	for _, b := range registry.BuiltinSearchSources {
		m, ok := byBuiltin[b.ID]
		if !ok {
			t.Errorf("no source row carries searchId %q", b.ID)
			continue
		}
		if name, _ := m["name"].(string); name != b.Name {
			t.Errorf("searchId %q is on %q, want %q", b.ID, name, b.Name)
		}
	}

	// And the id must be the SAME string /api/registries uses, or the link is
	// still broken.
	_, regEnv := ts.envelope(http.MethodGet, "/api/registries", nil)
	regs, _ := regEnv["registries"].([]any)
	for _, r := range regs {
		reg, _ := r.(map[string]any)
		sid, _ := reg["searchId"].(string)
		if sid == "" {
			continue
		}
		if _, ok := byBuiltin[sid]; !ok {
			t.Errorf("registry catalog advertises searchId %q but no source row carries it", sid)
		}
	}
}
