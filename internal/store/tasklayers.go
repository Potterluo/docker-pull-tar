package store

import "context"

// tasklayers.go implements the per-blob half of TaskStore: one row per
// layer (and one for the image config blob), so the /tasks page can draw
// a per-layer progress bar and a resumed job knows which blobs are done.
//
// The layer set is REPLACED, never patched in place, whenever the puller
// re-resolves a manifest: a re-resolved index can add or drop blobs, and
// a positional diff of that is more code than it is worth. Byte-level
// progress during a run goes through UpdateTaskLayer.

const taskLayerColumns = `id, task_id, digest, kind, name, position, size, downloaded, status, error`

const insertTaskLayerSQL = `INSERT INTO task_layers (` + taskLayerColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const updateTaskLayerSQL = `UPDATE task_layers SET digest = ?, kind = ?, name = ?, position = ?,
	size = ?, downloaded = ?, status = ?, error = ? WHERE id = ?`

func scanTaskLayer(scanner interface{ Scan(dest ...any) error }) (*TaskLayer, error) {
	var l TaskLayer
	if err := scanner.Scan(&l.ID, &l.TaskID, &l.Digest, &l.Kind, &l.Name, &l.Position,
		&l.Size, &l.Downloaded, &l.Status, &l.Error); err != nil {
		return nil, err
	}
	return &l, nil
}

// ReplaceTaskLayers makes layers the complete layer set of taskID: it
// deletes the task's existing rows and inserts the slice in one
// transaction, so a reader never observes a half-replaced task (progress
// polls run concurrently with a resume).
//
// Two conveniences for callers building the slice from a manifest, both
// write-backs onto the slice so the caller's copy matches what is stored:
//
//   - TaskID is taken from the taskID argument (a layer can only belong
//     to the task whose set is being replaced).
//   - Position 0 is filled from the slice index, so a caller that passes
//     layers in manifest order gets the right order without numbering
//     them by hand. A caller that numbers layers itself keeps its values.
func (d *DBStore) ReplaceTaskLayers(ctx context.Context, taskID string, layers []TaskLayer) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, d.rebind(`DELETE FROM task_layers WHERE task_id = ?`), taskID); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, d.rebind(insertTaskLayerSQL))
	if err != nil {
		return err
	}
	defer ins.Close()
	for i := range layers {
		l := &layers[i]
		l.TaskID = taskID
		if l.Position == 0 {
			l.Position = i
		}
		if _, err := ins.ExecContext(ctx,
			l.ID, l.TaskID, l.Digest, l.Kind, l.Name, l.Position,
			l.Size, l.Downloaded, l.Status, l.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListTaskLayers returns the task's blobs in manifest order. An unknown
// task yields an empty slice, not ErrNotFound: "this task has no layers
// yet" and "this task does not exist" are the same to every caller.
func (d *DBStore) ListTaskLayers(ctx context.Context, taskID string) ([]TaskLayer, error) {
	rows, err := d.db.QueryContext(ctx,
		d.rebind(`SELECT `+taskLayerColumns+` FROM task_layers WHERE task_id = ? ORDER BY position ASC, id ASC`),
		taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskLayer
	for rows.Next() {
		l, err := scanTaskLayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// UpdateTaskLayer rewrites one blob's row in place — the hot path: the
// puller's Sink reports byte counts and status transitions per digest.
func (d *DBStore) UpdateTaskLayer(ctx context.Context, l *TaskLayer) error {
	res, err := d.db.ExecContext(ctx, d.rebind(updateTaskLayerSQL),
		l.Digest, l.Kind, l.Name, l.Position, l.Size, l.Downloaded, l.Status, l.Error, l.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
