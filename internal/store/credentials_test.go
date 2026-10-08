package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// credentials_test.go covers the saved-registry-login slice: the CRUD round
// trip (including a CJK note), the UNIQUE host that makes "one login per
// registry host" true, both ErrNotFound paths, the updated_at bump, the host
// lookup, and the newest-first order ListCredentials promises.
//
// The secret is treated as opaque TEXT here on purpose — sealing is
// internal/secrets' job, and this package must never have an opinion about
// what is inside the column, so nothing in this file asserts anything about
// its shape beyond "it comes back byte-identical".

// sealedFixture is plausible-looking ciphertext: this package only ever
// moves it in and out of the column.
const sealedFixture = "AQAAANCMnd8BFdERjHoAwE/Cl+sBAAAAexample-blob-not-a-password"

func TestCredentialCreateGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	c := &Credential{
		Host:     "ghcr.io",
		Username: "octocat",
		Secret:   sealedFixture,
		Kind:     "token",
		// A CJK note is the normal case in this app, not an edge case: the
		// UI is Chinese, so the round trip has to survive multi-byte UTF-8
		// through the driver, the column and the scanner.
		Note: "GitHub 私人仓库令牌（只读，2024 年更新）",
	}
	if err := st.CreateCredential(ctx, c); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	// An empty ID means "mint one", and it is filled in on the record so a
	// caller can render/link the row without re-reading it.
	if c.ID == "" {
		t.Fatalf("CreateCredential left ID empty; it must generate one")
	}
	if !strings.HasPrefix(c.ID, "cred_") {
		t.Errorf("generated id %q does not follow the <prefix><hex> id shape", c.ID)
	}
	if c.CreatedAt.IsZero() {
		t.Fatalf("CreateCredential left CreatedAt zero")
	}
	if c.UpdatedAt.IsZero() {
		t.Fatalf("CreateCredential left UpdatedAt zero")
	}
	if !c.UpdatedAt.Equal(c.CreatedAt) {
		t.Errorf("a brand-new row has updated_at %s but created_at %s; want them equal", c.UpdatedAt, c.CreatedAt)
	}

	got, err := st.GetCredential(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if got.ID != c.ID || got.Host != "ghcr.io" || got.Username != "octocat" ||
		got.Secret != sealedFixture || got.Kind != "token" || got.Note != c.Note {
		t.Errorf("credential columns did not round-trip:\n got  %+v\n want %+v", got, c)
	}
	if !got.CreatedAt.Equal(c.CreatedAt) || !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("timestamps did not round-trip: got %s/%s, want %s/%s",
			got.CreatedAt, got.UpdatedAt, c.CreatedAt, c.UpdatedAt)
	}

	// A caller-supplied id is honoured as-is (imports, tests, hand-written
	// rows from a bug report).
	explicit := &Credential{ID: "cred_explicit", Host: "registry.gitlab.com", Secret: "x"}
	if err := st.CreateCredential(ctx, explicit); err != nil {
		t.Fatalf("CreateCredential(explicit id): %v", err)
	}
	if explicit.ID != "cred_explicit" {
		t.Errorf("explicit id was rewritten to %q", explicit.ID)
	}
	if _, err := st.GetCredential(ctx, "cred_explicit"); err != nil {
		t.Errorf("GetCredential(explicit id): %v", err)
	}
}

// TestCredentialDDLDefaults pins the layer-1 migration contract: every
// column except host has a DEFAULT, so a row inserted without them scans back
// as a usable value instead of a NULL that fails to scan.
func TestCredentialDDLDefaults(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	dbs, ok := st.(*DBStore)
	if !ok {
		t.Fatalf("NewTestStore returned %T, want *DBStore", st)
	}
	// Only the identity columns, the way a future code path that forgets a
	// field (or a hand-written INSERT) would.
	if _, err := dbs.db.ExecContext(ctx,
		`INSERT INTO credentials (id, host) VALUES ('cred_defaults', 'quay.io')`); err != nil {
		t.Fatalf("raw insert with only id+host: %v", err)
	}
	got, err := st.GetCredential(ctx, "cred_defaults")
	if err != nil {
		t.Fatalf("GetCredential on a defaulted row: %v", err)
	}
	if got.Username != "" || got.Secret != "" || got.Note != "" {
		t.Errorf("text defaults: got %+v, want empty username/secret/note", got)
	}
	if got.Kind != "basic" {
		t.Errorf("kind default: got %q, want %q", got.Kind, "basic")
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamp defaults did not scan back: created=%s updated=%s", got.CreatedAt, got.UpdatedAt)
	}
}

// TestCredentialHostIsUnique is the "one saved login per host" rule, on both
// the insert and the update path.
func TestCredentialHostIsUnique(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	first := &Credential{ID: "cred_a", Host: "ghcr.io", Username: "a", Secret: "s1"}
	if err := st.CreateCredential(ctx, first); err != nil {
		t.Fatalf("CreateCredential(first): %v", err)
	}
	dup := &Credential{ID: "cred_b", Host: "ghcr.io", Username: "b", Secret: "s2"}
	if err := st.CreateCredential(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second credential for the same host: got %v, want ErrDuplicate", err)
	}

	// The rejected insert must not have left a row behind.
	list, err := st.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("a duplicate insert left %d rows, want 1", len(list))
	}

	// Renaming one entry onto another's host collides the same way.
	if err := st.CreateCredential(ctx, &Credential{ID: "cred_c", Host: "quay.io", Secret: "s3"}); err != nil {
		t.Fatalf("CreateCredential(quay.io): %v", err)
	}
	moved, err := st.GetCredential(ctx, "cred_c")
	if err != nil {
		t.Fatalf("GetCredential(cred_c): %v", err)
	}
	moved.Host = "ghcr.io"
	if err := st.UpdateCredential(ctx, moved); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("updating onto an existing host: got %v, want ErrDuplicate", err)
	}
	if got, err := st.GetCredential(ctx, "cred_c"); err != nil || got.Host != "quay.io" {
		t.Errorf("the rejected update changed the row: host=%q err=%v", got.Host, err)
	}
}

func TestCredentialGetMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	if _, err := st.GetCredential(ctx, "cred_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCredential on a missing id: got %v, want ErrNotFound", err)
	}
}

// TestCredentialUpdate covers the read-modify-write path: the editable fields
// change, created_at is untouched, and updated_at is stamped by the method
// itself (the caller cannot forget it).
func TestCredentialUpdate(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	// A deliberately old birthday, so "bumped" is unambiguous whatever the
	// clock resolution is.
	old := time.Now().UTC().Add(-24 * time.Hour)
	c := &Credential{
		ID: "cred_upd", Host: "registry.gitlab.com", Username: "old-user",
		Secret: "cipher-old", Kind: "basic", Note: "旧凭据",
		CreatedAt: old, UpdatedAt: old,
	}
	if err := st.CreateCredential(ctx, c); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if !c.UpdatedAt.Equal(old) {
		t.Fatalf("an explicit UpdatedAt was overwritten on insert: got %s, want %s", c.UpdatedAt, old)
	}

	c.Username = "new-user"
	c.Secret = "cipher-new"
	c.Kind = "token"
	c.Note = "新令牌"
	if err := st.UpdateCredential(ctx, c); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}

	got, err := st.GetCredential(ctx, "cred_upd")
	if err != nil {
		t.Fatalf("GetCredential after update: %v", err)
	}
	if got.Username != "new-user" || got.Secret != "cipher-new" || got.Kind != "token" || got.Note != "新令牌" {
		t.Errorf("updated fields did not persist: %+v", got)
	}
	if !got.CreatedAt.Equal(old) {
		t.Errorf("UpdateCredential rewrote created_at: got %s, want %s", got.CreatedAt, old)
	}
	if !got.UpdatedAt.After(old) {
		t.Errorf("UpdateCredential did not bump updated_at: got %s, want something after %s", got.UpdatedAt, old)
	}
	// The stamp comes from the method, and the caller's record carries it, so
	// a page can render "updated just now" without re-reading the row.
	if !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("updated_at in the row (%s) differs from the record (%s)", got.UpdatedAt, c.UpdatedAt)
	}
}

func TestCredentialUpdateMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	if err := st.UpdateCredential(ctx, &Credential{ID: "cred_missing", Host: "ghcr.io"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateCredential on a missing id: got %v, want ErrNotFound", err)
	}
}

func TestCredentialDelete(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	if err := st.CreateCredential(ctx, &Credential{ID: "cred_del", Host: "ghcr.io", Secret: "s"}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := st.DeleteCredential(ctx, "cred_del"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, err := st.GetCredential(ctx, "cred_del"); !errors.Is(err, ErrNotFound) {
		t.Errorf("credential survived DeleteCredential: %v", err)
	}
	if err := st.DeleteCredential(ctx, "cred_del"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteCredential: got %v, want ErrNotFound", err)
	}
	// Deleting frees the host for a later save of the same registry.
	if err := st.CreateCredential(ctx, &Credential{ID: "cred_del2", Host: "ghcr.io", Secret: "s2"}); err != nil {
		t.Errorf("re-saving a deleted host: %v", err)
	}
}

func TestCredentialFindByHost(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	if err := st.CreateCredential(ctx, &Credential{
		ID: "cred_host", Host: "ghcr.io", Username: "octocat", Secret: sealedFixture, Kind: "token",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	got, err := st.FindCredentialByHost(ctx, "ghcr.io")
	if err != nil {
		t.Fatalf("FindCredentialByHost(hit): %v", err)
	}
	if got.ID != "cred_host" || got.Username != "octocat" || got.Secret != sealedFixture {
		t.Errorf("FindCredentialByHost returned the wrong row: %+v", got)
	}

	if _, err := st.FindCredentialByHost(ctx, "quay.io"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindCredentialByHost(miss): got %v, want ErrNotFound", err)
	}
	// The match is exact: hosts are stored normalised (lower-case, no
	// scheme), so a caller that forgot to normalise gets a clean miss
	// instead of a second row for the same registry.
	for _, notNormalised := range []string{"GHCR.IO", "https://ghcr.io", "ghcr.io/", " ghcr.io"} {
		if _, err := st.FindCredentialByHost(ctx, notNormalised); !errors.Is(err, ErrNotFound) {
			t.Errorf("FindCredentialByHost(%q): got %v, want ErrNotFound (exact match only)", notNormalised, err)
		}
	}
}

// TestCredentialListOrdering pins newest-first, which is what the settings
// page renders.
func TestCredentialListOrdering(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	base := time.Now().UTC().Add(-72 * time.Hour)
	for i, host := range []string{"ghcr.io", "quay.io", "registry.gitlab.com"} {
		if err := st.CreateCredential(ctx, &Credential{
			ID:        "cred_" + host,
			Host:      host,
			Secret:    sealedFixture,
			CreatedAt: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("CreateCredential(%s): %v", host, err)
		}
	}

	list, err := st.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListCredentials: got %d rows, want 3", len(list))
	}
	want := []string{"cred_registry.gitlab.com", "cred_quay.io", "cred_ghcr.io"}
	for i, id := range want {
		if list[i].ID != id {
			t.Fatalf("ListCredentials order: [%d] = %s, want %s (newest first)", i, list[i].ID, id)
		}
	}

	if err := st.DeleteCredential(ctx, "cred_quay.io"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	list, err = st.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials after delete: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListCredentials after delete: got %d rows, want 2", len(list))
	}
}
