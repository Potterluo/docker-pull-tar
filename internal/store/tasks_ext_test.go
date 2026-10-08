package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// tasks_ext_test.go covers the task/task-layer slice against a real
// SQLite database (NewTestStore), so the SQL, the `?` + d.rebind path,
// the 0/1 boolean conversion and the time.Time round-trip are all
// exercised for real rather than through a fake.

func TestTaskCRUD(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	first := &Task{
		ID: "task_1", Ref: "registry-1.docker.io/library/nginx:1.26",
		Registry: "registry-1.docker.io", Repository: "library/nginx", Tag: "1.26",
		Platform: "linux/amd64", Status: "pending", WorkDir: "/data/work/task_1",
		Workers: 4, VerifyTLS: true, CreatedAt: base,
	}
	second := &Task{ID: "task_2", Ref: "docker.io/library/redis:7", Status: "pending",
		CreatedAt: base.Add(time.Minute)}
	third := &Task{ID: "task_3", Ref: "docker.io/library/busybox:latest", Status: "pending",
		Insecure: true, CreatedAt: base.Add(2 * time.Minute)}
	for _, tk := range []*Task{first, second, third} {
		if err := st.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask(%s): %v", tk.ID, err)
		}
	}
	// CreateTask fills what the caller left zero: UpdatedAt is never zero
	// (callers render it straight after creating a task), and an explicit
	// CreatedAt is preserved.
	if third.UpdatedAt.IsZero() {
		t.Errorf("CreateTask left UpdatedAt zero")
	}
	if !third.UpdatedAt.Equal(third.CreatedAt) {
		t.Errorf("CreatedAt/UpdatedAt = %s/%s, want equal", third.CreatedAt, third.UpdatedAt)
	}

	got, err := st.GetTask(ctx, "task_1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Ref != first.Ref || got.Registry != first.Registry || got.Repository != first.Repository ||
		got.Tag != first.Tag || got.Platform != first.Platform || got.WorkDir != first.WorkDir {
		t.Errorf("config columns did not round-trip: %+v", got)
	}
	if got.Workers != 4 || !got.VerifyTLS || got.Insecure {
		t.Errorf("workers/flags did not round-trip: workers=%d verifyTLS=%v insecure=%v",
			got.Workers, got.VerifyTLS, got.Insecure)
	}
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt round-trip: got %s, want %s", got.CreatedAt, base)
	}

	if _, err := st.GetTask(ctx, "task_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTask on a missing id: got %v, want ErrNotFound", err)
	}

	// Newest first, and limit cuts from the front (the oldest end).
	list, err := st.ListTasks(ctx, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	want := []string{"task_3", "task_2", "task_1"}
	if len(list) != len(want) {
		t.Fatalf("ListTasks: got %d rows, want %d", len(list), len(want))
	}
	for i, id := range want {
		if list[i].ID != id {
			t.Fatalf("ListTasks order: [%d] = %s, want %s", i, list[i].ID, id)
		}
	}
	limited, err := st.ListTasks(ctx, 2)
	if err != nil {
		t.Fatalf("ListTasks(2): %v", err)
	}
	if len(limited) != 2 || limited[0].ID != "task_3" || limited[1].ID != "task_2" {
		t.Fatalf("ListTasks(2) = %v, want [task_3 task_2]", taskIDs(limited))
	}

	// Update: the puller's progress writes land here.
	got.Status = "running"
	got.TotalBytes = 1000
	got.DownloadedBytes = 400
	got.Speed = 1234.5
	got.StartedAt = base.Add(3 * time.Minute)
	if err := st.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	again, err := st.GetTask(ctx, "task_1")
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if again.Status != "running" || again.TotalBytes != 1000 || again.DownloadedBytes != 400 || again.Speed != 1234.5 {
		t.Errorf("progress columns did not persist: %+v", again)
	}
	if !again.StartedAt.Equal(got.StartedAt) {
		t.Errorf("StartedAt round-trip: got %s, want %s", again.StartedAt, got.StartedAt)
	}
	if !again.CreatedAt.Equal(base) {
		t.Errorf("UpdateTask rewrote CreatedAt: got %s, want %s", again.CreatedAt, base)
	}
	if !again.UpdatedAt.After(base) {
		t.Errorf("UpdateTask did not refresh UpdatedAt: %s", again.UpdatedAt)
	}

	if err := st.UpdateTask(ctx, &Task{ID: "task_missing", Ref: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateTask on a missing id: got %v, want ErrNotFound", err)
	}

	if err := st.DeleteTask(ctx, "task_1"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := st.GetTask(ctx, "task_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("task survived DeleteTask: %v", err)
	}
	if err := st.DeleteTask(ctx, "task_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteTask: got %v, want ErrNotFound", err)
	}
	if list, err = st.ListTasks(ctx, 0); err != nil || len(list) != 2 {
		t.Fatalf("ListTasks after delete = %v, %v; want 2 rows", taskIDs(list), err)
	}
}

func TestReplaceTaskLayers(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	task := &Task{ID: "task_layers", Ref: "docker.io/library/nginx:latest"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Deliberately out of slice order: the round-trip must come back in
	// Position order, not insertion order.
	layers := []TaskLayer{
		{ID: "lay_2", TaskID: task.ID, Digest: "sha256:bbbb", Kind: "config",
			Name: "bbbb", Position: 2, Size: 2048, Status: "pending"},
		{ID: "lay_1", TaskID: task.ID, Digest: "sha256:aaaa", Kind: "layer",
			Name: "aaaa", Position: 1, Size: 1024, Status: "pending"},
	}
	if err := st.ReplaceTaskLayers(ctx, task.ID, layers); err != nil {
		t.Fatalf("ReplaceTaskLayers: %v", err)
	}
	got, err := st.ListTaskLayers(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListTaskLayers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTaskLayers: got %d rows, want 2", len(got))
	}
	if got[0].ID != "lay_1" || got[0].Position != 1 || got[1].ID != "lay_2" || got[1].Position != 2 {
		t.Fatalf("layers not returned in position order: %+v", got)
	}
	if got[0].Digest != "sha256:aaaa" || got[0].Kind != "layer" || got[0].Size != 1024 {
		t.Errorf("layer columns did not round-trip: %+v", got[0])
	}
	if got[1].Kind != "config" || got[1].Size != 2048 {
		t.Errorf("config blob did not round-trip: %+v", got[1])
	}

	// A second call REPLACES the set (a re-resolved manifest can add or
	// drop blobs) instead of appending, and fills in the TaskID and any
	// zero Position the caller left to the slice order.
	replacement := []TaskLayer{
		{ID: "lay_3", Digest: "sha256:cccc", Kind: "layer", Size: 4096, Status: "completed"},
		{ID: "lay_4", Digest: "sha256:dddd", Kind: "layer", Size: 8192, Status: "completed"},
	}
	if err := st.ReplaceTaskLayers(ctx, task.ID, replacement); err != nil {
		t.Fatalf("second ReplaceTaskLayers: %v", err)
	}
	got, err = st.ListTaskLayers(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListTaskLayers after replace: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ReplaceTaskLayers appended instead of replacing: got %d rows, want 2", len(got))
	}
	if got[0].ID != "lay_3" || got[1].ID != "lay_4" {
		t.Fatalf("replacement layers out of order: %+v", got)
	}
	if got[0].Position != 0 || got[1].Position != 1 {
		t.Errorf("zero positions were not filled from slice order: %+v", got)
	}
	if got[0].TaskID != task.ID || got[1].TaskID != task.ID {
		t.Errorf("TaskID was not taken from the argument: %+v", got)
	}
	if replacement[0].Position != 0 || replacement[1].Position != 1 {
		t.Errorf("ReplaceTaskLayers did not write positions back: %+v", replacement)
	}

	// UpdateTaskLayer is the per-blob progress write.
	one := got[1]
	one.Downloaded = 8192
	one.Status = "downloading"
	if err := st.UpdateTaskLayer(ctx, &one); err != nil {
		t.Fatalf("UpdateTaskLayer: %v", err)
	}
	got, err = st.ListTaskLayers(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListTaskLayers after update: %v", err)
	}
	if got[1].ID != "lay_4" || got[1].Downloaded != 8192 || got[1].Status != "downloading" {
		t.Errorf("UpdateTaskLayer did not persist: %+v", got[1])
	}
	if got[0].Status != "completed" {
		t.Errorf("UpdateTaskLayer touched a sibling row: %+v", got[0])
	}
	if err := st.UpdateTaskLayer(ctx, &TaskLayer{ID: "lay_missing", TaskID: task.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateTaskLayer on a missing id: got %v, want ErrNotFound", err)
	}

	// Another task's layer set is untouched, and an unknown task is empty.
	other := &Task{ID: "task_other", Ref: "docker.io/library/redis:7"}
	if err := st.CreateTask(ctx, other); err != nil {
		t.Fatalf("CreateTask(other): %v", err)
	}
	if err := st.ReplaceTaskLayers(ctx, other.ID, []TaskLayer{
		{ID: "lay_other", Digest: "sha256:eeee", Status: "pending"},
	}); err != nil {
		t.Fatalf("ReplaceTaskLayers(other): %v", err)
	}
	if n := len(mustListLayers(t, st, task.ID)); n != 2 {
		t.Errorf("replacing another task's layers disturbed task %s: %d rows", task.ID, n)
	}
	if got := mustListLayers(t, st, "task_unknown"); len(got) != 0 {
		t.Errorf("unknown task returned %d layers, want 0", len(got))
	}

	// DeleteTask clears the layer rows with the task (no FKs, so the
	// cascade is explicit).
	if err := st.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if got := mustListLayers(t, st, task.ID); len(got) != 0 {
		t.Errorf("DeleteTask left %d orphan layer rows", len(got))
	}
	if got := mustListLayers(t, st, other.ID); len(got) != 1 {
		t.Errorf("DeleteTask removed another task's layers: %d rows", len(got))
	}
}

// TestMigrateIsIdempotent runs the whole schema — including the tables
// added for CONTRACT §4 — twice over, the way a second boot does.
func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st, err := New(&StorageConfig{Type: "sqlite"}, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dbs, ok := st.(*DBStore)
	if !ok {
		t.Fatalf("store is %T, want *DBStore", st)
	}
	for i := 1; i <= 2; i++ {
		if err := dbs.Migrate(ctx); err != nil {
			t.Fatalf("Migrate call %d: %v", i, err)
		}
	}
	// Every new table really exists after the double migrate.
	if err := st.SetSetting(ctx, "workers", "4"); err != nil {
		t.Fatalf("settings after Migrate: %v", err)
	}
	if err := st.CreateTask(ctx, &Task{ID: "task_migrate", Ref: "docker.io/library/nginx:latest"}); err != nil {
		t.Fatalf("tasks after Migrate: %v", err)
	}
	if err := st.ReplaceTaskLayers(ctx, "task_migrate", []TaskLayer{{ID: "lay_migrate", Digest: "sha256:ffff"}}); err != nil {
		t.Fatalf("task_layers after Migrate: %v", err)
	}
	if err := st.CreateSource(ctx, &Source{ID: "src_migrate", Kind: "mirror", Name: "migrate"}); err != nil {
		t.Fatalf("sources after Migrate: %v", err)
	}
	if err := st.CreateArtifact(ctx, &Artifact{ID: "art_migrate", Name: "a.tar", Path: "/tmp/a.tar"}); err != nil {
		t.Fatalf("artifacts after Migrate: %v", err)
	}
}

func taskIDs(tasks []Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}

func mustListLayers(t *testing.T, st Store, taskID string) []TaskLayer {
	t.Helper()
	layers, err := st.ListTaskLayers(context.Background(), taskID)
	if err != nil {
		t.Fatalf("ListTaskLayers(%s): %v", taskID, err)
	}
	return layers
}
