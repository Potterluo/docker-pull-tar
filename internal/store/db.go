package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// DBStore implements Store on top of database/sql for both dialects.
type DBStore struct {
	db      *sql.DB
	dialect string // "sqlite" | "postgres"
}

// NewDBStore opens the database. The DSN is already final at this point
// (factory.go assembles the tuned sqlite DSN).
func NewDBStore(dialect, dsn string) (*DBStore, error) {
	driver := dialect
	if dialect == "postgres" {
		driver = "postgres"
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite is happiest with a single write connection; WAL
	// already lets readers proceed concurrently.
	if dialect == "sqlite" {
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &DBStore{db: db, dialect: dialect}, nil
}

func (d *DBStore) Close() error { return d.db.Close() }

// Dialect reports the active dialect ("sqlite" | "postgres") for the
// rare query that must differ per backend.
func (d *DBStore) Dialect() string { return d.dialect }

// ph returns the placeholder for the nth argument: `?` for sqlite, `$n`
// for postgres. Older files in this package write queries with ph(n);
// newer (generated) files write plain `?` and wrap the query with
// rebind() — both produce identical SQL per dialect.
func (d *DBStore) ph(n int) string {
	if d.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// rebind rewrites every `?` in query to the postgres `$n` form (no-op on
// sqlite). Use it for queries written as plain string literals.
func (d *DBStore) rebind(query string) string {
	if d.dialect != "postgres" {
		return query
	}
	n := 0
	return placeholderRe.ReplaceAllStringFunc(query, func(string) string {
		n++
		return "$" + strconv.Itoa(n)
	})
}

var placeholderRe = regexp.MustCompile(`\?`)

// scanErr normalizes sql.ErrNoRows into the package-level ErrNotFound so
// callers never depend on driver internals.
func scanErr(err error) error {
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	return err
}

// uniqueViolation reports whether err is a UNIQUE constraint failure,
// matching both drivers by their message (sqlite: "UNIQUE constraint
// failed", postgres: "duplicate key value violates unique constraint").
// Good enough for the few columns that carry UNIQUE indexes; if you add
// many, switch to structured error codes per driver.
func uniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint")
}

// Migrate brings the schema up to date. Two layers, both idempotent:
//
//  1. migrationSQL() — CREATE TABLE IF NOT EXISTS for every table. Fresh
//     installs are fully covered by this layer.
//  2. Imperative migrate* steps for schema EVOLUTION on installs that
//     already have older tables (add column / backfill / rebuild). Each
//     step checks tableHasColumn (or tableExists) first, so it is safe
//     to run on every boot.
//
// This mirrors how a real product grows: new tables go in layer 1, and
// every change to an existing table becomes one small guarded step in
// layer 2. No migration framework, no version table to babysit.
func (d *DBStore) Migrate(ctx context.Context) error {
	for _, stmt := range d.migrationSQL() {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w (stmt: %.80s...)", err, stmt)
		}
	}

	// Layer 2 — schema evolution on existing installs. Each step is a
	// no-op on fresh databases (the CREATE TABLE above already has the
	// column) and a guarded one-time ALTER otherwise. Add a migrate* call
	// here when an EXISTING table gains a column; tableHasColumn is the
	// guard (see its doc comment for the recipe).
	return nil
}

// migrationSQL returns the full schema as idempotent DDL. Timestamps are
// stored as TIMESTAMP columns in UTC everywhere; go's time.Time round-
// trips through both drivers. Keep table definitions alphabetically
// grouped by domain with a comment saying what owns them.
func (d *DBStore) migrationSQL() []string {
	return []string{
		// --- Downloads: tasks (F2/F3/F4 — one row per pull job) ---
		// Single-user tables: no user_id column, so nothing here is in
		// the DeleteUser cascade (users.go). status values:
		// pending|running|paused|succeeded|failed|canceled.
		// insecure/verify_tls are INTEGER 0/1 (store.boolToInt).
		`CREATE TABLE IF NOT EXISTS tasks (
			id               TEXT PRIMARY KEY,
			ref              TEXT NOT NULL,
			registry         TEXT NOT NULL DEFAULT '',
			repository       TEXT NOT NULL DEFAULT '',
			tag              TEXT NOT NULL DEFAULT '',
			digest           TEXT NOT NULL DEFAULT '',
			platform         TEXT NOT NULL DEFAULT '',
			status           TEXT NOT NULL DEFAULT 'pending',
			error            TEXT NOT NULL DEFAULT '',
			total_bytes      INTEGER NOT NULL DEFAULT 0,
			downloaded_bytes INTEGER NOT NULL DEFAULT 0,
			speed            REAL NOT NULL DEFAULT 0,
			work_dir         TEXT NOT NULL DEFAULT '',
			tar_path         TEXT NOT NULL DEFAULT '',
			tar_size         INTEGER NOT NULL DEFAULT 0,
			workers          INTEGER NOT NULL DEFAULT 4,
			insecure         INTEGER NOT NULL DEFAULT 0,
			verify_tls       INTEGER NOT NULL DEFAULT 1,
			created_at       TIMESTAMP,
			started_at       TIMESTAMP,
			updated_at       TIMESTAMP,
			finished_at      TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status     ON tasks (status)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks (created_at)`,
		// --- Downloads: task_layers (one blob per row, per-layer progress) ---
		// kind values: "layer" | "config"; position is the manifest order.
		`CREATE TABLE IF NOT EXISTS task_layers (
			id         TEXT PRIMARY KEY,
			task_id    TEXT NOT NULL,
			digest     TEXT NOT NULL,
			kind       TEXT NOT NULL DEFAULT 'layer',
			name       TEXT NOT NULL DEFAULT '',
			position   INTEGER NOT NULL DEFAULT 0,
			size       INTEGER NOT NULL DEFAULT 0,
			downloaded INTEGER NOT NULL DEFAULT 0,
			status     TEXT NOT NULL DEFAULT 'pending',
			error      TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_task_layers_task_id ON task_layers (task_id)`,
		// --- Sources: search backends + registry mirrors (/settings) ---
		// kind values: "search" (uses url) | "mirror" (uses host). A name
		// is unique per kind so re-seeding built-ins cannot duplicate.
		`CREATE TABLE IF NOT EXISTS sources (
			id         TEXT PRIMARY KEY,
			kind       TEXT NOT NULL,
			name       TEXT NOT NULL,
			url        TEXT NOT NULL DEFAULT '',
			host       TEXT NOT NULL DEFAULT '',
			enabled    INTEGER NOT NULL DEFAULT 1,
			priority   INTEGER NOT NULL DEFAULT 0,
			is_default INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP,
			UNIQUE (kind, name)
		)`,
		// --- Settings: key/value app preferences (defaults live in code) ---
		`CREATE TABLE IF NOT EXISTS settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP
		)`,
		// --- Artifacts: finished .tar files (bytes on disk, row is the index) ---
		`CREATE TABLE IF NOT EXISTS artifacts (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			path       TEXT NOT NULL UNIQUE,
			repository TEXT NOT NULL DEFAULT '',
			tag        TEXT NOT NULL DEFAULT '',
			platform   TEXT NOT NULL DEFAULT '',
			size       INTEGER NOT NULL DEFAULT 0,
			task_id    TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP
		)`,
		// --- Credentials: saved registry logins (/settings, one per host) ---
		// `secret` holds CIPHERTEXT produced by internal/secrets (DPAPI on
		// Windows, AES-256-GCM with a key file elsewhere) — never a plaintext
		// password, and this package neither seals nor opens it.
		// host is the row's identity and is UNIQUE, so a host cannot be saved
		// twice; it is the one column with no DEFAULT (defaulting an identity
		// to '' would make the second such row a duplicate instead of a
		// distinct login). kind values: "basic" | "token".
		// Every other column is NOT NULL with a default, the datetime columns
		// included: scanning a NULL into string/time.Time fails, and that is
		// the hazard the older tables' nullable timestamps still carry.
		`CREATE TABLE IF NOT EXISTS credentials (
			id         TEXT PRIMARY KEY,
			host       TEXT NOT NULL UNIQUE,
			username   TEXT NOT NULL DEFAULT '',
			secret     TEXT NOT NULL DEFAULT '',
			kind       TEXT NOT NULL DEFAULT 'basic',
			note       TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// --- gen:migrations ---
	}
}

// tableHasColumn reports whether table already has column, across both
// dialects. It is the guard every layer-2 migration step uses:
//
//	func (d *DBStore) migrateTasksAddFooColumn(ctx context.Context) error {
//		has, err := d.tableHasColumn(ctx, "tasks", "foo")
//		if err != nil { return err }
//		if has { return nil }
//		_, err = d.db.ExecContext(ctx, `ALTER TABLE tasks ADD COLUMN foo TEXT NOT NULL DEFAULT ''`)
//		return err
//	}
//
// then call it from Migrate(). Never destructive without a migration.
func (d *DBStore) tableHasColumn(ctx context.Context, table, column string) (bool, error) {
	var q string
	var args []any
	if d.dialect == "postgres" {
		q = `SELECT COUNT(*) FROM information_schema.columns
		     WHERE table_name = $1 AND column_name = $2`
		args = []any{table, column}
	} else {
		// pragma_table_info takes the table name as a string argument,
		// so it is quoted inline and only the column name is a param.
		q = `SELECT COUNT(*) FROM pragma_table_info(` + quoteIdent(table) + `) WHERE name = ?`
		args = []any{column}
	}
	var n int
	if err := d.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// quoteIdent quotes an identifier for sqlite's pragma functions, which
// take a table-name *string* rather than an identifier.
func quoteIdent(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	return string(append(out, '\''))
}
