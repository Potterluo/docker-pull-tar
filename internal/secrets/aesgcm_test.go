package secrets

import (
	"encoding/base64"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

// aesgcm_test.go drives the AES-256-GCM implementation DIRECTLY, which is the
// point of keeping that code platform-independent (see aesgcm.go): on Windows
// New() hands out DPAPI, so without these tests the AES path — the one Linux
// and macOS users get — would never execute before shipping.

// testKey returns a deterministic 32-byte key, so a failure is reproducible.
func testKey(fill byte) []byte {
	key := make([]byte, keySize)
	for i := range key {
		key[i] = fill
	}
	return key
}

func mustAES(t *testing.T, key []byte) *aesProtector {
	t.Helper()
	p, err := newAESProtector(key)
	if err != nil {
		t.Fatalf("newAESProtector: %v", err)
	}
	return p
}

func TestAESProtectorRoundTrip(t *testing.T) {
	p := mustAES(t, testKey(0x2a))
	for name, plain := range map[string]string{
		"ascii": "ghp_AbCdEf0123456789",
		"cjk":   "私人仓库令牌：中文密码（含标点）",
		"empty": "",
	} {
		t.Run(name, func(t *testing.T) {
			sealed, err := p.Seal([]byte(plain))
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if plain == "" && sealed == "" {
				t.Fatalf("Seal of an empty secret returned an empty string, which Open rejects")
			}
			got, err := p.Open(sealed)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if string(got) != plain {
				t.Fatalf("round trip: got %q, want %q", got, plain)
			}
		})
	}
}

// TestAESProtectorNonceIsRandom is the property that makes a leaked ciphertext
// useless for comparison: the same secret must never seal to the same bytes.
func TestAESProtectorNonceIsRandom(t *testing.T) {
	p := mustAES(t, testKey(0x11))
	first, err := p.Seal([]byte("repeat-me"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := p.Seal([]byte("repeat-me"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if first == second {
		t.Fatalf("two seals are identical: the nonce is not random")
	}
}

// TestAESProtectorRejectsForeignOrTamperedCiphertext covers every way a blob
// can be wrong: modified, truncated, or genuine but sealed with another key
// (a copied app.db, or a replaced secret.key).
func TestAESProtectorRejectsForeignOrTamperedCiphertext(t *testing.T) {
	p := mustAES(t, testKey(0x33))
	sealed, err := p.Seal([]byte("must-not-open"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("Seal output is not base64: %v", err)
	}

	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-1] ^= 0xff
	if got, err := p.Open(base64.StdEncoding.EncodeToString(tampered)); !errors.Is(err, ErrDecrypt) {
		t.Errorf("flipped tag: got (%q, %v), want ErrDecrypt", got, err)
	}

	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)/2] ^= 0x01
	if got, err := p.Open(base64.StdEncoding.EncodeToString(flipped)); !errors.Is(err, ErrDecrypt) {
		t.Errorf("flipped ciphertext byte: got (%q, %v), want ErrDecrypt", got, err)
	}

	// The nonce is the first gcm.NonceSize() bytes; corrupting it must fail
	// just as loudly as corrupting the ciphertext.
	badNonce := append([]byte(nil), raw...)
	badNonce[0] ^= 0x01
	if got, err := p.Open(base64.StdEncoding.EncodeToString(badNonce)); !errors.Is(err, ErrDecrypt) {
		t.Errorf("flipped nonce: got (%q, %v), want ErrDecrypt", got, err)
	}

	if got, err := p.Open(base64.StdEncoding.EncodeToString(raw[:len(raw)-1])); err == nil {
		t.Errorf("truncated blob opened, returning %q", got)
	}
	if got, err := p.Open(sealed[:len(sealed)/2]); err == nil {
		t.Errorf("half a base64 blob opened, returning %q", got)
	}

	// A different key — the "secret.key was replaced / copied from another
	// machine" case — must not open it either.
	foreign := mustAES(t, testKey(0x44))
	if got, err := foreign.Open(sealed); !errors.Is(err, ErrDecrypt) {
		t.Errorf("a foreign key opened the secret: got (%q, %v), want ErrDecrypt", got, err)
	}
}

func TestAESProtectorRejectsEmptyAndGarbage(t *testing.T) {
	p := mustAES(t, testKey(0x55))
	if got, err := p.Open(""); !errors.Is(err, ErrEmptySealed) {
		t.Errorf("Open(\"\"): got (%q, %v), want ErrEmptySealed", got, err)
	}
	for _, sealed := range []string{"   ", "not base64 at all!!", "AAAA", "////", "AA=="} {
		got, err := p.Open(sealed)
		if err == nil {
			t.Errorf("Open(%q) returned %q with no error", sealed, got)
			continue
		}
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("Open(%q): got %v, want ErrMalformed", sealed, err)
		}
	}
}

func TestNewAESProtectorRejectsWrongKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := newAESProtector(make([]byte, n)); err == nil {
			t.Errorf("newAESProtector accepted a %d-byte key", n)
		}
	}
}

// TestLoadOrCreateKeyIsStable is what makes a restart work: the second call
// must return the same 32 bytes the first one wrote, not a fresh key.
func TestLoadOrCreateKeyIsStable(t *testing.T) {
	dir := t.TempDir()
	path := keyPath(dir)

	first, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("loadOrCreateKey (create): %v", err)
	}
	if len(first) != keySize {
		t.Fatalf("generated key is %d bytes, want %d", len(first), keySize)
	}
	second, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("loadOrCreateKey (load): %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("the key file changed between calls; every earlier secret would stop opening")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("key file mode is %o, want 600", perm)
		}
	}

	// A file of the wrong length is corruption, not a key.
	if err := os.WriteFile(path, []byte("too-short"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := loadOrCreateKey(path); err == nil {
		t.Errorf("loadOrCreateKey accepted a truncated key file")
	}
}

// TestAESProtectorSurvivesAKeyReload exercises the restart path end to end
// without going through New (which on Windows is DPAPI).
func TestAESProtectorSurvivesAKeyReload(t *testing.T) {
	dir := t.TempDir()
	key, err := loadOrCreateKey(keyPath(dir))
	if err != nil {
		t.Fatalf("loadOrCreateKey: %v", err)
	}
	const plain = "survives-a-restart"
	sealed, err := mustAES(t, key).Seal([]byte(plain))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	reloaded, err := loadOrCreateKey(keyPath(dir))
	if err != nil {
		t.Fatalf("loadOrCreateKey (reload): %v", err)
	}
	got, err := mustAES(t, reloaded).Open(sealed)
	if err != nil {
		t.Fatalf("Open with the reloaded key: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("round trip across a reload: got %q, want %q", got, plain)
	}
}

// TestAESProtectorDescribeIsHonest checks the label a settings page shows for
// the file-key scheme: it must name the key file and admit the boundary,
// rather than claiming the same protection DPAPI provides.
func TestAESProtectorDescribeIsHonest(t *testing.T) {
	desc := mustAES(t, testKey(0x66)).Describe()
	if !strings.Contains(desc, "AES-256-GCM") {
		t.Errorf("Describe() = %q, want the mechanism named", desc)
	}
	if !strings.Contains(desc, keyFileName) {
		t.Errorf("Describe() = %q, want %s named as the thing to protect", desc, keyFileName)
	}
	if !strings.Contains(desc, "不防") {
		t.Errorf("Describe() = %q, want the limitation stated (it protects the file, not the user session)", desc)
	}
}
