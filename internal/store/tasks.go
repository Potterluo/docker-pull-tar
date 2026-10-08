package store

import (
	"context"
	"time"
)

// tasks.go implements the download-task slice of Store (CONTRACT §4):
// one durable row per pull job. It is the persisted half of 断点续传 —
// internal/tasks points a resumed job at the same work_dir and the byte
// counters in this row are what the /tasks page renders after a restart.
//
// Tasks are single-user: there is no user_id column, so nothing in this
// file registers for the DeleteUser cascade (see users.go).
//
// Queries are written with plain `?` placeholders and wrapped in
// d.rebind (db.go): one statement per query, identical SQL on sqlite and
// postgres.

// boolToInt / intToBool convert flags at the store boundary. SQLite has
// no BOOLEAN type and the columns are INTEGER on both dialects, so every
// bool is written as 0/1 and read back through intToBool — the same
// boundary conversion as files.go's FileRecord does for its own columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// intToBool is the read half of boolToInt.
func intToBool(n int) bool { return n != 0 }

// TaskStore is the task half of Store: the job rows plus their per-blob
// rows (tasklayers.go). Embedded into Store at the gen:store-interfaces
// marker.
type TaskStore interface {
	CreateTask(ctx context.Context, t *Task) error
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasks(ctx context.Context, limit int) ([]Task, error)
	UpdateTask(ctx context.Context, t *Task) error
	DeleteTask(ctx context.Context, id string) error

	ReplaceTaskLayers(ctx context.Context, taskID string, layers []TaskLayer) error
	ListTaskLayers(ctx context.Context, taskID string) ([]TaskLayer, error)
	UpdateTaskLayer(ctx context.Context, l *TaskLayer) error
}

const taskColumns = `id, ref, registry, repository, tag, digest, platform, status, error, total_bytes, downloaded_bytes, speed, work_dir, tar_path, tar_size, workers, insecure, verify_tls, created_at, started_at, updated_at, finished_at`

const insertTaskSQL = `INSERT INTO tasks (` + taskColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// updateTaskSQL leaves created_at alone (the row's birthday) and refreshes
// updated_at from the record.
const updateTaskSQL = `UPDATE tasks SET ref = ?, registry = ?, repository = ?, tag = ?, digest = ?,
	platform = ?, status = ?, error = ?, total_bytes = ?, downloaded_bytes = ?, speed = ?,
	work_dir = ?, tar_path = ?, tar_size = ?, workers = ?, insecure = ?, verify_tls = ?,
	started_at = ?, updated_at = ?, finished_at = ? WHERE id = ?`

func scanTask(scanner interface{ Scan(dest ...any) error }) (*Task, error) {
	var (
		t        Task
		insecure int
		verify   int
	)
	if err := scanner.Scan(
		&t.ID, &t.Ref, &t.Registry, &t.Repository, &t.Tag, &t.Digest, &t.Platform,
		&t.Status, &t.Error, &t.TotalBytes, &t.DownloadedBytes, &t.Speed,
		&t.WorkDir, &t.TarPath, &t.TarSize, &t.Workers, &insecure, &verify,
		&t.CreatedAt, &t.StartedAt, &t.UpdatedAt, &t.FinishedAt,
	); err != nil {
		return nil, err
	}
	t.Insecure = intToBool(insecure)
	t.VerifyTLS = intToBool(verify)
	return &t, nil
}

// CreateTask inserts the job row and fills CreatedAt/UpdatedAt when the
// caller left them zero, so a caller can persist a task and immediately
// render it (or list it in a stable order) without touching the clock.
// Config columns are stored exactly as given — defaults are the caller's
// business (internal/tasks resolves them), except that the DDL defaults
// still cover rows inserted by hand.
func (d *DBStore) CreateTask(ctx context.Context, t *Task) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}
	_, err := d.db.ExecContext(ctx, d.rebind(insertTaskSQL),
		t.ID, t.Ref, t.Registry, t.Repository, t.Tag, t.Digest, t.Platform,
		t.Status, t.Error, t.TotalBytes, t.DownloadedBytes, t.Speed,
		t.WorkDir, t.TarPath, t.TarSize, t.Workers,
		boolToInt(t.Insecure), boolToInt(t.VerifyTLS),
		t.CreatedAt, t.StartedAt, t.UpdatedAt, t.FinishedAt)
	if err != nil && uniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (d *DBStore) GetTask(ctx context.Context, id string) (*Task, error) {
	row := d.db.QueryRowContext(ctx, d.rebind(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`), id)
	t, err := scanTask(row)
	return t, scanErr(err)
}

// ListTasks returns the most recent jobs first. limit <= 0 means "no
// limit" (the CLI's tasks list); the HTTP layer passes its own page size.
func (d *DBStore) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks ORDER BY created_at DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, d.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// UpdateTask rewrites the mutable columns from the record — the pattern is
// read-modify-write (GetTask, mutate, UpdateTask), which keeps the SQL
// surface to one statement per call site. UpdatedAt is always refreshed;
// CreatedAt is never touched. A missing row is ErrNotFound, not a silent
// success, so a worker updating a task the user just deleted notices.
func (d *DBStore) UpdateTask(ctx context.Context, t *Task) error {
	t.UpdatedAt = time.Now().UTC()
	res, err := d.db.ExecContext(ctx, d.rebind(updateTaskSQL),
		t.Ref, t.Registry, t.Repository, t.Tag, t.Digest, t.Platform,
		t.Status, t.Error, t.TotalBytes, t.DownloadedBytes, t.Speed,
		t.WorkDir, t.TarPath, t.TarSize, t.Workers,
		boolToInt(t.Insecure), boolToInt(t.VerifyTLS),
		t.StartedAt, t.UpdatedAt, t.FinishedAt, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTask removes the job and its layer rows in one transaction: the
// tables carry no foreign keys (nothing here cascades), so an unpaired
// delete would leave orphan task_layers rows behind forever.
func (d *DBStore) DeleteTask(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, d.rebind(`DELETE FROM task_layers WHERE task_id = ?`), id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, d.rebind(`DELETE FROM tasks WHERE id = ?`), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
