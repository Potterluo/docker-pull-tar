package store

import (
	"context"
	"errors"
	"testing"
)

// settings_ext_test.go covers the key/value preferences: a missing key is
// ErrNotFound (the caller applies its default), SetSetting upserts on both
// dialects, and ListSettings is key-ordered.

func TestSettingUpsertAndOrdering(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	if _, err := st.GetSetting(ctx, "workers"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSetting on a missing key: got %v, want ErrNotFound", err)
	}

	// First write is the INSERT path, second the UPDATE path.
	if err := st.SetSetting(ctx, "workers", "4"); err != nil {
		t.Fatalf("SetSetting insert: %v", err)
	}
	if v, err := st.GetSetting(ctx, "workers"); err != nil || v != "4" {
		t.Fatalf("GetSetting = %q, %v; want \"4\"", v, err)
	}
	if err := st.SetSetting(ctx, "workers", "8"); err != nil {
		t.Fatalf("SetSetting upsert: %v", err)
	}
	if v, err := st.GetSetting(ctx, "workers"); err != nil || v != "8" {
		t.Fatalf("second SetSetting did not win: %q, %v", v, err)
	}

	// An empty string is a value, not a missing row.
	if err := st.SetSetting(ctx, "proxy_url", ""); err != nil {
		t.Fatalf("SetSetting empty value: %v", err)
	}
	if v, err := st.GetSetting(ctx, "proxy_url"); err != nil || v != "" {
		t.Fatalf("GetSetting(proxy_url) = %q, %v; want an empty value and no error", v, err)
	}

	for k, v := range map[string]string{"proxy_mode": "system", "alpha": "1", "zzz": "26"} {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("SetSetting(%s): %v", k, err)
		}
	}

	settings, err := st.ListSettings(ctx)
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	wantKeys := []string{"alpha", "proxy_mode", "proxy_url", "workers", "zzz"}
	if len(settings) != len(wantKeys) {
		t.Fatalf("ListSettings: got %d rows (%v), want %d — the upsert appended a row?",
			len(settings), settingKeys(settings), len(wantKeys))
	}
	for i, key := range wantKeys {
		if settings[i].Key != key {
			t.Fatalf("ListSettings order: [%d] = %s, want %s", i, settings[i].Key, key)
		}
		if settings[i].UpdatedAt.IsZero() {
			t.Errorf("setting %s: UpdatedAt is zero", key)
		}
	}
	// The upserted key kept its single row and its latest value.
	if settings[3].Value != "8" {
		t.Errorf("workers = %q, want \"8\"", settings[3].Value)
	}
}

func settingKeys(settings []Setting) []string {
	out := make([]string, 0, len(settings))
	for _, s := range settings {
		out = append(out, s.Key)
	}
	return out
}
