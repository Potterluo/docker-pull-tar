package store

import (
	"context"
	"time"
)

// artifacts.go implements the ArtifactStore slice: the index of finished
// .tar files. The BYTES live on disk under the configured output_dir —
// only metadata is in the database, the same split files.go uses for
// uploads. `docker load -i <path>` is what consumes the file; this row is
// what the /artifacts page lists and /api/artifacts/{id} streams.
//
// path is UNIQUE: it is the filesystem identity of the artifact, so
// re-scanning an output directory (internal/tasks.ScanArtifacts)
// reconciles by path instead of duplicating rows.
//
// Artifacts are single-user: no user_id column, so nothing here registers
// for the DeleteUser cascade (see users.go).

// ArtifactStore is the finished-artifact half of Store. Embedded into
// Store at the gen:store-interfaces marker.
type ArtifactStore interface {
	CreateArtifact(ctx context.Context, a *Artifact) error
	GetArtifact(ctx context.Context, id string) (*Artifact, error)
	ListArtifacts(ctx context.Context) ([]Artifact, error)
	UpdateArtifact(ctx context.Context, a *Artifact) error
	DeleteArtifact(ctx context.Context, id string) error
}

const artifactColumns = `id, name, path, repository, tag, platform, size, task_id, created_at`

const insertArtifactSQL = `INSERT INTO artifacts (` + artifactColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

func scanArtifact(scanner interface{ Scan(dest ...any) error }) (*Artifact, error) {
	var a Artifact
	if err := scanner.Scan(&a.ID, &a.Name, &a.Path, &a.Repository, &a.Tag,
		&a.Platform, &a.Size, &a.TaskID, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateArtifact indexes a finished tar. CreatedAt is filled when zero so
// the puller can register an artifact the moment the file is closed.
// Inserting a second row for the same path is ErrDuplicate — the caller
// should treat the file as already known.
func (d *DBStore) CreateArtifact(ctx context.Context, a *Artifact) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := d.db.ExecContext(ctx, d.rebind(insertArtifactSQL),
		a.ID, a.Name, a.Path, a.Repository, a.Tag, a.Platform, a.Size, a.TaskID, a.CreatedAt)
	if err != nil && uniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (d *DBStore) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	row := d.db.QueryRowContext(ctx,
		d.rebind(`SELECT `+artifactColumns+` FROM artifacts WHERE id = ?`), id)
	a, err := scanArtifact(row)
	return a, scanErr(err)
}

// ListArtifacts returns newest first — the /artifacts page order, and the
// order the dashboard's "recent" counters read.
func (d *DBStore) ListArtifacts(ctx context.Context) ([]Artifact, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// DeleteArtifact drops the index row only. Deleting the file is the
// caller's decision (DELETE /api/artifacts/{id} does both), so a failed
// unlink never loses the row that describes the file.
// UpdateArtifact refreshes an existing row, in place.
//
// It exists so a re-download that rewrites the same path can fix the recorded
// size without a delete-then-insert: if that insert ever failed (a closing
// store, a crashed process) the delete had already lost the row, leaving the
// file untracked until the next directory scan.
func (d *DBStore) UpdateArtifact(ctx context.Context, a *Artifact) error {
	res, err := d.db.ExecContext(ctx, d.rebind(
		`UPDATE artifacts SET name = ?, path = ?, repository = ?, tag = ?, platform = ?,
		 size = ?, task_id = ?, created_at = ? WHERE id = ?`),
		a.Name, a.Path, a.Repository, a.Tag, a.Platform,
		a.Size, a.TaskID, a.CreatedAt, a.ID)
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

func (d *DBStore) DeleteArtifact(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, d.rebind(`DELETE FROM artifacts WHERE id = ?`), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
