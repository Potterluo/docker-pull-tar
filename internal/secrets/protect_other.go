//go:build !windows

package secrets

import (
	"errors"
	"fmt"
	"os"
)

// protect_other.go is the non-Windows wiring: AES-256-GCM with a random
// 32-byte key kept in <dataDir>/secret.key (0600, inside a 0700 directory).
//
// This is a FILE-key scheme, and it is honest about what that means: the
// ciphertext in app.db is useless without secret.key, so it protects against
// casual disclosure (a copied app.db, a backup, a support bundle, a
// screenshot of the table) — but a process already running as the same user
// can read the key file, so it is NOT protection against a hostile local
// process. Windows gets DPAPI instead precisely because DPAPI has no such
// file. Describe() says all of this in one line.
//
// The cipher and the key-file handling live in aesgcm.go, which is
// platform-independent so its tests run on Windows too; this file is the
// platform-specific part: where the key lives and how the directory around it
// is created.

// New loads (or, on first use, creates) the AES key under dataDir and returns
// the protector backed by it.
func New(dataDir string) (Protector, error) {
	if dataDir == "" {
		return nil, errors.New("secrets: 数据目录为空，无法放置密钥文件")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("secrets: 创建数据目录 %s 失败: %w", dataDir, err)
	}
	// MkdirAll's mode only applies to directories it creates; an existing one
	// may be wider, so tighten it explicitly.
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("secrets: 收紧数据目录 %s 权限失败: %w", dataDir, err)
	}
	key, err := loadOrCreateKey(keyPath(dataDir))
	if err != nil {
		return nil, err
	}
	return newAESProtector(key)
}
