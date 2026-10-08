package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// aesgcm.go is the AES-256-GCM protector: the ciphertext format and the key
// file it needs.
//
// It is deliberately NOT behind a build tag, even though only
// protect_other.go wires it up (on Windows, New returns the DPAPI protector).
// The reason is coverage: the format — nonce || ciphertext || tag, base64,
// the length checks, the tamper rejection, the key file lifecycle — is the
// part that goes silently wrong, and if it sat behind `//go:build !windows`
// the Windows test run (the one this repository's CI actually executes on
// every change) would never run a line of it before it shipped to Linux and
// macOS users. Platform-independent code, tested on every platform.
//
// Seal layout: base64(nonce || ciphertext || tag), with the nonce freshly
// random on every call. Plaintext is never written to disk, logged, or put in
// an error message.

// keyFileName is the key file inside the data directory.
const keyFileName = "secret.key"

// keySize is the AES-256 key length.
const keySize = 32

// aesProtector holds the loaded key. newAESProtector is the only constructor,
// so a fresh protector re-reads the key file — sealed text stays readable
// across processes and restarts (the tests assert that).
type aesProtector struct {
	key []byte
}

// newAESProtector wraps a key of exactly keySize bytes.
func newAESProtector(key []byte) (*aesProtector, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("secrets: AES 密钥长度 %d，应为 %d 字节", len(key), keySize)
	}
	// Keep our own copy: the caller's slice is not ours to hold on to.
	owned := make([]byte, keySize)
	copy(owned, key)
	return &aesProtector{key: owned}, nil
}

// Describe names the mechanism and its boundary: it stops the ciphertext from
// being readable on its own, but not a process running as this user.
func (aesProtector) Describe() string {
	return "本地 AES-256-GCM（密钥文件 " + keyFileName + "，可防明文泄露，不防同一用户下的其它进程）"
}

// Seal encrypts plaintext with a fresh random nonce prepended to the GCM
// output, and base64-encodes the lot.
func (a *aesProtector) Seal(plaintext []byte) (string, error) {
	gcm, err := a.gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secrets: 生成随机 nonce 失败: %w", err)
	}
	// Seal appends the ciphertext+tag to nonce, so the result is exactly
	// nonce || ciphertext || tag — one allocation, no separate concat.
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil)), nil
}

// Open reverses Seal. GCM authenticates the ciphertext and the tag, so any
// modification — a flipped bit, a truncated blob, a blob sealed with another
// key — fails here instead of returning garbage.
func (a *aesProtector) Open(sealed string) ([]byte, error) {
	if sealed == "" {
		return nil, ErrEmptySealed
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: 不是合法的 base64", ErrMalformed)
	}
	gcm, err := a.gcm()
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns+gcm.Overhead() {
		return nil, fmt.Errorf("%w: 长度 %d 短于 nonce+tag", ErrMalformed, len(raw))
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		// The stdlib message ("message authentication failed") carries no
		// secret material, so it is safe to wrap.
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return plain, nil
}

func (a *aesProtector) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(a.key)
	if err != nil {
		return nil, fmt.Errorf("secrets: 初始化 AES 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: 初始化 AES-GCM 失败: %w", err)
	}
	return gcm, nil
}

// loadOrCreateKey reads the key file, creating it on first use. Creation uses
// O_EXCL so two processes starting at once cannot each write a different key
// (the loser re-reads the winner's). A file of the wrong length is an error,
// not a key to be truncated: that is what a corrupted or truncated key file
// looks like, and silently deriving a shorter key would fail every earlier
// secret with a confusing "authentication failed".
func loadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != keySize {
			return nil, fmt.Errorf("secrets: 密钥文件 %s 长度 %d，应为 %d 字节", path, len(key), keySize)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secrets: 收紧密钥文件 %s 权限失败: %w", path, err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secrets: 读取密钥文件 %s 失败: %w", path, err)
	}

	fresh := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, fresh); err != nil {
		return nil, fmt.Errorf("secrets: 生成密钥失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Lost the race: the other process's key is the real one.
			return loadOrCreateKey(path)
		}
		return nil, fmt.Errorf("secrets: 创建密钥文件 %s 失败: %w", path, err)
	}
	if _, err := f.Write(fresh); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secrets: 写入密钥文件 %s 失败: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("secrets: 关闭密钥文件 %s 失败: %w", path, err)
	}
	return fresh, nil
}

// keyPath is the key file inside dataDir.
func keyPath(dataDir string) string { return filepath.Join(dataDir, keyFileName) }
