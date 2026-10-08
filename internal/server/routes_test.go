package server

import (
	"strings"
	"testing"
)

// routes_test.go enforces the conventions of the declarative route table.
//
// The table is data precisely so these checks are possible: with a pile of
// HandleFunc calls, "every route declares a method" and "no duplicate
// registrations" are conventions nobody can verify mechanically.

// TestEveryRouteDeclaresAMethodAndPath guards the struct's contract. A row
// with an empty method would panic at registration time with a confusing
// error; catching it here names the offending row.
func TestEveryRouteDeclaresAMethodAndPath(t *testing.T) {
	s := &Server{}
	seen := make(map[string]bool)

	for i, rt := range s.routes() {
		if strings.TrimSpace(rt.method) == "" {
			t.Errorf("route %d (%s): empty method", i, rt.pattern)
		}
		if !strings.HasPrefix(rt.pattern, "/api/") {
			t.Errorf("route %d (%s): pattern must start with /api/", i, rt.pattern)
		}
		if rt.h == nil {
			t.Errorf("route %d %s %s: nil handler", i, rt.method, rt.pattern)
		}

		key := rt.method + " " + rt.pattern
		if seen[key] {
			t.Errorf("duplicate route registration: %s", key)
		}
		seen[key] = true
	}
}

// TestRoutesUseKnownMethods keeps a typo'd verb ("GETT") from producing a
// route that silently never matches.
func TestRoutesUseKnownMethods(t *testing.T) {
	known := map[string]bool{
		"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true,
	}
	s := &Server{}
	for _, rt := range s.routes() {
		if !known[rt.method] {
			t.Errorf("route %s %s: unknown HTTP method", rt.method, rt.pattern)
		}
	}
}

// TestMutatingRoutesUseAPathParameter guards against a mutation registered
// on a collection path by accident (POST /api/tasks is create; the per-row
// verbs must all carry {id}).
//
// PUT /api/settings is the deliberate exception: it is a bulk upsert of
// key/value pairs, so it addresses the collection rather than a row.
func TestMutatingRoutesUseAPathParameter(t *testing.T) {
	collectionMutations := map[string]bool{
		"PUT /api/settings": true, // bulk key/value upsert
	}
	s := &Server{}
	for _, rt := range s.routes() {
		switch rt.method {
		case "PUT", "PATCH", "DELETE":
			key := rt.method + " " + rt.pattern
			if collectionMutations[key] {
				if strings.Contains(rt.pattern, "{id}") {
					t.Errorf("%s is listed as a collection mutation but names a row", key)
				}
				continue
			}
			if !strings.Contains(rt.pattern, "{id}") {
				t.Errorf("%s %s: a row-level verb must name its row with {id} (or be listed as a documented collection mutation)", rt.method, rt.pattern)
			}
		}
	}
}

// TestCoreRoutesExist pins the surface CONTRACT §6 promises. A rename here
// breaks the frontend silently (it 404s at runtime), so it should break a
// test instead.
func TestCoreRoutesExist(t *testing.T) {
	want := []string{
		"GET /api/status",
		"GET /api/search",
		"GET /api/tags",
		"POST /api/images/inspect",
		"GET /api/tasks",
		"POST /api/tasks",
		"GET /api/tasks/{id}",
		"DELETE /api/tasks/{id}",
		"POST /api/tasks/{id}/pause",
		"POST /api/tasks/{id}/resume",
		"POST /api/tasks/{id}/cancel",
		"POST /api/tasks/{id}/retry",
		"GET /api/artifacts",
		"GET /api/artifacts/{id}",
		"DELETE /api/artifacts/{id}",
		"GET /api/sources",
		"POST /api/sources",
		"PUT /api/sources/{id}",
		"DELETE /api/sources/{id}",
		"GET /api/settings",
		"PUT /api/settings",
		"GET /api/stats",
		"GET /api/events",
	}

	s := &Server{}
	have := make(map[string]bool)
	for _, rt := range s.routes() {
		have[rt.method+" "+rt.pattern] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing route: %s", w)
		}
	}
}

// TestRouteTableGrowsAdditively is a change-detector with a purpose: the
// core surface is pinned by TestCoreRoutesExist, and this asserts the table
// has not SHRUNK below it.
//
// An exact count would be wrong here: `generator entity` appends five routes
// per entity by design (see docs/agent/add-entity.md), so any fixed number
// fails the moment a developer uses the tool as intended. What matters is
// that nothing documented went missing and no route lost its handler — both
// covered by the checks above.
func TestRouteTableGrowsAdditively(t *testing.T) {
	s := &Server{}
	const documentedCore = 23
	if got := len(s.routes()); got < documentedCore {
		t.Errorf("route count = %d, want at least the %d documented in CONTRACT §6 — a route was removed", got, documentedCore)
	}
}
