package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// artifacts_ext_test.go covers the finished-tar index: creation with the
// timestamp filled in, lookup by id and by path, the UNIQUE path that
// makes an output-directory rescan idempotent, and delete.

func TestArtifactCRUDAndPathUniqueness(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	dir := t.TempDir()

	// CreatedAt is filled when the caller leaves it zero (the puller
	// registers an artifact the moment the file is closed).
	first := &Artifact{
		ID: "art_1", Name: "nginx_1.26_amd64.tar", Path: filepath.Join(dir, "nginx_1.26_amd64.tar"),
		Repository: "library/nginx", Tag: "1.26", Platform: "linux/amd64", Size: 1024, TaskID: "task_1",
	}
	if err := st.CreateArtifact(ctx, first); err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if first.CreatedAt.IsZero() {
		t.Fatalf("CreateArtifact left CreatedAt zero")
	}
	// An explicit CreatedAt is honoured, which is what makes the listing
	// order below deterministic.
	second := &Artifact{
		ID: "art_2", Name: "redis_7_arm64.tar", Path: filepath.Join(dir, "redis_7_arm64.tar"),
		Repository: "library/redis", Tag: "7", Platform: "linux/arm64", Size: 2048, TaskID: "task_2",
		CreatedAt: first.CreatedAt.Add(time.Hour),
	}
	if err := st.CreateArtifact(ctx, second); err != nil {
		t.Fatalf("CreateArtifact(second): %v", err)
	}

	list, err := st.ListArtifacts(ctx)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListArtifacts: got %d rows, want 2", len(list))
	}
	for i, want := range []string{"art_2", "art_1"} {
		if list[i].ID != want {
			t.Fatalf("ListArtifacts order: [%d] = %s, want %s (newest first)", i, list[i].ID, want)
		}
	}

	got, err := st.GetArtifact(ctx, "art_1")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if got.Path != first.Path || got.Name != first.Name || got.Repository != "library/nginx" ||
		got.Tag != "1.26" || got.Platform != "linux/amd64" || got.Size != 1024 || got.TaskID != "task_1" {
		t.Errorf("artifact columns did not round-trip: %+v", got)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt round-trip: got %s, want %s", got.CreatedAt, first.CreatedAt)
	}
	if _, err := st.GetArtifact(ctx, "art_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetArtifact on a missing id: got %v, want ErrNotFound", err)
	}

	// The path is the artifact's filesystem identity: exactly one row per
	// path, which is how a rescan of output_dir stays idempotent.
	byPath := 0
	for _, a := range list {
		if a.Path == second.Path {
			byPath++
		}
	}
	if byPath != 1 {
		t.Fatalf("looking the artifact up by path found %d rows, want 1", byPath)
	}

	dup := &Artifact{ID: "art_dup", Name: "copy.tar", Path: second.Path}
	if err := st.CreateArtifact(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second artifact with the same path: got %v, want ErrDuplicate", err)
	}
	if list, err = st.ListArtifacts(ctx); err != nil || len(list) != 2 {
		t.Fatalf("ListArtifacts after duplicate attempt: %d rows, %v; want 2", len(list), err)
	}

	if err := st.DeleteArtifact(ctx, "art_1"); err != nil {
		t.Fatalf("DeleteArtifact: %v", err)
	}
	if _, err := st.GetArtifact(ctx, "art_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("artifact survived DeleteArtifact: %v", err)
	}
	if err := st.DeleteArtifact(ctx, "art_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteArtifact: got %v, want ErrNotFound", err)
	}
	// The deleted path is free again — the file was removed with the row.
	if err := st.CreateArtifact(ctx, &Artifact{ID: "art_3", Name: "nginx_1.26_amd64.tar", Path: first.Path}); err != nil {
		t.Fatalf("re-indexing a deleted path: %v", err)
	}
}
