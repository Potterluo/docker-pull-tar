package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// credentials.go implements the CredentialStore slice: the registry logins
// the user saved, so a pull no longer needs -u/-p on the command line.
//
// SECRETS ARE CIPHERTEXT HERE. The `secret` column holds text produced by
// internal/secrets (DPAPI on Windows, AES-256-GCM elsewhere); this file never
// seals, opens, logs or otherwise touches a plaintext password. The sealing
// happens in the layer that owns the Protector, on the way in and on the way
// out — which is also why nothing in this file can leak a credential into an
// error message or a log line.
//
// host is UNIQUE and is the lookup key: one saved login per registry host.
// It must arrive already normalised (lower-case, no scheme, no path — the
// form internal/registry resolves a reference to, e.g. "ghcr.io"). This
// package does NOT normalise it: silently rewriting what the caller stored
// would make FindCredentialByHost("GHCR.IO") a mystery miss, and the host
// format is the registry package's business, not persistence's.
//
// Credentials are single-user: no user_id column, so nothing here cascades
// from a user delete.
//
// Queries are written with plain `?` placeholders and wrapped in d.rebind
// (db.go): one statement per query, identical SQL on sqlite and postgres.

// Credential is a stored registry login. Secret holds CIPHERTEXT (see
// internal/secrets); this package never sees or produces plaintext.
type Credential struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`     // normalised, lower-case, no scheme: "ghcr.io"
	Username  string    `json:"username"` // may be empty for a token-only entry
	Secret    string    `json:"secret"`   // opaque sealed text; never a plaintext password
	Kind      string    `json:"kind"`     // "basic" | "token"
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CredentialStore is the saved-registry-login half of Store. Embedded into
// Store at the gen:store-interfaces marker.
type CredentialStore interface {
	CreateCredential(ctx context.Context, c *Credential) error
	GetCredential(ctx context.Context, id string) (*Credential, error)
	ListCredentials(ctx context.Context) ([]Credential, error)
	UpdateCredential(ctx context.Context, c *Credential) error
	DeleteCredential(ctx context.Context, id string) error
	// FindCredentialByHost returns the entry for an exact normalised host.
	// Missing row => store.ErrNotFound.
	FindCredentialByHost(ctx context.Context, host string) (*Credential, error)
}

const credentialColumns = `id, host, username, secret, kind, note, created_at, updated_at`

const insertCredentialSQL = `INSERT INTO credentials (` + credentialColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// updateCredentialSQL does not touch created_at (the row's birthday) and
// refreshes updated_at from the method, not from the record, so a caller
// cannot forget to stamp an edit.
const updateCredentialSQL = `UPDATE credentials SET host = ?, username = ?, secret = ?,
	kind = ?, note = ?, updated_at = ? WHERE id = ?`

func scanCredential(scanner interface{ Scan(dest ...any) error }) (*Credential, error) {
	var c Credential
	if err := scanner.Scan(&c.ID, &c.Host, &c.Username, &c.Secret,
		&c.Kind, &c.Note, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// newCredentialID mints "<prefix><24 hex chars>" identifiers, the same shape
// internal/server's randomID uses for the other entities (see
// handlers_sources.go's newSourceID). Callers treat ids as opaque, so the
// only thing that matters is that they do not collide and do not need a
// round trip to the database to allocate.
func newCredentialID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to a timestamp so
		// a save cannot panic on an exotic platform.
		return fmt.Sprintf("cred_%d", time.Now().UnixNano())
	}
	return "cred_" + hex.EncodeToString(buf[:])
}

// CreateCredential inserts a login. ID is minted when the caller leaves it
// empty, and CreatedAt/UpdatedAt are filled when zero, so a caller can save a
// credential and immediately render it. A second row for the same host is
// ErrDuplicate — the caller should update the existing entry instead (or
// treat the host as already saved).
func (d *DBStore) CreateCredential(ctx context.Context, c *Credential) error {
	if c.ID == "" {
		c.ID = newCredentialID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	_, err := d.db.ExecContext(ctx, d.rebind(insertCredentialSQL),
		c.ID, c.Host, c.Username, c.Secret, c.Kind, c.Note, c.CreatedAt, c.UpdatedAt)
	if err != nil && uniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (d *DBStore) GetCredential(ctx context.Context, id string) (*Credential, error) {
	row := d.db.QueryRowContext(ctx,
		d.rebind(`SELECT `+credentialColumns+` FROM credentials WHERE id = ?`), id)
	c, err := scanCredential(row)
	return c, scanErr(err)
}

// ListCredentials returns newest first — the /settings order, and the order a
// "recently added" hint would read.
func (d *DBStore) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// FindCredentialByHost resolves the saved login for one registry host, which
// is what a pull does before it talks to a registry. host is matched exactly
// (see the file comment: normalisation is the caller's job). A host with no
// saved login is ErrNotFound, so the caller can fall back to anonymous access
// instead of treating "no credentials" as "empty credentials".
func (d *DBStore) FindCredentialByHost(ctx context.Context, host string) (*Credential, error) {
	row := d.db.QueryRowContext(ctx,
		d.rebind(`SELECT `+credentialColumns+` FROM credentials WHERE host = ?`), host)
	c, err := scanCredential(row)
	return c, scanErr(err)
}

// UpdateCredential rewrites the editable columns from the record
// (read-modify-write: GetCredential, mutate, UpdateCredential). UpdatedAt is
// always stamped here, so "when did this credential last change" cannot drift
// from what the caller passed in; CreatedAt is never touched. A missing id is
// ErrNotFound, and moving a row onto another row's host is ErrDuplicate.
func (d *DBStore) UpdateCredential(ctx context.Context, c *Credential) error {
	c.UpdatedAt = time.Now().UTC()
	res, err := d.db.ExecContext(ctx, d.rebind(updateCredentialSQL),
		c.Host, c.Username, c.Secret, c.Kind, c.Note, c.UpdatedAt, c.ID)
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

func (d *DBStore) DeleteCredential(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, d.rebind(`DELETE FROM credentials WHERE id = ?`), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
