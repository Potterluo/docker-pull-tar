package secrets

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// secrets_test.go is platform-agnostic on purpose: it drives the Protector
// interface only, so the same suite must pass on Windows (DPAPI) and
// elsewhere (AES-256-GCM + key file). Anything that is true of just one
// implementation belongs in that implementation's file, not here.

func mustNew(t *testing.T, dataDir string) Protector {
	t.Helper()
	p, err := New(dataDir)
	if err != nil {
		t.Fatalf("New(%q): %v", dataDir, err)
	}
	if p == nil {
		t.Fatalf("New(%q) returned a nil Protector with no error", dataDir)
	}
	if p.Describe() == "" {
		t.Fatalf("Describe() is empty; a settings page has nothing to show")
	}
	return p
}

// TestSealOpenRoundTrip covers the payloads that actually occur: an ordinary
// ASCII token, a multi-byte CJK secret (the UI is Chinese, so a password or
// note can be CJK), and the empty secret the API explicitly allows.
func TestSealOpenRoundTrip(t *testing.T) {
	p := mustNew(t, t.TempDir())
	cases := map[string]string{
		"ascii": "ghp_AbCdEf0123456789",
		"cjk":   "私人仓库令牌：中文密码（含标点）",
		"empty": "",
	}
	for name, plain := range cases {
		t.Run(name, func(t *testing.T) {
			sealed, err := p.Seal([]byte(plain))
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if sealed == "" {
				t.Fatalf("Seal returned an empty ciphertext for %q — an empty string is Open's error case, not a valid blob", plain)
			}
			// It has to be safe in a TEXT column AND survive a JSON round
			// trip: base64 is the contract, so decode it as such.
			if _, err := base64.StdEncoding.DecodeString(sealed); err != nil {
				t.Fatalf("Seal returned non-base64 text %q: %v", sealed, err)
			}
			got, err := p.Open(sealed)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if string(got) != plain {
				t.Fatalf("round trip: got %q, want %q", got, plain)
			}
			// The plaintext must not be sitting inside the ciphertext in any
			// recognisable form.
			if plain != "" && strings.Contains(sealed, plain) {
				t.Fatalf("ciphertext contains the plaintext verbatim")
			}
		})
	}
}

// TestSealIsNonDeterministic pins the random per-call nonce: two seals of the
// same secret must differ, and both must still open.
func TestSealIsNonDeterministic(t *testing.T) {
	p := mustNew(t, t.TempDir())
	const plain = "same-secret-sealed-twice"
	first, err := p.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("Seal(first): %v", err)
	}
	second, err := p.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("Seal(second): %v", err)
	}
	if first == second {
		t.Fatalf("two seals of the same plaintext are byte-identical — the nonce is not random")
	}
	for i, sealed := range []string{first, second} {
		got, err := p.Open(sealed)
		if err != nil {
			t.Fatalf("Open(seal %d): %v", i, err)
		}
		if string(got) != plain {
			t.Fatalf("Open(seal %d): got %q, want %q", i, got, plain)
		}
	}
}

// TestOpenRejectsTamperedCiphertext is the point of an authenticated mode: a
// single flipped bit must fail, not decrypt into garbage.
func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	p := mustNew(t, t.TempDir())
	const plain = "do-not-leak-this-secret"
	sealed, err := p.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("Seal output is not base64: %v", err)
	}
	if len(raw) < 2 {
		t.Fatalf("ciphertext is only %d bytes; nothing to tamper with meaningfully", len(raw))
	}

	// Flipping the final byte lands in the authenticator on both
	// implementations (DPAPI's trailing MAC, GCM's tag), which is the
	// tamper the format must catch.
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-1] ^= 0xff
	got, err := p.Open(base64.StdEncoding.EncodeToString(tampered))
	if err == nil {
		t.Fatalf("a tampered ciphertext opened successfully, returning %q", got)
	}
	if !errors.Is(err, ErrDecrypt) {
		t.Errorf("tampered Open: got %v, want ErrDecrypt", err)
	}
	if strings.Contains(err.Error(), plain) {
		t.Errorf("the error message leaks the plaintext: %v", err)
	}

	// A flip in the middle must fail too, whatever it lands on.
	mid := append([]byte(nil), raw...)
	mid[len(mid)/2] ^= 0x01
	if got, err := p.Open(base64.StdEncoding.EncodeToString(mid)); err == nil {
		t.Errorf("a ciphertext with a flipped middle byte opened, returning %q", got)
	}

	// Truncation is the other half of the same attack.
	if got, err := p.Open(base64.StdEncoding.EncodeToString(raw[:len(raw)-1])); err == nil {
		t.Errorf("a truncated ciphertext opened, returning %q", got)
	}
}

// TestOpenRejectsEmptyAndGarbage pins "never panic, never a silent empty
// secret": every one of these must be an error, and the empty string in
// particular must NOT come back as an empty secret.
func TestOpenRejectsEmptyAndGarbage(t *testing.T) {
	p := mustNew(t, t.TempDir())

	got, err := p.Open("")
	if err == nil {
		t.Fatalf("Open(\"\") returned %q with no error", got)
	}
	if !errors.Is(err, ErrEmptySealed) {
		t.Errorf("Open(\"\"): got %v, want ErrEmptySealed", err)
	}

	for _, sealed := range []string{
		"   ",
		"not base64 at all!!",
		"AAAA",
		"////",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		got, err := p.Open(sealed)
		if err == nil {
			t.Errorf("Open(%q) returned %q with no error", sealed, got)
			continue
		}
		if len(got) != 0 {
			t.Errorf("Open(%q) returned %d bytes alongside its error", sealed, len(got))
		}
	}
}

// TestNewNeverFailsOnThisPlatform is the wiring contract: constructing the
// protector at boot must not need a network, a prompt or a pre-made key.
func TestNewNeverFailsOnThisPlatform(t *testing.T) {
	p, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New(t.TempDir()): %v", err)
	}
	if p == nil {
		t.Fatalf("New returned a nil Protector with no error")
	}
}

// TestSealedSecretSurvivesANewProtector proves the protection is not
// process-local state: a protector built later, from nothing but the same
// data directory, still opens what the first one sealed. This is the
// property a restart depends on — for the key-file implementation the key
// comes back off disk, for DPAPI it comes back from the user's profile.
func TestSealedSecretSurvivesANewProtector(t *testing.T) {
	dir := t.TempDir()
	first := mustNew(t, dir)
	const plain = "credential-that-must-outlive-this-process"
	sealed, err := first.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	second := mustNew(t, dir)
	got, err := second.Open(sealed)
	if err != nil {
		t.Fatalf("a fresh protector on the same data dir could not open the secret: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("fresh protector round trip: got %q, want %q", got, plain)
	}
	if second.Describe() != first.Describe() {
		t.Errorf("Describe changed between two protectors: %q vs %q", first.Describe(), second.Describe())
	}
}
