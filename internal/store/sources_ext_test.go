package store

import (
	"context"
	"errors"
	"testing"
)

// sources_ext_test.go covers the configurable search sources / registry
// mirrors: kind filtering, the priority+name order the pickers rely on,
// the (kind, name) uniqueness that makes re-seeding idempotent, and the
// 0/1 conversion of its two flags.

func TestSourceCRUDAndOrdering(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	sources := []*Source{
		{ID: "src_mirror_slow", Kind: "mirror", Name: "zeta 加速源", Host: "zeta.example", Priority: 20, Enabled: true},
		{ID: "src_mirror_default", Kind: "mirror", Name: "Docker Hub 官方", Host: "registry-1.docker.io", Priority: 10, Enabled: true, IsDefault: true},
		{ID: "src_mirror_fast", Kind: "mirror", Name: "alpha 加速源", Host: "alpha.example", Priority: 20, Enabled: false},
		{ID: "src_search_hub", Kind: "search", Name: "Docker Hub 官方", URL: "https://hub.docker.com", Priority: 0, Enabled: true},
	}
	for _, s := range sources {
		if err := st.CreateSource(ctx, s); err != nil {
			t.Fatalf("CreateSource(%s): %v", s.ID, err)
		}
		if s.CreatedAt.IsZero() {
			t.Errorf("CreateSource(%s) left CreatedAt zero", s.ID)
		}
	}

	all, err := st.ListSources(ctx, "")
	if err != nil {
		t.Fatalf("ListSources(\"\"): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("ListSources(\"\"): got %d rows, want 4", len(all))
	}

	mirrors, err := st.ListSources(ctx, "mirror")
	if err != nil {
		t.Fatalf("ListSources(mirror): %v", err)
	}
	// priority ASC, name ASC: the priority-10 default first, then the two
	// priority-20 mirrors alphabetically ("alpha" before "zeta").
	wantMirrors := []string{"src_mirror_default", "src_mirror_fast", "src_mirror_slow"}
	if len(mirrors) != len(wantMirrors) {
		t.Fatalf("ListSources(mirror): got %d rows, want %d", len(mirrors), len(wantMirrors))
	}
	for i, id := range wantMirrors {
		if mirrors[i].ID != id {
			t.Fatalf("mirror order: [%d] = %s, want %s", i, mirrors[i].ID, id)
		}
		if mirrors[i].Kind != "mirror" {
			t.Errorf("kind filter leaked %q", mirrors[i].Kind)
		}
	}
	searches, err := st.ListSources(ctx, "search")
	if err != nil {
		t.Fatalf("ListSources(search): %v", err)
	}
	if len(searches) != 1 || searches[0].ID != "src_search_hub" {
		t.Fatalf("ListSources(search) = %d rows, want just src_search_hub", len(searches))
	}

	// Flags and the kind-specific columns round-trip.
	fast, err := st.GetSource(ctx, "src_mirror_fast")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if fast.Enabled {
		t.Errorf("Enabled round-trip: got true, want false")
	}
	if fast.Host != "alpha.example" || fast.Priority != 20 {
		t.Errorf("mirror columns did not round-trip: %+v", fast)
	}
	def, err := st.GetSource(ctx, "src_mirror_default")
	if err != nil {
		t.Fatalf("GetSource(default): %v", err)
	}
	if !def.Enabled || !def.IsDefault {
		t.Errorf("default flags round-trip: enabled=%v isDefault=%v", def.Enabled, def.IsDefault)
	}
	if def.CreatedAt.IsZero() {
		t.Errorf("CreatedAt did not round-trip")
	}
	if _, err := st.GetSource(ctx, "src_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSource on a missing id: got %v, want ErrNotFound", err)
	}

	// (kind, name) is unique — this is what makes seeding idempotent.
	dup := &Source{ID: "src_dup", Kind: "mirror", Name: "Docker Hub 官方"}
	if err := st.CreateSource(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate (kind,name): got %v, want ErrDuplicate", err)
	}
	// ... but the same name under the other kind is a different source.
	if err := st.CreateSource(ctx, &Source{ID: "src_same_name", Kind: "search", Name: "南大镜像源"}); err != nil {
		t.Fatalf("same name, other kind: %v", err)
	}

	// Update rewrites the row and keeps CreatedAt.
	created := def.CreatedAt
	def.Priority = 5
	def.Enabled = false
	def.URL = "https://mirror.example"
	if err := st.UpdateSource(ctx, def); err != nil {
		t.Fatalf("UpdateSource: %v", err)
	}
	again, err := st.GetSource(ctx, "src_mirror_default")
	if err != nil {
		t.Fatalf("GetSource after update: %v", err)
	}
	if again.Priority != 5 || again.Enabled || again.URL != "https://mirror.example" {
		t.Errorf("UpdateSource did not persist: %+v", again)
	}
	if !again.CreatedAt.Equal(created) {
		t.Errorf("UpdateSource rewrote CreatedAt: %s -> %s", created, again.CreatedAt)
	}
	// Renaming onto another row's (kind, name) is a duplicate, and the
	// write must be rejected, not silently applied.
	slow, err := st.GetSource(ctx, "src_mirror_slow")
	if err != nil {
		t.Fatalf("GetSource(slow): %v", err)
	}
	slow.Name = "Docker Hub 官方"
	if err := st.UpdateSource(ctx, slow); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("UpdateSource onto a duplicate name: got %v, want ErrDuplicate", err)
	}
	if err := st.UpdateSource(ctx, &Source{ID: "src_missing", Kind: "mirror", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateSource on a missing id: got %v, want ErrNotFound", err)
	}

	if err := st.DeleteSource(ctx, "src_mirror_slow"); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if _, err := st.GetSource(ctx, "src_mirror_slow"); !errors.Is(err, ErrNotFound) {
		t.Errorf("source survived DeleteSource: %v", err)
	}
	if err := st.DeleteSource(ctx, "src_mirror_slow"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteSource: got %v, want ErrNotFound", err)
	}
	if mirrors, err = st.ListSources(ctx, "mirror"); err != nil || len(mirrors) != 2 {
		t.Fatalf("ListSources(mirror) after delete: %d rows, %v; want 2", len(mirrors), err)
	}
}
