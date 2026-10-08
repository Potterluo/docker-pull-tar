package server

import (
	"net/http"
	"strings"
	"testing"
)

// The credential API is write-only for secrets. These tests pin that: not just
// "it works", but "the password cannot come back out in any response".

func TestCredentialLifecycleAndSecretNeverReturns(t *testing.T) {
	ts := newTestServer(t)

	const password = "ghp_superSecretValue"
	code, env := ts.envelope(http.MethodPost, "/api/credentials", map[string]any{
		"host":     "ghcr.io",
		"username": "octocat",
		"secret":   password,
		"kind":     "basic",
		"note":     "ci token",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/credentials = %d (%v)", code, env)
	}
	// The create response must not echo the password back.
	assertNoSecret(t, "create response", env, password)

	created, ok := env["credential"].(map[string]any)
	if !ok {
		t.Fatalf("credential = %T, want an object", env["credential"])
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("created credential has no id")
	}
	if got, _ := created["host"].(string); got != "ghcr.io" {
		t.Errorf("host = %q", got)
	}
	if got, _ := created["hasSecret"].(bool); !got {
		t.Error("hasSecret = false, so the UI cannot tell a password is stored")
	}

	// Listing must not leak it either.
	code, env = ts.envelope(http.MethodGet, "/api/credentials", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/credentials = %d", code)
	}
	assertNoSecret(t, "list response", env, password)
	list, ok := env["credentials"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("credentials = %#v, want one row", env["credentials"])
	}
	if protection, _ := env["protection"].(string); protection == "" {
		t.Error("protection is empty; the page cannot say how secrets are stored")
	}

	// A metadata-only edit keeps the password.
	code, env = ts.envelope(http.MethodPut, "/api/credentials/"+id, map[string]any{"note": "renamed"})
	if code != http.StatusOK {
		t.Fatalf("PUT /api/credentials/%s = %d (%v)", id, code, env)
	}
	assertNoSecret(t, "update response", env, password)
	updated, _ := env["credential"].(map[string]any)
	if got, _ := updated["note"].(string); got != "renamed" {
		t.Errorf("note = %q, want renamed", got)
	}

	// Delete.
	code, env = ts.envelope(http.MethodDelete, "/api/credentials/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE /api/credentials/%s = %d (%v)", id, code, env)
	}
	_, env = ts.envelope(http.MethodGet, "/api/credentials", nil)
	if list, _ := env["credentials"].([]any); len(list) != 0 {
		t.Errorf("after delete the list has %d rows", len(list))
	}
}

// assertNoSecret walks a decoded response looking for the plaintext password,
// including inside nested maps and arrays. This is the assertion that would
// have caught a handler that returned store.Credential directly.
func assertNoSecret(t *testing.T, what string, value any, secret string) {
	t.Helper()
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch typed := v.(type) {
		case string:
			if strings.Contains(typed, secret) {
				t.Errorf("%s leaks the password at %s: %q", what, path, typed)
			}
		case map[string]any:
			for k, child := range typed {
				if strings.Contains(strings.ToLower(k), "secret") || strings.Contains(strings.ToLower(k), "password") {
					// A field literally named secret/password must not carry the
					// value; hasSecret (a bool) is fine.
					if s, isStr := child.(string); isStr && s != "" {
						t.Errorf("%s returns a non-empty %q field — secrets must be write-only", what, k)
					}
				}
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range typed {
				walk(child, path+"[]")
				_ = i
			}
		}
	}
	walk(value, "$")
}

func TestCredentialAPIValidation(t *testing.T) {
	ts := newTestServer(t)

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing host", map[string]any{"username": "u", "secret": "p", "kind": "basic"}, http.StatusBadRequest},
		{"blank secret", map[string]any{"host": "ghcr.io", "username": "u", "secret": "  ", "kind": "basic"}, http.StatusBadRequest},
		{"basic without username", map[string]any{"host": "ghcr.io", "secret": "p", "kind": "basic"}, http.StatusBadRequest},
		{"unknown kind", map[string]any{"host": "ghcr.io", "username": "u", "secret": "p", "kind": "ldap"}, http.StatusBadRequest},
		{"valid", map[string]any{"host": "ghcr.io", "username": "u", "secret": "p", "kind": "basic"}, http.StatusOK},
	} {
		code, env := ts.envelope(http.MethodPost, "/api/credentials", tc.body)
		if code != tc.want {
			t.Errorf("%s: status = %d (%v), want %d", tc.name, code, env, tc.want)
		}
	}
}

// TestCredentialUpsertsByHost pins the one-login-per-registry rule, including
// through the host aliases users actually type.
func TestCredentialUpsertsByHost(t *testing.T) {
	ts := newTestServer(t)

	for _, host := range []string{"ghcr.io", "GHCR.IO", "https://ghcr.io/"} {
		code, env := ts.envelope(http.MethodPost, "/api/credentials", map[string]any{
			"host": host, "username": "u", "secret": "p", "kind": "basic",
		})
		if code != http.StatusOK {
			t.Fatalf("POST host %q = %d (%v)", host, code, env)
		}
	}
	_, env := ts.envelope(http.MethodGet, "/api/credentials", nil)
	list, _ := env["credentials"].([]any)
	if len(list) != 1 {
		t.Fatalf("three spellings of one host produced %d rows, want 1", len(list))
	}
	if got, _ := list[0].(map[string]any)["host"].(string); got != "ghcr.io" {
		t.Errorf("host = %q, want the canonical ghcr.io", got)
	}
}

func TestCredentialDeleteMissingIsNotFound(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodDelete, "/api/credentials/cred_doesnotexist", nil)
	if code != http.StatusNotFound {
		t.Errorf("deleting a missing credential = %d (%v), want 404", code, env)
	}
}

func TestCredentialUpdateMissingIsNotFound(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodPut, "/api/credentials/cred_doesnotexist", map[string]any{"note": "x"})
	if code != http.StatusNotFound {
		t.Errorf("updating a missing credential = %d (%v), want 404", code, env)
	}
}
