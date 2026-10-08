// Package secrets protects ONE secret at rest: the ciphertext half of a
// stored registry login.
//
// The package knows nothing about registries, HTTP or the database. It turns
// a plaintext secret into base64 text that is safe to keep in a TEXT column,
// and turns that text back into the secret — nothing else. The store layer
// (internal/store/credentials.go) holds the result and never sees plaintext.
//
// # Which protector you get
//
// New returns the best one the platform offers:
//
//	windows  DPAPI (CryptProtectData), keyed by the current user account
//	other    AES-256-GCM with a 32-byte key file under the data directory
//
// Callers must not choose an implementation themselves and must not assume
// which one they got: Describe reports it for a settings page, and the only
// portable promises are the Protector contract and that a secret sealed now
// opens later on the same machine and account.
//
// # What this does and does not buy
//
// Every implementation makes the ciphertext useless to someone who walks off
// with app.db (or the whole data directory) as long as they cannot also act
// as the current user on the original machine. None of them defend against a
// process already running as that user: DPAPI's key is unlocked for that
// user's session, and the AES key file is readable by that user. That is the
// honest boundary, and Describe says so.
package secrets

import "errors"

// Protector seals a secret for storage and opens it again.
type Protector interface {
	// Seal seals plaintext and returns base64 text safe for a TEXT column.
	// The result is non-deterministic (a fresh nonce per call), so the same
	// secret sealed twice yields two different strings that both open.
	Seal(plaintext []byte) (string, error)

	// Open reverses Seal. Empty, malformed, tampered, truncated or foreign
	// input is an error — never a panic and never a silent empty secret.
	Open(sealed string) ([]byte, error)

	// Describe is a short human label for a settings page, in the UI
	// language the rest of the app uses, that is honest about what the
	// protection actually covers.
	Describe() string
}

// Open's failure modes. They are distinct so a settings page (or a log line)
// can say which one it hit, but no caller should branch on more than "it did
// not open": all three mean the stored secret cannot be read and the user has
// to enter it again. None of them ever carries secret material.
var (
	// ErrEmptySealed is returned for an empty ciphertext. It is an error on
	// purpose: returning an empty secret would turn a missing credential
	// into an empty password, which is how a "logged out" bug becomes a
	// silent anonymous request.
	ErrEmptySealed = errors.New("secrets: 密文为空，没有可解密的凭据")

	// ErrMalformed: not base64, or too short to be one of our blobs.
	ErrMalformed = errors.New("secrets: 密文格式无效")

	// ErrDecrypt: well-formed but unopenable — tampered with, truncated, or
	// produced by another user, machine or application.
	ErrDecrypt = errors.New("secrets: 解密失败，密文已被篡改或不是当前用户/本应用加密的")
)
