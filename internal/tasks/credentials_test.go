package tasks

import (
	"context"
	"strings"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
	"github.com/Potterluo/docker-pull-tar/internal/store"
)

// These tests cover the stored-credential feature end to end: sealing, host
// normalisation, the API-visible view, and — the point of the whole thing — a
// pull that authenticates with a SAVED login and no -u/-p.

func TestSaveCredentialNormalisesHostAndUpserts(t *testing.T) {
	mgr, st, _, _, _ := newTestManager(t)
	ctx := t.Context()

	// Every Docker Hub spelling must land on ONE row: users type docker.io,
	// the daemon talks to registry-1.docker.io, `docker login` writes
	// index.docker.io/v1/, and three rows would each miss on lookup.
	for _, spelling := range []string{"https://index.docker.io/v1/", "registry-1.docker.io", "DOCKER.IO"} {
		if _, err := mgr.SaveCredential(ctx, spelling, "u", "p-"+spelling, CredentialBasic, ""); err != nil {
			t.Fatalf("SaveCredential(%q): %v", spelling, err)
		}
	}
	rows, err := st.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored %d rows for three Docker Hub spellings, want 1", len(rows))
	}
	if rows[0].Host != "docker.io" {
		t.Errorf("host = %q, want the canonical docker.io", rows[0].Host)
	}
	// Re-saving replaced the secret, not appended a row.
	if rows[0].Username != "u" {
		t.Errorf("username = %q", rows[0].Username)
	}
}

func TestCredentialSecretIsSealedAndNeverExposed(t *testing.T) {
	mgr, st, _, _, _ := newTestManager(t)
	ctx := t.Context()

	const password = "ghp_thisMustNeverBeReadable"
	saved, err := mgr.SaveCredential(ctx, "ghcr.io", "octocat", password, CredentialBasic, "note")
	if err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	// What the store holds must not be the password.
	raw, err := st.GetCredential(ctx, saved.ID)
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if raw.Secret == password {
		t.Fatal("the stored secret IS the plaintext password")
	}
	if strings.Contains(raw.Secret, password) {
		t.Fatal("the stored secret CONTAINS the plaintext password")
	}
	if raw.Secret == "" {
		t.Fatal("the stored secret is empty, so nothing was sealed")
	}

	// What the API-visible view holds must not be the password either — the
	// whole point of returning a separate view type is that no handler can
	// leak the field by forgetting to strip it.
	list, err := mgr.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d rows", len(list))
	}
	if !list[0].HasSecret {
		t.Error("HasSecret = false although a secret is stored")
	}
	// CredentialView has no Secret field at all, so a JSON marshal can only
	// ever carry these fields.
	if list[0].ID != saved.ID || list[0].Host != "ghcr.io" || list[0].Username != "octocat" {
		t.Errorf("view = %+v", list[0])
	}

	// And the sealed value must still decrypt back to exactly the password.
	got, found, err := mgr.CredentialsFor(ctx, "ghcr.io", registry.Credentials{})
	if err != nil {
		t.Fatalf("CredentialsFor: %v", err)
	}
	if !found {
		t.Fatal("CredentialsFor did not find the stored login")
	}
	if got.Username != "octocat" || got.Password != password {
		t.Errorf("decrypted = %q/%q, want octocat/%q", got.Username, got.Password, password)
	}
}

// TestPullUsesStoredCredential is the feature's whole point: run a pull with NO
// credentials on the call and a saved login for that registry, against a
// registry that demands Basic auth — and it must succeed.
func TestPullUsesStoredCredential(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{
		BasicUser: "octocat",
		BasicPass: "s3cret",
	})
	img := registrytest.MustBuildImage("library/private", "latest", "amd64", "f.txt", "secret payload")
	reg.RegisterImage(img)

	mgr, _, _, _, _ := newTestManager(t)
	host := reg.Host()

	// Without a credential the pull must FAIL — otherwise this test would pass
	// for the wrong reason (the registry not actually enforcing auth).
	failed := startMockPull(t, mgr, host, "library/private", "latest", 1)
	if got := waitTask(t, mgr, failed.ID); got.Status == StatusSucceeded {
		t.Fatal("the pull succeeded with no credentials, so the registry is not enforcing auth")
	}

	// Now save the login and pull again with no -u/-p.
	if _, err := mgr.SaveCredential(t.Context(), host, "octocat", "s3cret", CredentialBasic, ""); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	ok := startMockPull(t, mgr, host, "library/private", "latest", 1)
	final := waitTask(t, mgr, ok.ID)
	if final.Status != StatusSucceeded {
		t.Fatalf("pull with a stored credential ended %q (error %q), want succeeded", final.Status, final.Error)
	}
}

// TestCredentialsForPrefersTheExplicitCredential pins the precedence: a
// credential the user just typed must win over a stored one, because their
// input is the more recent statement of intent.
func TestCredentialsForPrefersTheExplicitCredential(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	ctx := t.Context()

	if _, err := mgr.SaveCredential(ctx, "ghcr.io", "stored", "stored-pass", CredentialBasic, ""); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	explicit := registry.Credentials{Username: "typed", Password: "typed-pass"}
	got, found, err := mgr.CredentialsFor(ctx, "ghcr.io", explicit)
	if err != nil {
		t.Fatalf("CredentialsFor: %v", err)
	}
	if !found || got.Username != "typed" {
		t.Errorf("CredentialsFor = %+v (found %v), want the explicit credential", got, found)
	}
}

// TestCredentialsForReportsMissingAsNotFound is the difference between "no
// login configured" (normal for public images, must not fail the pull) and
// "a login exists but cannot be read" (must fail loudly).
func TestCredentialsForReportsMissingAndUnreadableDifferently(t *testing.T) {
	mgr, st, _, _, _ := newTestManager(t)
	ctx := t.Context()

	_, found, err := mgr.CredentialsFor(ctx, "ghcr.io", registry.Credentials{})
	if err != nil {
		t.Fatalf("no stored credential should not be an error: %v", err)
	}
	if found {
		t.Error("found = true with nothing stored")
	}

	saved, err := mgr.SaveCredential(ctx, "ghcr.io", "u", "p", CredentialBasic, "")
	if err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	// Simulate a blob that this machine/account cannot open (a copied database,
	// a rotated key file).
	broken := *saved
	broken.Secret = "bm90LWEtcmVhbC1zZWFsZWQtYmxvYg=="
	if err := st.UpdateCredential(ctx, &broken); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	_, _, openErr := mgr.CredentialsFor(ctx, "ghcr.io", registry.Credentials{})
	if openErr == nil {
		t.Fatal("an unreadable secret returned no error; the pull would silently go anonymous")
	}
	if !strings.Contains(openErr.Error(), "无法解密") {
		t.Errorf("error should explain the decryption failure, got %q", openErr)
	}
	// The message must name the host but never the secret material.
	if strings.Contains(openErr.Error(), "bm90LWE") {
		t.Error("the error leaked the sealed value")
	}
}

func TestSaveCredentialValidation(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	ctx := t.Context()

	cases := []struct {
		name     string
		host     string
		username string
		secret   string
		kind     string
	}{
		{"blank secret", "ghcr.io", "u", "   ", CredentialBasic},
		{"empty secret", "ghcr.io", "u", "", CredentialBasic},
		{"basic without a username", "ghcr.io", "", "p", CredentialBasic},
		{"unknown kind", "ghcr.io", "u", "p", "kerberos"},
	}
	for _, tc := range cases {
		if _, err := mgr.SaveCredential(ctx, tc.host, tc.username, tc.secret, tc.kind, ""); err == nil {
			t.Errorf("%s: SaveCredential returned nil, want an error", tc.name)
		}
	}
	// A token login legitimately has no username.
	if _, err := mgr.SaveCredential(ctx, "ghcr.io", "", "token-value", CredentialToken, ""); err != nil {
		t.Errorf("a token login with no username should be allowed: %v", err)
	}
}

// TestUpdateCredentialKeepsTheSecretWhenOmitted protects the reason the client
// never has to hold the password: editing a note must not wipe the login.
func TestUpdateCredentialKeepsTheSecretWhenOmitted(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	ctx := t.Context()

	saved, err := mgr.SaveCredential(ctx, "ghcr.io", "u", "original-secret", CredentialBasic, "before")
	if err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	updated, err := mgr.UpdateCredential(ctx, saved.ID, CredentialUpdate{Note: "after"})
	if err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	if updated.Note != "after" {
		t.Errorf("note = %q, want after", updated.Note)
	}
	// The secret must still decrypt to the original.
	got, found, err := mgr.CredentialsFor(ctx, "ghcr.io", registry.Credentials{})
	if err != nil || !found {
		t.Fatalf("CredentialsFor after a metadata edit: found=%v err=%v", found, err)
	}
	if got.Password != "original-secret" {
		t.Errorf("password = %q, want the original (a note edit must not clear it)", got.Password)
	}

	// Supplying a new secret replaces it.
	if _, err := mgr.UpdateCredential(ctx, saved.ID, CredentialUpdate{NewSecret: "rotated"}); err != nil {
		t.Fatalf("UpdateCredential(new secret): %v", err)
	}
	got, _, err = mgr.CredentialsFor(ctx, "ghcr.io", registry.Credentials{})
	if err != nil {
		t.Fatalf("CredentialsFor: %v", err)
	}
	if got.Password != "rotated" {
		t.Errorf("password = %q, want rotated", got.Password)
	}
}

// TestUpdateCredentialRefusesAOccupiedHost means "move this login to another
// registry" cannot silently merge two logins into one.
func TestUpdateCredentialRefusesAnOccupiedHost(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	ctx := t.Context()

	first, err := mgr.SaveCredential(ctx, "ghcr.io", "a", "pa", CredentialBasic, "")
	if err != nil {
		t.Fatalf("SaveCredential(ghcr): %v", err)
	}
	if _, err := mgr.SaveCredential(ctx, "quay.io", "b", "pb", CredentialBasic, ""); err != nil {
		t.Fatalf("SaveCredential(quay): %v", err)
	}
	if _, err := mgr.UpdateCredential(ctx, first.ID, CredentialUpdate{Host: "quay.io"}); err == nil {
		t.Fatal("moving a login onto an occupied host returned nil, want an error")
	}
	// A free host is accepted.
	if _, err := mgr.UpdateCredential(ctx, first.ID, CredentialUpdate{Host: "registry.k8s.io"}); err != nil {
		t.Errorf("moving to a free host failed: %v", err)
	}
}

func TestDeleteCredentialByHost(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	ctx := t.Context()

	if _, err := mgr.SaveCredential(ctx, "ghcr.io", "u", "p", CredentialBasic, ""); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	// An unnormalised spelling must find the row.
	if err := mgr.DeleteCredentialByHost(ctx, "https://ghcr.io/"); err != nil {
		t.Fatalf("DeleteCredentialByHost: %v", err)
	}
	if _, found, _ := mgr.CredentialsFor(ctx, "ghcr.io", registry.Credentials{}); found {
		t.Error("the credential survived the delete")
	}
	if err := mgr.DeleteCredentialByHost(ctx, "ghcr.io"); err != store.ErrNotFound {
		t.Errorf("deleting a missing host = %v, want ErrNotFound", err)
	}
	this := mgr.ProtectorLabel()
	if this == "" {
		t.Error("ProtectorLabel is empty; the UI would show nothing about how secrets are protected")
	}
	t.Logf("protection on this platform: %s", this)
}

// TestResumedPullReResolvesTheCredential guards the resume path.
//
// The task row deliberately does not carry secrets, so a resumed private pull
// must look the login up again. If resume forgot to, every interrupted private
// download would come back as a 401.
func TestResumePathResolvesCredentialsAgain(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{BasicUser: "octocat", BasicPass: "s3cret"})
	img := registrytest.MustBuildImage("library/private", "latest", "amd64", "f.txt", "x")
	// The layer is missing, so the pull reaches the download and stays running.
	registerWithoutLayer(reg, img)

	mgr, _, _, _, _ := newTestManager(t)
	if _, err := mgr.SaveCredential(t.Context(), reg.Host(), "octocat", "s3cret", CredentialBasic, ""); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	task := startMockPull(t, mgr, reg.Host(), "library/private", "latest", 1)
	// Wait until it is actually downloading, so the pause exercises the live
	// path rather than racing the start.
	waitFor(t, "the task to start downloading", func() bool {
		got, err := mgr.Get(context.Background(), task.ID)
		return err == nil && got.Status == StatusRunning
	})

	if err := mgr.Pause(t.Context(), task.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Pause signals the worker; the row only reaches "paused" once the puller
	// has actually stopped, and Resume rejects a still-running task.
	waitFor(t, "the task to reach paused", func() bool {
		got, err := mgr.Get(context.Background(), task.ID)
		return err == nil && got.Status == StatusPaused
	})
	// Resume must not fail on a credential lookup.
	if err := mgr.Resume(t.Context(), task.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, "the resumed task to run again", func() bool {
		got, err := mgr.Get(context.Background(), task.ID)
		return err == nil && got.Status == StatusRunning
	})
}

// TestHubCredentialIsNeverSentToAnAccelerator pins a security boundary that the
// accelerator feature creates: a Docker Hub login must NOT follow a pull to a
// third-party mirror.
//
// When `default_mirror` (or the chosen download source) is an accelerator, the
// effective host becomes docker.1ms.run and the manager looks the credential up
// under THAT name. If credential lookup folded a mirror host onto docker.io —
// the way NormalizeAuthHost folds docker.io's own aliases — the operator's Hub
// password would be handed to whoever runs the mirror. Conversely the folding
// must still work between Hub's real aliases, or a login saved from the GUI
// stops matching the host the daemon pulls from.
func TestHubCredentialIsNeverSentToAnAccelerator(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	ctx := t.Context()

	if _, err := mgr.SaveCredential(ctx, "docker.io", "hubuser", "hubpass", CredentialBasic, ""); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	// 1. Hub's own aliases must all resolve to the one login.
	for _, alias := range []string{
		"docker.io",
		"registry-1.docker.io",
		"index.docker.io",
		"https://index.docker.io/v1/",
	} {
		got, found, err := mgr.CredentialsFor(ctx, alias, registry.Credentials{})
		if err != nil {
			t.Fatalf("CredentialsFor(%q): %v", alias, err)
		}
		if !found || got.Username != "hubuser" {
			t.Errorf("CredentialsFor(%q) = %+v (found %v), want the stored Hub login", alias, got, found)
		}
	}

	// 2. An accelerator must NOT inherit it.
	for _, mirror := range []string{
		"docker.1ms.run",
		"docker.nju.edu.cn",
		"docker.xuanyuan.me",
		"docker.m.daocloud.io",
	} {
		got, found, err := mgr.CredentialsFor(ctx, mirror, registry.Credentials{})
		if err != nil {
			t.Fatalf("CredentialsFor(%q): %v", mirror, err)
		}
		if found {
			t.Errorf("CredentialsFor(%q) returned %+v; a Hub credential must never be offered to an accelerator",
				mirror, got)
		}
	}
}
