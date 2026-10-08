package store

import (
	"context"
	"time"
)

// settings.go implements the SettingStore slice: the key/value table
// behind 设置 (proxy mode, workers, TLS verification, default mirror,
// chunk threshold …).
//
// The DEFAULTS are not here. Each consumer (internal/tasks, the HTTP
// layer) applies its own default when GetSetting reports ErrNotFound, so
// a fresh install has an empty table and still behaves, and clearing a
// preference is just a DELETE away. CONTRACT §4 lists the key names:
// output_dir, workers, verify_tls, proxy_mode, proxy_url,
// default_mirror, default_search_source, chunk_threshold_mb, max_retries.

// SettingStore is the app-preferences half of Store. Embedded into Store
// at the gen:store-interfaces marker.
type SettingStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	ListSettings(ctx context.Context) ([]Setting, error)
}

const settingColumns = `key, value, updated_at`

const insertSettingSQL = `INSERT INTO settings (` + settingColumns + `) VALUES (?, ?, ?)`

const updateSettingSQL = `UPDATE settings SET value = ?, updated_at = ? WHERE key = ?`

func scanSetting(scanner interface{ Scan(dest ...any) error }) (*Setting, error) {
	var s Setting
	if err := scanner.Scan(&s.Key, &s.Value, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSetting returns the stored value. A key that was never set is
// ErrNotFound — callers fall back to their default rather than treating
// an empty string as "explicitly blank".
func (d *DBStore) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := d.db.QueryRowContext(ctx,
		d.rebind(`SELECT value FROM settings WHERE key = ?`), key).Scan(&value)
	if err != nil {
		return "", scanErr(err)
	}
	return value, nil
}

// SetSetting upserts one preference. It is written portably on purpose:
// an UPDATE first and an INSERT only when nothing was updated, rather than
// `ON CONFLICT ... DO UPDATE` / `ON DUPLICATE KEY UPDATE`, whose support
// differs between sqlite and postgres as the drivers are wired here. Two
// statements but exactly one method, and no dialect branch — the same
// reasoning as writing every other query with `?` + d.rebind.
//
// The write is not a transaction: both statements are single-statement
// and idempotent in effect, and a lost race between two upserts of the
// same key still leaves exactly one row with one of the two values.
func (d *DBStore) SetSetting(ctx context.Context, key, value string) error {
	now := time.Now().UTC()
	res, err := d.db.ExecContext(ctx, d.rebind(updateSettingSQL), value, now, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = d.db.ExecContext(ctx, d.rebind(insertSettingSQL), key, value, now)
	return err
}

// ListSettings returns every override, key-ordered so the settings page
// renders deterministically.
func (d *DBStore) ListSettings(ctx context.Context) ([]Setting, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+settingColumns+` FROM settings ORDER BY key ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		s, err := scanSetting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}
