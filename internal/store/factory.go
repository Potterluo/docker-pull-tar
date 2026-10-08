package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// StorageConfig selects the backend. Type is "sqlite" (default, zero
// config) or "postgres" (production). DSN empty for sqlite means
// "<DataDir>/app.db" with tuned PRAGMAs.
type StorageConfig struct {
	Type        string
	DSN         string
	AutoMigrate bool
}

// New opens the configured Store.
//
// The sqlite DSN carries PRAGMA settings as URL params (modernc.org/sqlite
// reads them from `_pragma=`): WAL lets readers proceed while one writer
// works (default rollback-journal mode blocks everything on write and
// produces "database is locked" under load), busy_timeout makes contended
// writers wait 5s instead of erroring, synchronous=NORMAL is the standard
// WAL pairing, and foreign_keys is on as a matter of hygiene.
func New(cfg *StorageConfig, dataDir string) (Store, error) {
	if cfg == nil || cfg.Type == "" {
		cfg = &StorageConfig{Type: "sqlite", AutoMigrate: true}
	}
	dsn := cfg.DSN
	if cfg.Type == "sqlite" && dsn == "" {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
		}
		dsn = "file:" + filepath.Join(dataDir, "app.db") +
			"?_pragma=journal_mode(WAL)" +
			"&_pragma=busy_timeout(5000)" +
			"&_pragma=synchronous(NORMAL)" +
			"&_pragma=foreign_keys(1)"
	}
	slog.Info("using database storage", "dialect", cfg.Type, "dsn", maskDSN(dsn))
	db, err := NewDBStore(cfg.Type, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if cfg.AutoMigrate {
		if err := db.Migrate(context.Background()); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return db, nil
}

// maskDSN keeps credentials out of logs.
func maskDSN(dsn string) string {
	if len(dsn) > 20 {
		return dsn[:10] + "***" + dsn[len(dsn)-5:]
	}
	return "***"
}
