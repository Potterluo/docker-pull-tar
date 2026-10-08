package store

import "testing"

// helpers_test.go holds the shared test fixture for this package.
//
// It lives in a _test.go file on purpose: a non-test file would import
// "testing" into the shipped binary's dependency graph for no reason.
// Packages outside this one build their own store with store.New directly.

// NewTestStore returns a migrated SQLite store backed by a temporary
// directory. It is closed automatically when the test finishes.
func NewTestStore(t *testing.T) Store {
	t.Helper()
	st, err := New(&StorageConfig{Type: "sqlite", AutoMigrate: true}, t.TempDir())
	if err != nil {
		t.Fatalf("NewTestStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
