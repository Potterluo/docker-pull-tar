package store

import (
	"context"
	"time"
)

// sources.go implements the SourceStore slice: the user-managed list of
// search backends ("search", keyed by url) and registry mirrors
// ("mirror", keyed by host) that the /settings page edits.
//
// The store is deliberately ignorant of the built-ins — internal/tasks
// seeds them from registry.BuiltinMirrors / registry.BuiltinSearchSources
// on first boot. Importing internal/registry here would tie persistence to
// the registry client and risk an import cycle, so this file is plain CRUD.
//
// A source is unique per (kind, name): that is what makes re-seeding a
// no-op instead of a duplicate row, and re-enabling a built-in the user
// had disabled or renamed.

// SourceStore is the configurable-source half of Store. Embedded into
// Store at the gen:store-interfaces marker.
type SourceStore interface {
	CreateSource(ctx context.Context, s *Source) error
	GetSource(ctx context.Context, id string) (*Source, error)
	ListSources(ctx context.Context, kind string) ([]Source, error) // kind "" = all
	UpdateSource(ctx context.Context, s *Source) error
	DeleteSource(ctx context.Context, id string) error
}

const sourceColumns = `id, kind, name, url, host, enabled, priority, is_default, created_at`

const insertSourceSQL = `INSERT INTO sources (` + sourceColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// updateSourceSQL does not touch created_at: a source's age is not
// editable, and re-seeding must not make an old row look new.
const updateSourceSQL = `UPDATE sources SET kind = ?, name = ?, url = ?, host = ?,
	enabled = ?, priority = ?, is_default = ? WHERE id = ?`

func scanSource(scanner interface{ Scan(dest ...any) error }) (*Source, error) {
	var (
		s         Source
		enabled   int
		isDefault int
	)
	if err := scanner.Scan(&s.ID, &s.Kind, &s.Name, &s.URL, &s.Host,
		&enabled, &s.Priority, &isDefault, &s.CreatedAt); err != nil {
		return nil, err
	}
	s.Enabled = intToBool(enabled)
	s.IsDefault = intToBool(isDefault)
	return &s, nil
}

func (d *DBStore) CreateSource(ctx context.Context, s *Source) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	_, err := d.db.ExecContext(ctx, d.rebind(insertSourceSQL),
		s.ID, s.Kind, s.Name, s.URL, s.Host,
		boolToInt(s.Enabled), s.Priority, boolToInt(s.IsDefault), s.CreatedAt)
	if err != nil && uniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (d *DBStore) GetSource(ctx context.Context, id string) (*Source, error) {
	row := d.db.QueryRowContext(ctx,
		d.rebind(`SELECT `+sourceColumns+` FROM sources WHERE id = ?`), id)
	s, err := scanSource(row)
	return s, scanErr(err)
}

// ListSources returns the sources of one kind — or every source when kind
// is empty (the kind=="" path /api/sources?kind= uses).
// Order is priority ASC, name ASC: the picker order the user configured,
// stable for equal priorities.
func (d *DBStore) ListSources(ctx context.Context, kind string) ([]Source, error) {
	q := `SELECT ` + sourceColumns + ` FROM sources`
	var args []any
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY priority ASC, name ASC`
	rows, err := d.db.QueryContext(ctx, d.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// UpdateSource rewrites a source from the record (read-modify-write).
// Colliding with another row's (kind, name) is ErrDuplicate; a missing id
// is ErrNotFound.
func (d *DBStore) UpdateSource(ctx context.Context, s *Source) error {
	res, err := d.db.ExecContext(ctx, d.rebind(updateSourceSQL),
		s.Kind, s.Name, s.URL, s.Host,
		boolToInt(s.Enabled), s.Priority, boolToInt(s.IsDefault), s.ID)
	if err != nil {
		if uniqueViolation(err) {
			return ErrDuplicate
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DBStore) DeleteSource(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, d.rebind(`DELETE FROM sources WHERE id = ?`), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
